package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Listen      string
	DSN         string
	SecretKey   string
	UploadDir   string
	DataDir     string
	SessionDays int
	Debug       bool

	// 底图。amap 走免密钥瓦片；填了 map_key 或选 tianditu 时改由服务端代理，
	// 密钥不出现在页面源码里。
	MapProvider string // amap | tianditu | custom
	MapKey      string
	MapTileURL  string // custom 模式下的瓦片模板，支持 {s} {x} {y} {z} {key}
	MapCRS      string // custom 模式下底图的坐标系：gcj02 | wgs84

	// 地理编码（后台「按名称搜坐标」）。高德 Web 服务类型密钥，与瓦片用的
	// JS 密钥不是同一个；只经服务端代理调用，不进入前端源码。留空则功能关闭。
	GeocodeKey string

	// TrackScript 是第三方统计 / 追踪脚本。填 URL 会包成 <script src>；
	// 需要附加属性或初始化代码的服务（百度统计、Umami 等）可以把整段 <script> 贴进来。
	TrackScript string

	// 地点图片的对象存储（S3 兼容，Cloudflare R2 等）。存哪边由后台「图片存储」
	// 设置决定，凭据只留在服务端、不进数据库也不下发前端。全部留空 = 只能用本地磁盘。
	S3Endpoint   string // R2 形如 https://<account>.r2.cloudflarestorage.com
	S3Region     string // R2 填 auto
	S3Bucket     string
	S3AccessKey  string
	S3SecretKey  string
	S3PublicBase string // 公开访问前缀（自定义域名或 r2.dev），拼对象 key 即为 <img src>
}

// S3Ready 判断对象存储是否配置齐全（缺一不可，否则只能在本地存）。
func (c *Config) S3Ready() bool {
	return c.S3Endpoint != "" && c.S3Bucket != "" && c.S3AccessKey != "" && c.S3SecretKey != ""
}

// MediaDir 是本地图片的落盘目录。
func (c *Config) MediaDir() string { return filepath.Join(c.DataDir, "media") }

func Load(path string) (*Config, error) {
	kv := map[string]string{}
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			kv[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}

	cfg := &Config{
		Listen:      get(kv, "listen", ":8080"),
		DSN:         get(kv, "dsn", ""),
		SecretKey:   get(kv, "secret_key", ""),
		UploadDir:   get(kv, "upload_dir", "storage/uploads"),
		DataDir:     get(kv, "data_dir", "storage/data"),
		SessionDays: getInt(kv, "session_days", 14),
		Debug:       get(kv, "debug", "false") == "true",

		MapProvider: strings.ToLower(get(kv, "map_provider", "amap")),
		MapKey:      get(kv, "map_key", ""),
		MapTileURL:  get(kv, "map_tile_url", ""),
		MapCRS:      strings.ToLower(get(kv, "map_crs", "gcj02")),

		GeocodeKey: get(kv, "geocode_key", ""),

		TrackScript: strings.TrimSpace(get(kv, "track_script", "")),

		S3Endpoint:   strings.TrimRight(get(kv, "s3_endpoint", ""), "/"),
		S3Region:     get(kv, "s3_region", "auto"),
		S3Bucket:     get(kv, "s3_bucket", ""),
		S3AccessKey:  get(kv, "s3_access_key", ""),
		S3SecretKey:  get(kv, "s3_secret_key", ""),
		S3PublicBase: normalizeBaseURL(get(kv, "s3_public_base", "")),
	}
	if cfg.DSN == "" {
		return nil, fmt.Errorf("配置缺少 dsn（PostgreSQL 连接串）")
	}
	if cfg.MapProvider != "amap" && cfg.MapProvider != "tianditu" && cfg.MapProvider != "custom" {
		return nil, fmt.Errorf("map_provider 只支持 amap / tianditu / custom，当前为 %q", cfg.MapProvider)
	}
	if cfg.MapProvider == "tianditu" && cfg.MapKey == "" {
		return nil, fmt.Errorf("map_provider = tianditu 时必须填写 map_key（天地图密钥）")
	}
	if cfg.MapProvider == "custom" && cfg.MapTileURL == "" {
		return nil, fmt.Errorf("map_provider = custom 时必须填写 map_tile_url")
	}
	if cfg.MapCRS != "gcj02" && cfg.MapCRS != "wgs84" {
		return nil, fmt.Errorf("map_crs 只支持 gcj02 / wgs84，当前为 %q", cfg.MapCRS)
	}
	if cfg.SecretKey == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		cfg.SecretKey = hex.EncodeToString(b)
	}
	for _, d := range []string{cfg.UploadDir, cfg.DataDir, cfg.MediaDir()} {
		if err := os.MkdirAll(filepath.Clean(d), 0o755); err != nil {
			return nil, fmt.Errorf("创建目录 %s: %w", d, err)
		}
	}
	return cfg, nil
}

// normalizeBaseURL 把「域名」补成完整 URL。
// s3_public_base 是直接拼进 <img src> 的，写成裸域名（r2.example.com）会得到相对地址，
// 浏览器把当前页面的路径拼上去，图全变破图——这种写法太容易发生了，这里统一补 https。
func normalizeBaseURL(v string) string {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v == "" || strings.Contains(v, "://") {
		return v
	}
	return "https://" + v
}

func get(kv map[string]string, key, def string) string {
	if v, ok := kv[key]; ok && v != "" {
		return v
	}
	return def
}

func getInt(kv map[string]string, key string, def int) int {
	if v, ok := kv[key]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
