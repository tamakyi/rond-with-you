package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rond-with-you/internal/setting"
)

// 地点图片的几条硬约束。字节由浏览器转好 WebP 才发过来，服务端只做形状校验，
// 不重新编码（1 核机器扛不住解码大图）。
const (
	imageMaxBytes  = 6 << 20 // 单张上限 6MB：长边 1440px、按 220KB 目标降质的 WebP 通常不到 300KB
	imageMaxPer    = 12      // 单个地点最多留几张，防止误拖一堆进来
	imageUploadMem = 8 << 20 // multipart 解析用的内存上限
)

// PlaceImage 是模板里用的地点图片。
type PlaceImage struct {
	ID     int64
	URL    string
	Width  int
	Height int
	Bytes  int64
}

// savePlaceImage 把一段已经转好的 WebP 字节存到当前后端，落一行记录，并返回可用的地址。
// 上传接口与照片导入共用这一条路径，省得两处各写一遍 key 规则。
// url 为空表示「存是存进去了，但没配 s3_public_base，浏览器取不到」——调用方要据此提示。
func (s *Server) savePlaceImage(ctx context.Context, datasetID int64, srcPK int, data []byte, w, h int) (int64, string, error) {
	if !isWebP(data) {
		return 0, "", fmt.Errorf("不是 WebP 数据")
	}
	backend, err := s.imageBackend(ctx)
	if err != nil {
		return 0, "", err
	}
	sum := sha256.Sum256(data)
	// 对象名 = <数据集>/<上传月份>/<地点号>-<内容哈希>.webp
	//   按月份分目录：一次出行传几十张，按月归堆最好找、也最好整月备份/迁移；
	//   若按地点分目录，几百个地点会摊出几百个大多只有零张图的空夹。
	//   文件名带地点号：在对象存储控制台里一眼看出这张是谁的，不用回库查。
	//   带内容哈希：同一张图重复上传落到同一个 key，天然去重。
	key := fmt.Sprintf("%d/%s/%d-%s.webp", datasetID, time.Now().Format("200601"),
		srcPK, hex.EncodeToString(sum[:])[:16])
	if err := s.media.Save(ctx, backend, key, data, "image/webp"); err != nil {
		return 0, "", err
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO place_images
		(dataset_id, src_pk, storage, object_key, width, height, bytes, position)
		VALUES ($1,$2,$3,$4,$5,$6,$7,
		        COALESCE((SELECT max(position)+1 FROM place_images WHERE dataset_id=$1 AND src_pk=$2), 0))
		ON CONFLICT (dataset_id, src_pk, object_key) DO UPDATE SET bytes=EXCLUDED.bytes
		RETURNING id`,
		datasetID, srcPK, backend, key, w, h, int64(len(data))).Scan(&id); err != nil {
		return 0, "", err
	}
	return id, s.media.PublicURL(backend, key), nil
}

// adminPlaceImageUpload 收下浏览器转好的 WebP 并落库。
// 存哪边由后台设置决定；对象存储的凭据只在服务端，前端拿到的永远是最终 URL。
func (s *Server) adminPlaceImageUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		s.json(w, map[string]any{"error": "还没有可编辑的数据集"})
		return
	}
	if err := r.ParseMultipartForm(imageUploadMem); err != nil {
		s.json(w, map[string]any{"error": "上传内容读不出来（可能太大）"})
		return
	}
	src, err := strconv.Atoi(strings.TrimSpace(r.FormValue("src_pk")))
	if err != nil {
		s.json(w, map[string]any{"error": "地点标识无效"})
		return
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT true FROM places
		WHERE dataset_id=$1 AND src_pk=$2`, ds.ID, src).Scan(&exists); err != nil {
		s.json(w, map[string]any{"error": "地点不存在"})
		return
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM place_images
		WHERE dataset_id=$1 AND src_pk=$2`, ds.ID, src).Scan(&count); err != nil {
		s.serverError(w, r, err)
		return
	}
	if count >= imageMaxPer {
		s.json(w, map[string]any{"error": fmt.Sprintf("一个地点最多 %d 张，先删几张再传", imageMaxPer)})
		return
	}

	file, hdr, err := r.FormFile("file")
	if err != nil {
		s.json(w, map[string]any{"error": "没有收到图片"})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, imageMaxBytes+1))
	if err != nil {
		s.json(w, map[string]any{"error": "读取上传内容失败"})
		return
	}
	if len(data) > imageMaxBytes {
		s.json(w, map[string]any{"error": fmt.Sprintf("图片超过 %dMB", imageMaxBytes>>20)})
		return
	}
	if !isWebP(data) {
		// 前端没转成功（老浏览器不支持 canvas.toBlob('image/webp')）时给出明确提示
		s.json(w, map[string]any{"error": "只接受 WebP：请用较新的浏览器，或在转换完成后再保存"})
		return
	}

	id, url, err := s.savePlaceImage(ctx, ds.ID, src, data,
		atoiOr(r.FormValue("w"), 0), atoiOr(r.FormValue("h"), 0))
	if err != nil {
		log.Printf("地点图片写入失败（src_pk=%d）: %v", src, err)
		s.json(w, map[string]any{"error": "保存图片失败：" + err.Error()})
		return
	}
	backend, _ := s.imageBackend(ctx)
	out := map[string]any{"ok": true, "id": id, "url": url, "bytes": len(data),
		"name": hdr.Filename, "storage": backend}
	if url == "" {
		out["error"] = "图片已存进对象存储，但没配 s3_public_base，浏览器取不到；" +
			"请在 conf/app.ini 里补上公开域名后重新上传"
	}
	s.json(w, out)
}

// adminPlaceImageDelete 删掉一张图：先删对象再删行（对象删失败也不留孤儿行）。
func (s *Server) adminPlaceImageDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		s.json(w, map[string]any{"error": "还没有可编辑的数据集"})
		return
	}
	// 前端是用 FormData 提交的（multipart），这里**不要**先 r.ParseForm()：
	// 它会把 r.Form 先填成「不含 multipart 字段」的那一份，之后再怎么读都是空的
	// （FormValue 只在 r.Form == nil 时才去解析）。直接用 FormValue，它自己会挑对解析方式。
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if err != nil {
		s.json(w, map[string]any{"error": "图片标识无效"})
		return
	}
	var backend, key string
	if err := s.db.QueryRowContext(ctx, `SELECT storage, object_key FROM place_images
		WHERE id=$1 AND dataset_id=$2`, id, ds.ID).Scan(&backend, &key); err != nil {
		s.json(w, map[string]any{"error": "图片不存在"})
		return
	}
	if err := s.media.Delete(ctx, backend, key); err != nil {
		// 对象删不掉就留着行，否则文件永远无人回收
		log.Printf("删除图片对象失败（%s %s）: %v", backend, key, err)
		s.json(w, map[string]any{"error": "删除文件失败：" + err.Error()})
		return
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM place_images WHERE id=$1 AND dataset_id=$2`,
		id, ds.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.json(w, map[string]any{"ok": true})
}

// imageBackend 取当前设置的存储后端；选了对象存储但凭据不全时明确报出来，
// 不要静默退回本地（那样用户以为存到 R2 了，其实落在服务器磁盘上）。
func (s *Server) imageBackend(ctx context.Context) (string, error) {
	st, err := s.sets.Load(ctx)
	if err != nil {
		return "", err
	}
	if st.MediaBackend == setting.MediaBackendS3 {
		if !s.media.S3Ready() {
			return "", fmt.Errorf("后台选了对象存储，但 conf/app.ini 里的 s3_* 没配齐")
		}
		return setting.MediaBackendS3, nil
	}
	return setting.MediaBackendLocal, nil
}

// placeImagesOf 读某个地点的图片，按 position 排序。
func (s *Server) placeImagesOf(ctx context.Context, datasetID int64, srcPK int) ([]PlaceImage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, storage, object_key, width, height, bytes
		FROM place_images WHERE dataset_id=$1 AND src_pk=$2 ORDER BY position, id`, datasetID, srcPK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlaceImage
	for rows.Next() {
		var img PlaceImage
		var backend, key string
		if err := rows.Scan(&img.ID, &backend, &key, &img.Width, &img.Height, &img.Bytes); err != nil {
			return nil, err
		}
		url := s.media.PublicURL(backend, key)
		if url == "" {
			continue // 存储配置被撤掉后取不到址，跳过而不是给出破图
		}
		img.URL = url
		out = append(out, img)
	}
	return out, rows.Err()
}

// serveMedia 提供本地存储的图片。key 是内容寻址的（带 sha1 前缀），
// 内容永不变化，所以可以长期强缓存。
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/media/")
	full, err := s.media.LocalPath(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mimeOfExt(filepath.Ext(full)))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, path.Base(full), fi.ModTime(), f)
}

func mimeOfExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".webp":
		return "image/webp"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	default:
		return "application/octet-stream"
	}
}

// isWebP 校验 RIFF....WEBP 头。前端已经转过一道，这里是防御性的最后一道。
func isWebP(b []byte) bool {
	return len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP"
}
