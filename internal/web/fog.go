package web

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"rond-with-you/internal/fog"
	"rond-with-you/internal/geo"
)

const maxFogUploadBytes = 64 << 20 // .fwss 快照一般几 MB，放宽到 64MB

// 迷雾是站点级数据（与公开页读数据不区分归属的约定一致），
// 内存里只持一份索引，首次访问时从库里加载。
var (
	fogOnce   sync.Once
	fogIndexV *fog.Index
)

func (s *Server) fogIndex() (*fog.Index, error) {
	fogOnce.Do(func() {
		fogIndexV = fog.NewIndex()
		if err := fogIndexV.LoadAll(s.db); err != nil {
			fogIndexV = nil
		}
	})
	if fogIndexV == nil {
		return nil, fmt.Errorf("迷雾数据加载失败")
	}
	return fogIndexV, nil
}

// handleFogTile 渲染迷雾透明瓦片。迷雾网格是 WGS-84；当前激活底图若是
// GCJ-02（高德），前端会带 ?crs=gcj02，服务端把每个块做 WGS→GCJ 正偏移后再画。
func (s *Server) handleFogTile(w http.ResponseWriter, r *http.Request) {
	z, err1 := strconv.Atoi(r.PathValue("z"))
	x, err2 := strconv.Atoi(r.PathValue("x"))
	y, err3 := strconv.Atoi(r.PathValue("y"))
	if err1 != nil || err2 != nil || err3 != nil || z < 0 || z > 22 ||
		x < 0 || y < 0 || x >= 1<<uint(z) || y >= 1<<uint(z) {
		http.NotFound(w, r)
		return
	}
	ix, err := s.fogIndex()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	crs := r.URL.Query().Get("crs")
	if crs != "gcj02" && crs != "wgs84" {
		crs = s.cfg.MapCRS
	}
	// 遮罩只对访客生效：站长自己要能看到完整迷雾，否则没法核对遮哪里
	isAdmin := false
	if u, _ := s.sessionUser(r); u != nil {
		isAdmin = true
	}
	// 渲染结果缓存：一张瓦片要逐格画位图再做 PNG 编码，而内容只随
	// 「迷雾数据版本 + 遮罩 + 底图坐标系」变化。站长的瓦片不含遮罩，指纹用 "-"，
	// 这样改遮罩时不必连带丢掉站长的缓存。
	fp := "-"
	if !isAdmin {
		fp = s.maskFP()
	}
	cacheKey := strconv.Itoa(z) + "/" + strconv.Itoa(x) + "/" + strconv.Itoa(y) + "/" + crs + "/" + fp
	if t, ok := s.fogTiles.get(cacheKey); ok {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", fogTileCacheControl(isAdmin))
		w.Write(t.body)
		return
	}
	masks := s.maskSetFor(isAdmin)
	var img *image.NRGBA
	ix.View(func(blocks map[[2]int][]byte) {
		img = fog.RenderTile(blocks, z, x, y, crs, masks)
	})
	if img == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", fogTileCacheControl(isAdmin))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.fogTiles.put(cacheKey, cachedTile{body: buf.Bytes(), ct: "image/png", at: time.Now()})
	w.Write(buf.Bytes())
}

// fogTileCacheControl 决定迷雾瓦片的缓存策略。站长与访客的瓦片内容不同（一个含遮罩、
// 一个不含）而 URL 相同，所以站长的响应绝不能进共享缓存——否则 CDN / 反向代理会把
// 未遮罩的瓦片原样发给访客。访客那份内容对所有人一致，允许公开缓存。
func fogTileCacheControl(isAdmin bool) string {
	if isAdmin {
		return "private, no-store"
	}
	return "public, max-age=3600"
}

