package web

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

// 可压缩的响应类型。图片（迷雾 PNG、图标）本身已是压缩格式，再压纯属浪费 CPU。
func compressible(ct string) bool {
	ct = strings.ToLower(ct)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	switch ct {
	case "text/html", "text/css", "text/plain", "text/javascript",
		"application/javascript", "application/json", "application/x-javascript",
		"image/svg+xml", "application/manifest+json", "application/xml":
		return true
	}
	return strings.HasPrefix(ct, "text/") && ct != "text/cache-manifest"
}

type gzipWriter struct {
	http.ResponseWriter
	gz   *gzip.Writer
	comp bool
	seen bool // 首部是否已定下来（WriteHeader 已调用，或首字节已写）
}

var gzipPool = sync.Pool{New: func() any {
	return gzip.NewWriter(nil)
}}

// WriteHeader 必须在首部提交前决定压不压。静态文件由 http.FileServer 提供，
// 它会先设好 Content-Length 再显式 WriteHeader(200/206)；若拖到 Write 里再改首部，
// 首部早已提交，会出现「声明长度 ≠ 实际压缩后长度」，浏览器报
// ERR_CONTENT_LENGTH_MISMATCH 并把 CSS/JS 截断。只有 200 才压缩，
// 206（Range）/304 等自带长度语义，一律原样透传。
func (g *gzipWriter) WriteHeader(code int) {
	if g.seen {
		return
	}
	g.seen = true
	if code == http.StatusOK && compressible(g.Header().Get("Content-Type")) {
		g.comp = true
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Set("Vary", "Accept-Encoding")
		g.Header().Del("Content-Length")
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.seen {
		g.seen = true
		// 上游没有显式 WriteHeader 时，首字节写入才能确定内容类型，此时决定压不压
		ct := g.Header().Get("Content-Type")
		if ct == "" {
			ct = http.DetectContentType(b)
			g.Header().Set("Content-Type", ct)
		}
		if compressible(ct) {
			g.comp = true
			g.Header().Set("Content-Encoding", "gzip")
			g.Header().Set("Vary", "Accept-Encoding")
			g.Header().Del("Content-Length")
		}
	}
	if !g.comp {
		return g.ResponseWriter.Write(b)
	}
	if g.gz == nil {
		g.gz = gzipPool.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
	}
	return g.gz.Write(b)
}

func (g *gzipWriter) Flush() {
	if g.gz != nil {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) close() {
	if g.gz != nil {
		g.gz.Close()
		gzipPool.Put(g.gz)
		g.gz = nil
	}
}

// gzipHandler 对声明了 gzip 且内容类型可压缩的响应做在线压缩。
func gzipHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 带 Range 的请求（浏览器校验 / 续传 immutable 缓存的静态资源）会返回 206 并自带
		// Content-Length，gzip 无法在首部已提交后再改写长度，会导致浏览器把 CSS/JS 截断
		// （ERR_CONTENT_LENGTH_MISMATCH）。这类请求直接透传、不压缩。
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			r.Header.Get("Upgrade") != "" ||
			r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}
