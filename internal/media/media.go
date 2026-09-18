// Package media 负责地点图片的落盘 / 对象存储与取址。
//
// 存哪边由后台设置决定（local | s3），S3 凭据只在 conf/app.ini 里、不进数据库。
// 上传的字节由浏览器转成 WebP 之后才发过来，服务端只做「接收 → 原样写出去」，
// 不碰解码与转码，1 核机器也扛得住。
package media

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rond-with-you/internal/config"
)

// BackendLocal / BackendS3 是两种存储后端的标识（与后台设置里的取值一致）。
const (
	BackendLocal = "local"
	BackendS3    = "s3"
)

// Store 把「写到哪」收敛到一处：本地磁盘或 S3 兼容对象存储。
type Store struct {
	cfg    *config.Config
	client *http.Client
}

func New(cfg *config.Config) *Store {
	return &Store{cfg: cfg, client: &http.Client{Timeout: 60 * time.Second}}
}

// LocalDir 返回本地图片目录，供静态路由挂载。
func (s *Store) LocalDir() string { return s.cfg.MediaDir() }

// S3Ready 表示对象存储是否配置齐全（后台选了 s3 但没配齐时要能给出明确提示）。
func (s *Store) S3Ready() bool { return s.cfg.S3Ready() }

// LocalPath 把对象 key 映射成本地磁盘路径，并保证不越出图片目录。
func (s *Store) LocalPath(key string) (string, error) {
	clean := path.Clean("/" + key)
	if clean == "/" || strings.Contains(clean, "..") {
		return "", fmt.Errorf("非法对象名 %q", key)
	}
	return filepath.Join(s.cfg.MediaDir(), filepath.FromSlash(strings.TrimPrefix(clean, "/"))), nil
}

// PublicURL 返回浏览器可直接放进 <img src> 的地址。
// 本地存储走站内 /media/；对象存储走 s3_public_base（没配就返回空串，
// 调用方要当成「配错了」处理——R2 桶默认私有，不给公开域名浏览器取不到）。
func (s *Store) PublicURL(backend, key string) string {
	if backend == BackendS3 {
		return s.S3URL(key)
	}
	return "/media/" + strings.TrimPrefix(key, "/")
}

// S3URL 返回对象存储上的公开地址；未配 s3_public_base 时返回空串。
func (s *Store) S3URL(key string) string {
	if s.cfg.S3PublicBase == "" {
		return ""
	}
	return s.cfg.S3PublicBase + "/" + strings.TrimPrefix(key, "/")
}

// Save 写入一个对象。backend 传 BackendLocal 或 BackendS3。
func (s *Store) Save(ctx context.Context, backend, key string, data []byte, contentType string) error {
	switch backend {
	case BackendLocal:
		full, err := s.LocalPath(key)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		// 先写临时文件再改名：中途失败不会留下半张图
		tmp := full + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, full)
	case BackendS3:
		if !s.cfg.S3Ready() {
			return errors.New("对象存储未配置齐全（需要 s3_endpoint / s3_bucket / s3_access_key / s3_secret_key）")
		}
		return s.putS3(ctx, key, data, contentType)
	default:
		return fmt.Errorf("未知的存储后端 %q", backend)
	}
}

// Delete 删掉一个对象；对象本来就不在不算错误（本地文件不存在 / S3 返回 404）。
func (s *Store) Delete(ctx context.Context, backend, key string) error {
	switch backend {
	case BackendLocal:
		full, err := s.LocalPath(key)
		if err != nil {
			return err
		}
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return err
		}
		s.pruneEmptyDirs(filepath.Dir(full))
		return nil
	case BackendS3:
		if !s.cfg.S3Ready() {
			return nil // 配置被撤掉了，本地已经无从删除，交给调用方记日志
		}
		return s.deleteS3(ctx, key)
	default:
		return fmt.Errorf("未知的存储后端 %q", backend)
	}
}

// pruneEmptyDirs 从里往外收掉空目录。按月份分目录后，删掉某个月最后一张图就会留下空夹；
// 只删"空"的（os.Remove 对非空目录会报错，就此打住），且绝不越出图片根目录。
func (s *Store) pruneEmptyDirs(dir string) {
	root := filepath.Clean(s.cfg.MediaDir())
	for filepath.Clean(dir) != root && strings.HasPrefix(filepath.Clean(dir), root+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

/* ---------- S3 兼容对象存储：只实现 PUT / DELETE + SigV4 签名 ---------- */
func (s *Store) putS3(ctx context.Context, key string, data []byte, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req, err := s.s3Request(ctx, http.MethodPut, key, data)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	// 图片内容寻址、key 里带哈希，可以长期缓存
	req.Header.Set("Cache-Control", "public, max-age=31536000, immutable")
	s.sign(req, data)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("上传对象存储失败：HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *Store) deleteS3(ctx context.Context, key string) error {
	req, err := s.s3Request(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	s.sign(req, nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("删除对象失败：HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *Store) s3Request(ctx context.Context, method, key string, body []byte) (*http.Request, error) {
	// 路径式访问：R2 / MinIO / AWS 都支持，省掉虚拟主机名的判断
	u := s.cfg.S3Endpoint + "/" + s.cfg.S3Bucket + "/" + escapePath(key)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	req.Header.Set("x-amz-content-sha256", sha256Hex(body))
	return req, nil
}

// sign 给请求补上 SigV4 签名（用当前时间）。
func (s *Store) sign(req *http.Request, body []byte) {
	s.signAt(req, body, time.Now().UTC())
}

// unsignedHeaders 是不参与签名的头：Authorization 自己就是签名结果，
// 其余几个由 transport 在发送时补/改，签了反而对不上。
var unsignedHeaders = map[string]struct{}{
	"authorization": {}, "content-length": {}, "user-agent": {},
	"accept-encoding": {}, "connection": {},
}

// signAt 与 sign 相同，但时间可注入——SigV4 的签名依赖时刻，测试要能锁死它。
// 签进去的是「请求上现有的全部头（去掉上面那几个）+ host」，签全量比挑几个更稳：
// 不会因为漏签某个自定义头而被服务端拒绝。
func (s *Store) signAt(req *http.Request, body []byte, now time.Time) {
	const (
		algo    = "AWS4-HMAC-SHA256"
		service = "s3"
	)
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256Hex(body)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", amzDate)

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		name := strings.ToLower(k)
		if _, skip := unsignedHeaders[name]; skip {
			continue
		}
		headers[name] = strings.Join(v, ",")
	}
	// 参与签名的头必须按名字排序，且值要去掉首尾空白、内部连续空白压成一个空格
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(strings.Join(strings.Fields(headers[n]), " "))
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.Path),
		canonicalQuery(req.URL.RawQuery),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, s.cfg.S3Region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		algo,
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+s.cfg.S3SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, s.cfg.S3Region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algo, s.cfg.S3AccessKey, scope, signedHeaders, signature))
}

// canonicalURI 逐段编码路径；"/" 保留作分隔符，其余按 RFC 3986 转义。
func canonicalURI(p string) string {
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = escape(s)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery 按 key 排序并转义查询串（当前用不到，留着保证签名公式完整）。
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func escapePath(p string) string {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range segs {
		segs[i] = escape(s)
	}
	return strings.Join(segs, "/")
}

// escape 实现 RFC 3986 的 unreserved 之外全部百分号转义（空格要转成 %20，不能是 +）。
func escape(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
