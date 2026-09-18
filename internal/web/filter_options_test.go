package web

import (
	"bytes"
	"strings"
	"testing"

	"rond-with-you/internal/stats"
)

// TestTagDropdownKeepsAllOptionsUnderFilter 盯住一个真实踩过的坑。
//
// 统计页的「标签排行」把数据放在自己的 Tags 字段上，而它内嵌了 Page —— 模板里的
// `.Tags` 会优先取外层那个，于是公共筛选里的标签下拉被换成了「筛选后剩下的标签」：
// 一旦选中某个标签，其它标签就从下拉里消失，用户再也切不回去。
// 现在下拉专用字段叫 Page.TagOptions，与排行用的 Tags 分开。
func TestTagDropdownKeepsAllOptionsUnderFilter(t *testing.T) {
	f := newMovementFixture(t)
	tmpl := f.srv.pages["stats"].t // layout.html + stats.html 一起解析过，「filters」在里面

	data := StatsData{Page: Page{
		FilterBase: "/stats",
		TagOptions: []stats.TagStat{{ID: 1, Name: "甲"}, {ID: 2, Name: "乙"}},
		TagID:      1, // 当前正按「甲」筛选
	}}
	// 排行只筛出被选中的那一个
	data.Tags = []stats.TagStat{{ID: 1, Name: "甲"}}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "filters", data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	// 只看标签那一个 select：整个筛选表单里还有城市、最短停留等下拉
	start := strings.Index(html, `<select name="tag">`)
	if start < 0 {
		t.Fatalf("没找到标签下拉：\n%s", html)
	}
	seg := html[start:]
	if end := strings.Index(seg, "</select>"); end >= 0 {
		seg = seg[:end]
	}
	if got := strings.Count(seg, "<option"); got != 3 { // 全部标签 + 甲 + 乙
		t.Errorf("标签下拉应当始终列出全部标签（3 项），实际 %d 项\n%s", got, seg)
	}
	if !strings.Contains(seg, ">乙</option>") {
		t.Error("未被选中的标签「乙」从下拉里消失了")
	}
}
