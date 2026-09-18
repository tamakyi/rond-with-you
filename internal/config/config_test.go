package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestExampleConfigDocumentsEveryKey 守住「样例配置与代码不脱节」。
//
// app.example.ini 是用户唯一能看到的配置说明：里面没有的键，等于不存在。
// 反过来说，样例里写了代码不读的键（曾经有个 base_url）同样是坑——填了没反应，
// 用户只会怀疑自己写错了。两个方向都检查。
func TestExampleConfigDocumentsEveryKey(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("读 config.go: %v", err)
	}
	// 以真正的读取点为准：get(kv, "...") / getInt(kv, "...")
	// 注意键名要允许数字（s3_endpoint 这类），写成 [a-z_]+ 会把它们整批漏掉，
	// 于是「样例里写了但代码不读」这种坑就查不出来了。
	re := regexp.MustCompile(`get(?:Int)?\(kv,\s*"([a-z0-9_]+)"`)
	used := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		used[m[1]] = true
	}
	if len(used) < 10 {
		t.Fatalf("只从 config.go 扫到 %d 个配置键，抽取规则可能已失效", len(used))
	}

	ex, err := os.ReadFile("../../conf/app.example.ini")
	if err != nil {
		t.Fatalf("读 app.example.ini: %v", err)
	}
	documented := map[string]bool{}
	for _, line := range strings.Split(string(ex), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		k, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		documented[strings.TrimSpace(k)] = true
	}

	for k := range used {
		if !documented[k] {
			t.Errorf("app.example.ini 缺少配置项 %s（代码会读它）", k)
		}
	}
	for k := range documented {
		if !used[k] {
			t.Errorf("app.example.ini 里的 %s 代码并不读取，要么补上实现要么删掉", k)
		}
	}
}

// TestExampleConfigLoads 用示例配置真跑一遍解析，确认它开箱可用。
// dsn 是样例里的占位串，解析阶段不会去连库，所以这里能过。
func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../conf/app.example.ini")
	if err != nil {
		t.Fatalf("示例配置解析失败: %v", err)
	}
	// 挑几个「文档里承诺过」的默认值：改了代码或改了文档，这里都会响
	if cfg.Listen != ":8080" {
		t.Errorf("listen 默认值变了: %q", cfg.Listen)
	}
	if cfg.SessionDays != 14 {
		t.Errorf("session_days 默认值变了: %d", cfg.SessionDays)
	}
	if cfg.MapProvider != "amap" || cfg.MapCRS != "gcj02" {
		t.Errorf("底图默认值变了: provider=%q crs=%q", cfg.MapProvider, cfg.MapCRS)
	}
	if cfg.S3Region != "auto" || cfg.S3Ready() {
		t.Errorf("对象存储默认应当是「未配置」且 region=auto，实际 ready=%v region=%q",
			cfg.S3Ready(), cfg.S3Region)
	}
	if cfg.Debug {
		t.Error("示例配置里 debug 应当是关的")
	}
	if cfg.SecretKey == "" {
		t.Error("secret_key 留空时应自动生成，不该是空串")
	}
}
