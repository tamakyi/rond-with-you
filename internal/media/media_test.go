package media

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rond-with-you/internal/config"
)

// TestSignV4MatchesAWSExample 用 AWS 官方文档里的示例向量锁死 SigV4。
// 手写签名最容易在「规范化请求 / 签名串 / 派生密钥」这三步之一算错，
// 而那类错误只有真去 PUT 才会暴露；有了官方向量，离线就能验。
//
// 向量来自 AWS《Examples of the complete Signature Version 4 signing process》，
// 场景是 GET 一个带 Range 的对象（服务 s3、区域 us-east-1）。
func TestSignV4MatchesAWSExample(t *testing.T) {
	s := New(&config.Config{
		S3Endpoint:  "https://examplebucket.s3.amazonaws.com",
		S3Region:    "us-east-1",
		S3Bucket:    "examplebucket",
		S3AccessKey: "AKIAIOSFODNN7EXAMPLE",
		S3SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	})
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")

	s.signAt(req, nil, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))

	const want = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "Signature="+want) {
		t.Errorf("SigV4 与官方向量不符\n得到: %s\n期望签名: %s", auth, want)
	}
	if !strings.Contains(auth, "SignedHeaders=host;range;x-amz-content-sha256;x-amz-date") {
		t.Errorf("参与签名的头不对: %s", auth)
	}
	if !strings.Contains(auth, "Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request") {
		t.Errorf("Credential 作用域不对: %s", auth)
	}
}

// TestEscapeRFC3986 复核百分号转义：对象名里出现空格、加号、中文时不能被签错。
func TestEscapeRFC3986(t *testing.T) {
	cases := map[string]string{
		"abc-_.~":   "abc-_.~",
		"a b":       "a%20b",
		"a+b":       "a%2Bb",
		"中文":        "%E4%B8%AD%E6%96%87",
		"a/b":       "a%2Fb",
		"2026/01/1": "2026%2F01%2F1",
	}
	for in, want := range cases {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q，期望 %q", in, got, want)
		}
	}
	// 路径按段编码：分隔符要保留
	if got := canonicalURI("/a b/c+d/x.webp"); got != "/a%20b/c%2Bd/x.webp" {
		t.Errorf("canonicalURI 分段编码不对: %q", got)
	}
	if got := escapePath("2026/01/a b.webp"); got != "2026/01/a%20b.webp" {
		t.Errorf("escapePath 不对: %q", got)
	}
}

// TestLocalKeyRoundTrip 本地后端：key → 磁盘路径 → 读回，并挡住目录穿越。
func TestLocalKeyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(&config.Config{DataDir: dir})
	if err := s.Save(t.Context(), BackendLocal, "9/12/abc.webp", []byte("RIFFwebp"), "image/webp"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, err := s.LocalPath("9/12/abc.webp")
	if err != nil {
		t.Fatalf("取路径失败: %v", err)
	}
	if b, err := os.ReadFile(got); err != nil || string(b) != "RIFFwebp" {
		t.Fatalf("读回失败: %q %v", b, err)
	}
	if err := s.Delete(t.Context(), BackendLocal, "9/12/abc.webp"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if err := s.Delete(t.Context(), BackendLocal, "9/12/abc.webp"); err != nil {
		t.Fatalf("重复删除不该报错: %v", err)
	}
	// key 一律当成「相对图片目录」的路径：带 .. 的会被规整，规整后必须仍落在目录内
	for _, tricky := range []string{"../conf/app.ini", "a/../../etc/passwd", "/abs/x.webp"} {
		p, err := s.LocalPath(tricky)
		if err != nil {
			continue // 直接拒绝也可以
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("路径逃出了图片目录: %q -> %q", tricky, p)
		}
	}
}

// TestPublicURL 复核取址：本地走站内，对象存储走公开域名；没配公开域名要返回空串。
func TestPublicURL(t *testing.T) {
	local := New(&config.Config{DataDir: t.TempDir()})
	if got := local.PublicURL(BackendLocal, "9/a.webp"); got != "/media/9/a.webp" {
		t.Errorf("本地取址不对: %q", got)
	}
	noBase := New(&config.Config{DataDir: t.TempDir(), S3Endpoint: "https://x.r2.cloudflarestorage.com",
		S3Bucket: "b", S3AccessKey: "k", S3SecretKey: "s"})
	if got := noBase.PublicURL(BackendS3, "9/a.webp"); got != "" {
		t.Errorf("没配公开域名时应当返回空串（提示配错），实际 %q", got)
	}
	withBase := New(&config.Config{DataDir: t.TempDir(), S3PublicBase: "https://img.example.com"})
	if got := withBase.PublicURL(BackendS3, "9/a.webp"); got != "https://img.example.com/9/a.webp" {
		t.Errorf("对象存储取址不对: %q", got)
	}
}

// TestS3RequestUsesPathStyle 复核请求形态：路径式、带上内容哈希。
func TestS3RequestUsesPathStyle(t *testing.T) {
	s := New(&config.Config{S3Endpoint: "https://acct.r2.cloudflarestorage.com",
		S3Region: "auto", S3Bucket: "pics", S3AccessKey: "k", S3SecretKey: "s"})
	req, err := s.s3Request(t.Context(), http.MethodPut, "9/12/a b.webp", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := req.URL.Path, "/pics/9/12/a b.webp"; got != want {
		t.Errorf("路径不对: %q（期望 %q）", got, want)
	}
	if got := req.Header.Get("x-amz-content-sha256"); got != sha256Hex([]byte("x")) {
		t.Errorf("内容哈希不对: %q", got)
	}
	if req.URL.Host != "acct.r2.cloudflarestorage.com" {
		t.Errorf("主机不对: %q", req.URL.Host)
	}
	// 签名后不应报错，且路径仍是「已解码」的原样路径（签名时再按段转义）
	s.sign(req, []byte("x"))
	if req.Header.Get("Authorization") == "" {
		t.Error("签名后没有 Authorization 头")
	}
}
