package web

import (
	"strings"
	"testing"
)

func TestTrackTag(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"https://umami.example.com/script.js",
			`<script defer src="https://umami.example.com/script.js"></script>`},
		{"http://a/b.js?x=1&y=2", `<script defer src="http://a/b.js?x=1&y=2"></script>`},
		// 整段贴进来的原样输出（百度统计这类要带初始化代码）
		{`<script>var _hmt=_hmt||[];</script><script src="https://hm.baidu.com/hm.js?x"></script>`,
			`<script>var _hmt=_hmt||[];</script><script src="https://hm.baidu.com/hm.js?x"></script>`},
		// 形如 URL 却夹了引号/尖括号：宁可不挂，也不拼出可注出属性的标签
		{`https://x/y.js" onload="alert(1)`, ""},
		{`https://x/<y>.js`, ""},
		{"  https://a/b.js  ", `<script defer src="https://a/b.js"></script>`},
	}
	for _, c := range cases {
		if got := string(trackTag(c.in)); got != c.want {
			t.Errorf("trackTag(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	if !strings.Contains(string(trackTag("//cdn.example.com/a.js")), "//cdn.example.com/a.js") {
		t.Error("不以 http 开头但含尖括号以外的内容应原样输出")
	}
}