// adminFogUpload 导入 .fwss：mode=overlay 按位或合并进现有迷雾，
// mode=cover 清空后整体替换。
func (s *Server) adminFogUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	r.Body = http.MaxBytesReader(w, r.Body, maxFogUploadBytes+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		s.adminFlash(w, r, "err", "读取上传内容失败："+err.Error())
		return
	}
	file, hdr, err := r.FormFile("fwss")
	if err != nil {
		s.adminFlash(w, r, "err", "请选择要上传的 .fwss 迷雾快照")
		return
	}
	defer file.Close()
	name := strings.ToLower(filepath.Base(hdr.Filename))
	if !strings.HasSuffix(name, ".fwss") && !strings.HasSuffix(name, ".zip") {
		s.adminFlash(w, r, "err", "文件类型不正确，请上传 Fog of World 导出的 .fwss 快照")
		return
	}
	mode := r.PostFormValue("mode")
	if mode != "overlay" && mode != "cover" {
		mode = "overlay"
	}

	data, err := io.ReadAll(io.LimitReader(file, maxFogUploadBytes+1))
	if err != nil {
		s.adminFlash(w, r, "err", "读取文件失败："+err.Error())
		return
	}
	if int64(len(data)) > maxFogUploadBytes {
		s.adminFlash(w, r, "err", "迷雾快照超过 64MB，疑似文件有误")
		return
	}
	blocks, cells, err := fog.Decode(data)
	if err != nil {
		s.adminFlash(w, r, "err", "解析迷雾快照失败："+err.Error())
		return
	}
	// 快照里还有 Model/~ 与 Model/# 两层（约占一半体积）：站点不用它们渲染，
	// 但导出时要原样写回，否则重导出的快照在 fog of world 里是残缺的
	layers, err := fog.RawLayers(data)
	if err != nil {
		s.adminFlash(w, r, "err", "解析快照的其它图层失败："+err.Error())
		return
	}
	if len(blocks) == 0 {
		s.adminFlash(w, r, "err", "快照里没有找到迷雾瓦片数据")
		return
	}
	ix, err := s.fogIndex()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	applied, err := ix.Import(s.db, u.ID, blocks, mode == "overlay")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// 「整体替换」时图层也整体换掉，避免留下上一份快照里已经不存在的图层条目
	if err := ix.SaveRawLayers(s.db, u.ID, layers, mode == "cover"); err != nil {
		log.Printf("留存迷雾其它图层失败: %v", err)
	}
	b, c := ix.Stats()
	verb := "叠加合并"
	if mode == "cover" {
		verb = "整体替换"
	}
	s.adminFlash(w, r, "ok", fmt.Sprintf(
		"迷雾导入成功（%s）：本次写入 %d 块 / %d 格，当前共 %d 块 / %d 格", verb, applied, cells, b, c))
}

// ---------- 迷雾覆盖率 ----------

// FogCity 是单个城市（或省份）的迷雾点亮统计：地点里有多少个落在已探索格内。
type FogCity struct {
	Name  string `json:"name"`
	Lit   int    `json:"lit"`
	Total int    `json:"total"`
}

// FogCoverage 是统计页展示的迷雾覆盖率数据。
type FogCoverage struct {
	AreaKm2   float64   `json:"area_km2"`
	Blocks    int       `json:"blocks"`
	Cells     int       `json:"cells"`
	LitPlaces int       `json:"lit_places"`
	Places    int       `json:"places"`
	Cities    []FogCity `json:"cities"`
	Provinces []FogCity `json:"provinces"`
}

type fogCounter struct{ lit, total map[string]int }

func newFogCounter() *fogCounter {
	return &fogCounter{lit: map[string]int{}, total: map[string]int{}}
}

func (fc *fogCounter) add(name string, lit bool) {
	fc.total[name]++
	if lit {
		fc.lit[name]++
	}
}

