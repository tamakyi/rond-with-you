package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// webpBytes 造一段带正确 RIFF/WEBP 魔数的假数据。服务端只校验文件头
// （转码在浏览器做），所以这里不需要真的 WebP 编码器。
func webpBytes(n int) []byte {
	b := make([]byte, 12+n)
	copy(b[0:4], "RIFF")
	copy(b[8:12], "WEBP")
	for i := 12; i < len(b); i++ {
		b[i] = byte(i)
	}
	return b
}

// postMultipart 按浏览器 FormData 的形态发一个 multipart 请求。
func postMultipart(t *testing.T, f *movementFixture, handler http.HandlerFunc, fields map[string]string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for field, data := range files {
		part, err := w.CreateFormFile(field, "image.webp")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/x", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req = req.WithContext(f.ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// setMediaBackend 把「图片存哪边」临时改成 backend，测试结束再恢复。
// 多个测试共用一个库，不还原就会互相干扰（前一个设了 s3，后一个的本地上传就失败了）。
func setMediaBackend(t *testing.T, f *movementFixture, backend string) {
	t.Helper()
	var prev string
	if err := f.pg.QueryRowContext(f.ctx, `SELECT COALESCE(value->>'media_backend','local')
		FROM settings WHERE key='site'`).Scan(&prev); err != nil {
		t.Fatalf("读图片存储设置失败: %v", err)
	}
	write := func(v string) {
		t.Helper()
		if _, err := f.pg.ExecContext(f.ctx, `UPDATE settings
			SET value = jsonb_set(value, '{media_backend}', to_jsonb($1::text)) WHERE key='site'`, v); err != nil {
			t.Fatalf("写图片存储设置失败: %v", err)
		}
	}
	write(backend)
	t.Cleanup(func() { write(prev) })
}

// TestPlaceImageUploadDelete 端到端跑一遍浏览器那条路：
// multipart 上传 → 落库 + 落盘 → /media 取回同样的字节 → multipart 删除 → 两处都没了。
//
// 这里刻意用 multipart 而不是 urlencoded：前端是用 FormData 发的，
// 而「先 ParseForm 再读 multipart」会把字段读成空——这个坑真踩过（删除按钮点了没反应）。
func TestPlaceImageUploadDelete(t *testing.T) {
	f := newMovementFixture(t)
	f.cfg.DataDir = t.TempDir() // 图片写进临时目录，别脏了 storage/
	setMediaBackend(t, f, "local")

	// 借一个现成的地点
	var src int
	if err := f.pg.QueryRowContext(f.ctx, `SELECT src_pk FROM places
		WHERE dataset_id=$1 ORDER BY src_pk LIMIT 1`, f.dsID).Scan(&src); err != nil {
		t.Fatalf("找不到地点: %v", err)
	}

	raw := webpBytes(64)
	rec := postMultipart(t, f, f.srv.adminPlaceImageUpload,
		map[string]string{"src_pk": fmt.Sprint(src), "w": "1200", "h": "900"},
		map[string][]byte{"file": raw})
	var up struct {
		OK    bool   `json:"ok"`
		ID    int64  `json:"id"`
		URL   string `json:"url"`
		Bytes int    `json:"bytes"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatalf("上传响应不是 JSON: %s", rec.Body.String())
	}
	if !up.OK || up.Error != "" {
		t.Fatalf("上传失败: %s", rec.Body.String())
	}
	if !strings.HasPrefix(up.URL, "/media/") {
		t.Fatalf("本地存储的地址应以 /media/ 开头，实际 %q", up.URL)
	}

	// 落盘 + 落库
	full, err := f.srv.media.LocalPath(strings.TrimPrefix(up.URL, "/media/"))
	if err != nil {
		t.Fatalf("取路径失败: %v", err)
	}
	if got, err := os.ReadFile(full); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("落盘的字节与上传的不一致: %v", err)
	}

	// /media 直出同样的字节
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", up.URL, nil).WithContext(f.ctx)
	f.srv.serveMedia(rec, req)
	if rec.Code != 200 {
		t.Fatalf("取图返回 %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Fatal("取回的字节与上传的不一致")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/webp" {
		t.Errorf("Content-Type 不对: %q", ct)
	}

	// 非 WebP 必须被挡住（前端转码失败时会发来 PNG）
	rec = postMultipart(t, f, f.srv.adminPlaceImageUpload,
		map[string]string{"src_pk": fmt.Sprint(src)},
		map[string][]byte{"file": append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)})
	if !strings.Contains(rec.Body.String(), "只接受 WebP") {
		t.Errorf("非 WebP 没被拒绝: %s", rec.Body.String())
	}

	// 删除：同样走 multipart
	rec = postMultipart(t, f, f.srv.adminPlaceImageDelete,
		map[string]string{"id": fmt.Sprint(up.ID)}, nil)
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("删除失败: %s", rec.Body.String())
	}
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Errorf("文件没删掉: %v", err)
	}
	var n int
	if err := f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM place_images WHERE id=$1`, up.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("库里的记录没删掉")
	}
}

// TestPhotoImportWithImage 照片导入「存图」：地点与该行图片要在一次导入里都建好，
// 并且图片挂在正确的 src_pk 上。
func TestPhotoImportWithImage(t *testing.T) {
	f := newMovementFixture(t)
	f.cfg.DataDir = t.TempDir()
	setMediaBackend(t, f, "local")

	raw := webpBytes(48)
	rec := postMultipart(t, f, f.srv.adminPhotosImport, map[string]string{
		"n": "1", "coord": "wgs84", "keep_0": "on",
		"lat_0": "30.123456", "lon_0": "120.123456",
		"arrival_0": "2026-04-18T18:14", "name_0": "带图的导入点",
		"country_0": "CN", "timezone_0": "Asia/Shanghai",
		"image_w_0": "1200", "image_h_0": "900",
	}, map[string][]byte{"image_0": raw})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("导入没返回重定向: %d %s", rec.Code, rec.Body.String())
	}
	loc, err := url.QueryUnescape(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(loc, "err=") {
		t.Fatalf("导入报错: %s", loc)
	}
	if !strings.Contains(loc, "配了 1 张图") {
		t.Errorf("结果消息里应提到配了图: %s", loc)
	}

	var src int
	var w, h int
	if err := f.pg.QueryRowContext(f.ctx, `SELECT p.src_pk, pi.width, pi.height FROM places p
		JOIN place_images pi ON pi.dataset_id=p.dataset_id AND pi.src_pk=p.src_pk
		WHERE p.dataset_id=$1 AND p.name='带图的导入点'`, f.dsID).Scan(&src, &w, &h); err != nil {
		t.Fatalf("配图没挂到地点上: %v", err)
	}
	if w != 1200 || h != 900 {
		t.Errorf("宽高没记对: %dx%d", w, h)
	}
	// 字节确实落盘了：目录是 <数据集>/<上传月份>/，文件名是 <地点号>-<哈希>.webp
	dir := filepath.Join(f.cfg.MediaDir(), fmt.Sprint(f.dsID), time.Now().Format("200601"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("月份目录不存在: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("期望落盘 1 个文件，实际 %d", len(entries))
	}
	if name := entries[0].Name(); !strings.HasPrefix(name, fmt.Sprint(src)+"-") ||
		!strings.HasSuffix(name, ".webp") {
		t.Errorf("文件名应是 <地点号>-<哈希>.webp，实际 %q", name)
	}
}

// TestPlaceImageS3Backend 走一遍对象存储那条路：用 httptest 假装 S3 端点，
// 断言请求打到「路径式 + 桶名 + 对象名」上，并带上了 SigV4 的 Authorization。
// 真去校验签名的是 media 包里的官方向量测试，这里盯的是接线对不对。
func TestPlaceImageS3Backend(t *testing.T) {
	f := newMovementFixture(t)
	f.cfg.DataDir = t.TempDir()

	var gotPath, gotAuth, gotCT string
	var gotBody []byte
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer fake.Close()

	f.cfg.S3Endpoint = fake.URL
	f.cfg.S3Region = "auto"
	f.cfg.S3Bucket = "pics"
	f.cfg.S3AccessKey = "AKIATEST"
	f.cfg.S3SecretKey = "secret"
	f.cfg.S3PublicBase = "https://img.example.com"

	setMediaBackend(t, f, "s3")

	var src int
	if err := f.pg.QueryRowContext(f.ctx, `SELECT src_pk FROM places
		WHERE dataset_id=$1 ORDER BY src_pk LIMIT 1`, f.dsID).Scan(&src); err != nil {
		t.Fatalf("找不到地点: %v", err)
	}

	raw := webpBytes(32)
	rec := postMultipart(t, f, f.srv.adminPlaceImageUpload,
		map[string]string{"src_pk": fmt.Sprint(src)}, map[string][]byte{"file": raw})

	var up struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatalf("上传响应不是 JSON: %s", rec.Body.String())
	}
	if !up.OK || up.Error != "" {
		t.Fatalf("对象存储上传失败: %s", rec.Body.String())
	}
	if !strings.HasPrefix(up.URL, "https://img.example.com/") {
		t.Errorf("对象存储的地址应当用 s3_public_base 拼，实际 %q", up.URL)
	}
	// 对象名 = <数据集>/<上传月份>/<地点号>-<内容哈希>.webp
	parts := strings.Split(strings.TrimPrefix(gotPath, "/pics/"), "/")
	if len(parts) != 3 {
		t.Fatalf("对象名层级不对（应为 数据集/月份/文件名）: %q", gotPath)
	}
	if parts[0] != fmt.Sprint(f.dsID) {
		t.Errorf("第一层应是数据集号，实际 %q", parts[0])
	}
	if len(parts[1]) != 6 || !strings.HasPrefix(parts[1], time.Now().Format("2006")) {
		t.Errorf("第二层应是上传月份（YYYYMM），实际 %q", parts[1])
	}
	if !strings.HasPrefix(parts[2], fmt.Sprint(src)+"-") || !strings.HasSuffix(parts[2], ".webp") {
		t.Errorf("文件名应是 <地点号>-<哈希>.webp，实际 %q", parts[2])
	}
	if !strings.Contains(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIATEST/") ||
		!strings.Contains(gotAuth, "/auto/s3/aws4_request") {
		t.Errorf("没带上 SigV4 签名: %q", gotAuth)
	}
	if gotCT != "image/webp" {
		t.Errorf("Content-Type 不对: %q", gotCT)
	}
	if !bytes.Equal(gotBody, raw) {
		t.Error("发出去的字节与上传的不一致")
	}

	// 存储被切到对象存储后，本地不应再落盘
	entries, _ := os.ReadDir(path.Join(f.cfg.MediaDir(), fmt.Sprint(f.dsID)))
	if len(entries) != 0 {
		t.Errorf("切到对象存储后不该再写本地磁盘，却发现 %d 个条目", len(entries))
	}
}
