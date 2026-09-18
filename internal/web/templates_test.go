package web

import (
	"html/template"
	"path/filepath"
	"testing"
)

// TestPageTemplatesParse 把每个页面模板都解析一遍。
// 模板是启动时编译的（见 New），语法错会让整个服务起不来；
// 这个测试把它提前到 go test 阶段，不用真起一次服务才发现。
func TestPageTemplatesParse(t *testing.T) {
	for key, def := range pageFiles {
		if _, err := template.New(filepath.Base(def.files[0])).Funcs(templateFuncs()).
			ParseFS(tmplFS, def.files...); err != nil {
			t.Errorf("解析模板 %s 失败: %v", key, err)
		}
	}
}