func (fc *fogCounter) top() []FogCity {
	out := make([]FogCity, 0, len(fc.total))
	for name, total := range fc.total {
		out = append(out, FogCity{Name: name, Lit: fc.lit[name], Total: total})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Lit != out[j].Lit {
			return out[i].Lit > out[j].Lit
		}
		return out[i].Total > out[j].Total
	})
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// fogCoverage 计算当前数据集地点与迷雾位图的叠加：迷雾网格是 WGS-84，
// 库里的地点是 GCJ-02，先反算到 WGS 再查格位图。
// 没导入迷雾或没数据时返回 nil，模板据此隐藏整个区块。
// fogCoverage 统计迷雾覆盖率。regions/exclude 来自 filterOf（站长为空），
// 与其它公开统计保持一致——否则黑名单里的城市名照样会出现在「涉及城市」里。
func (s *Server) fogCoverage(ctx context.Context, datasetID int64, regions []string, exclude bool, admin bool) *FogCoverage {
	ix, err := s.fogIndex()
	if err != nil {
		return nil
	}
	blocks, cells := ix.Stats()
	if blocks == 0 {
		return nil
	}

	// 遮罩内的迷雾对访客不算数：地点要跳过，面积也要扣掉这部分格子
	masks := s.maskSetFor(admin)
	args := []any{datasetID}
	q := `SELECT COALESCE(p.city,''), COALESCE(p.province,''),
		COALESCE(p.lat,0), COALESCE(p.lon,0), p.lat IS NULL
		FROM places p WHERE p.dataset_id=$1` + regionSQL("p", regions, exclude, &args)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	cov := &FogCoverage{Blocks: blocks, Cells: cells}
	cities, provs := newFogCounter(), newFogCounter()
	for rows.Next() {
		var city, province string
		var lat, lon float64
		var nullCoord bool
		if err := rows.Scan(&city, &province, &lat, &lon, &nullCoord); err != nil {
			return nil
		}
		cov.Places++
		if nullCoord {
			continue
		}
		if masks.HitLatLon(lat, lon) {
			continue
		}
		wlat, wlon := geo.GCJ02ToWGS84(lat, lon)
		cx, cy := fog.CellOf(wlat, wlon)
		lit := ix.CellLit(cx, cy)
		if lit {
			cov.LitPlaces++
		}
		if city != "" {
			cities.add(city, lit)
		}
		if province != "" {
			provs.add(province, lit)
		}
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	cov.Cities = cities.top()
	cov.Provinces = provs.top()
	// 面积与格数都用索引里预计算好的总量，再减掉遮罩盖住的部分
	// （以前是每次现扫一遍全部位图、还顺手复制排序了三次整张块表）。
	cov.AreaKm2 = ix.AreaKm2()
	if mc, ma := ix.MaskedStats(masks); mc > 0 {
		cov.Cells -= mc
		cov.AreaKm2 -= ma / 1e6
	}
	return cov
}

// fogResetIndex 丢弃内存里的迷雾索引（清空迷雾后调用），
// 下次访问时按空库重建。
func fogResetIndex() {
	fogIndexV = fog.NewIndex()
}

// adminFogClear 清空全部迷雾数据。迷雾是站点级数据，一次清空影响所有展示。
func (s *Server) adminFogClear(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM fog_blocks`); err != nil {
		s.serverError(w, r, err)
		return
	}
	// 其它图层一并丢弃：留着的话导出的快照会有图层条目却没有对应的 Model/* 瓦片
	if err := fog.DropRawLayers(s.db); err != nil {
		log.Printf("清空迷雾其它图层失败: %v", err)
	}
	fogResetIndex()
	s.adminFlash(w, r, "ok", "迷雾已清空")
}

// adminFogExport 把当前迷雾还原成 .fwss 供迁移。
func (s *Server) adminFogExport(w http.ResponseWriter, r *http.Request) {
	ix, err := s.fogIndex()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	blocks := ix.All()
	if len(blocks) == 0 {
		s.adminFlash(w, r, "err", "还没有导入任何迷雾数据")
		return
	}
	layers, err := fog.LoadRawLayers(s.db, userFrom(r.Context()).ID)
	if err != nil {
		log.Printf("读取迷雾其它图层失败: %v", err)
	}
	out, err := fog.Encode(blocks, layers)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	_, cells := ix.Stats()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="fog-export-%s.fwss"`, time.Now().Format("20060102-150405")))
	w.Header().Set("X-Fog-Cells", strconv.Itoa(cells))
	w.Write(out)
}
