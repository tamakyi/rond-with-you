package web

import (
	"fmt"
	"html"
	"io"
	"net/http"
)

// pausePage 渲染一个「停一下」的极简提示页：给一句人话、一个回首页的按钮，
// 需要时再自动跳回首页。用于「内容对访客不开放」这类情况——
// 直接甩 404 会让人以为站点坏了，说清楚再引导走开才不突兀。
//
// autoSeconds > 0 时页面会自己跳回首页（同时把按钮留给人手动点）。
func (s *Server) pausePage(w http.ResponseWriter, status int, icon, title, msg string, autoSeconds int) {
	var auto string
	if autoSeconds > 0 {
		auto = fmt.Sprintf(`<meta http-equiv="refresh" content="%d;url=/">`, autoSeconds)
	}
	if icon == "" {
		icon = "🔒"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">`+
		`<meta name="viewport" content="width=device-width,initial-scale=1">`+
		`<title>%s</title>%s`+
		`<link rel="stylesheet" href="/static/css/app.css?v=%s"></head>`+
		`<body class="notice-body"><div class="notice-card">`+
		`<div class="notice-ico">%s</div><h1>%s</h1><p>%s</p>`+
		`<a class="btn primary" href="/">返回首页</a>`,
		html.EscapeString(title), auto, assetVersion, icon,
		html.EscapeString(title), html.EscapeString(msg))
	if autoSeconds > 0 {
		fmt.Fprintf(w, `<div class="notice-tip">%d 秒后自动回到首页</div>`, autoSeconds)
	}
	io.WriteString(w, `</div></body></html>`)
}

// placeHiddenPage 是「这个地点没对访客开放」时的回应。
//
// 注意取舍：以前这里直接 404，好处是访客分不清「被藏起来了」和「根本不存在」。
// 现在改成明确告知，更好懂，代价是能反推出某个 ID 确实存在。个人站点这个取舍划算，
// 想换回原样只需要把这一处改回 http.NotFound。
func (s *Server) placeHiddenPage(w http.ResponseWriter) {
	s.pausePage(w, http.StatusForbidden, "🔒", "这个地点没有对访客开放",
		"站长按区域设置隐藏了这个地点的记录。带你去别处看看。", 4)
}

// notFoundPage 是「这个地址没有东西」时的回应。
//
// 状态码仍是 404（脚本和搜索引擎照旧当成不存在），但给人看的是站点样式的提示页 ——
// Go 默认那句纯文本 `404 page not found` 摆在白底上像是站点坏了。这个站其它「不给你看」
// 的场景（内容未开放、地点被区域隐藏）早就都走 pausePage 了，只有「压根没有」这类漏在外头。
//
// 与 placeHiddenPage 的区别要保持：那个是 403「存在但不给看」，这个是 404「就没有」。
func (s *Server) notFoundPage(w http.ResponseWriter, title, msg string) {
	if title == "" {
		title = "这个页面不存在"
	}
	if msg == "" {
		msg = "地址可能写错了，或者这条记录已经被删掉。带你去别处看看。"
	}
	s.pausePage(w, http.StatusNotFound, "🧭", title, msg, 4)
}
