package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rond-with-you/internal/ingest"
	"rond-with-you/internal/rond"
	"rond-with-you/internal/stats"
)

// 这两个导出面向站长自己的数据迁移，因此不做「家 / 工作」过滤——
// 能进后台的人本来就拥有全部数据的查看权。

func (s *Server) exportPlaces(w http.ResponseWriter, r *http.Request) []stats.Place {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		s.adminFlash(w, r, "err", "还没有可导出的数据集")
		return nil
	}
	// 直接按 dataset 全量取，绕过筛选与隐私开关
	rows, err := s.db.QueryContext(ctx, `SELECT `+stats.PlaceCols+`
		FROM places p WHERE p.dataset_id=$1 ORDER BY p.id`, ds.ID)
	if err != nil {
		s.serverError(w, r, err)
		return nil
	}
	defer rows.Close()
	var out []stats.Place
	for rows.Next() {
		var p stats.Place
		if err := stats.ScanPlace(rows, &p); err != nil {
			s.serverError(w, r, err)
			return nil
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		s.serverError(w, r, err)
		return nil
	}
	return out
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func (s *Server) exportGeoJSON(w http.ResponseWriter, r *http.Request) {
	places := s.exportPlaces(w, r)
	if places == nil {
		return
	}
	var b strings.Builder
	b.WriteString(`{"type":"FeatureCollection","features":[`)
	for i, p := range places {
		if i > 0 {
			b.WriteByte(',')
		}
		var first, last string
		if p.FirstAt != nil {
			first = p.FirstAt.UTC().Format("2006-01-02")
		}
		if p.LastAt != nil {
			last = p.LastAt.UTC().Format("2006-01-02")
		}
		// GeoJSON 坐标是 [lon, lat]；库里的坐标本身是 GCJ-02，原样导出并在说明里注明
		fmt.Fprintf(&b, `{"type":"Feature","geometry":{"type":"Point","coordinates":[%v,%v]},`+
			`"properties":{"name":%q,"city":%q,"province":%q,"category":%q,`+
			`"visits":%d,"dwell_min":%d,"first":%q,"last":%q,"crs":"gcj02"}}`,
			p.Lon, p.Lat, p.Name, p.City, p.Province, p.POICategory,
			p.VisitCount, p.DwellMinutes, first, last)
	}
	b.WriteString(`]}`)
	w.Header().Set("Content-Type", "application/geo+json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="places.geojson"`)
	w.Write([]byte(b.String()))
}

func (s *Server) exportGPX(w http.ResponseWriter, r *http.Request) {
	places := s.exportPlaces(w, r)
	if places == nil {
		return
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<gpx version="1.1" creator="rond-with-you" xmlns="http://www.topografix.com/GPX/1/1">` + "\n")
	for _, p := range places {
		var parts []string
		if p.City != "" {
			parts = append(parts, p.Province, p.City)
		}
		if p.POICategory != "" {
			parts = append(parts, poiName(p.POICategory))
		}
		desc := strings.Join(strings.Fields(strings.Join(parts, " · ")), " ")
		var last string
		if p.LastAt != nil {
			last = p.LastAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		fmt.Fprintf(&b, "<wpt lat=\"%v\" lon=\"%v\"><name>%s</name>", p.Lat, p.Lon, xmlEscape(p.Name))
		if desc != "" {
			fmt.Fprintf(&b, "<desc>%s</desc>", xmlEscape(desc))
		}
		if last != "" {
			fmt.Fprintf(&b, "<time>%s</time>", last)
		}
		b.WriteString("</wpt>\n")
	}
	b.WriteString("</gpx>\n")
	w.Header().Set("Content-Type", "application/gpx+xml; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="places-%s.gpx"`, time.Now().Format("20060102")))
	w.Write([]byte(b.String()))
}

// baselinePath 返回某数据集的基线库路径。基线是导入时从原始备份包留下的单文件副本，
// 导出时以它为底做覆盖，从而保住 Core Data 的元数据表、索引、持久化历史，
// 以及我们并不落库的实体（原始 GPS 采样点 ZRAWVISIT、ZKEYWORD、ZTRIP* 等）。
func (s *Server) baselinePath(datasetID int64) string {
	return filepath.Join(s.cfg.DataDir, "baselines", fmt.Sprintf("%d.sqlite", datasetID))
}

// ensureBaseline 确保基线库存在；缺失时从当初上传的原始备份包重建。
func (s *Server) ensureBaseline(ctx context.Context, datasetID int64) (string, error) {
	p := s.baselinePath(datasetID)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	archive, err := s.findSourceArchive(ctx, datasetID)
	if err != nil {
		return "", err
	}
	if err := rond.WriteBaseline(archive, p); err != nil {
		return "", fmt.Errorf("重建基线库失败：%w", err)
	}
	return p, nil
}

// findSourceArchive 在 uploads 目录里按 datasets.sha256 找回当初上传的备份包。
func (s *Server) findSourceArchive(ctx context.Context, datasetID int64) (string, error) {
	var sha string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(sha256,'') FROM datasets WHERE id=$1`, datasetID).Scan(&sha); err != nil {
		return "", err
	}
	files, _ := filepath.Glob(filepath.Join(s.cfg.UploadDir, "*"))
	// 新的在前，命中即止
	sort.Slice(files, func(i, j int) bool {
		fi, ei := os.Stat(files[i])
		fj, ej := os.Stat(files[j])
		if ei != nil || ej != nil {
			return false
		}
		return fi.ModTime().After(fj.ModTime())
	})
	for _, f := range files {
		if fi, err := os.Stat(f); err != nil || fi.IsDir() {
			continue
		}
		got, _, err := ingest.File(f)
		if err != nil {
			continue
		}
		if sha == "" || got == sha {
			return f, nil
		}
	}
	return "", fmt.Errorf("找不到当初上传的原始备份包（可能已清理），请到后台重新上传一次该 .rondbackup")
}

// exportRondbackup 以基线库为底，把当前数据集写回为 rond 原始格式的 .rondbackup 并下载。
// 坐标是 GCJ-02，与 rond 同系，原样写回；新增/编辑的记录随数据集一并导出。
func (s *Server) exportRondbackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		s.adminFlash(w, r, "err", "还没有可导出的数据集")
		return
	}
	base, err := s.ensureBaseline(ctx, ds.ID)
	if err != nil {
		s.adminFlash(w, r, "err", "导出 rondbackup 失败："+err.Error())
		return
	}
	// 先构建到缓冲，成功后再写响应头，避免半截输出破坏下载文件
	var buf bytes.Buffer
	if err := rond.ExportRondbackup(ctx, s.db, ds.ID, base, &buf); err != nil {
		s.adminFlash(w, r, "err", "导出 rondbackup 失败："+err.Error())
		return
	}
	stem := strings.TrimSuffix(filepath.Base(ds.Name), filepath.Ext(ds.Name))
	if stem == "" {
		stem = "LifeEasy"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-export-%s.rondbackup"`, stem, time.Now().Format("20060102")))
	w.Write(buf.Bytes())
}
