package web

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"rond-with-you/internal/fog"
	"rond-with-you/internal/geo"
)

// fogMaskPage 管理「对访客隐藏」的迷雾范围。
func (s *Server) fogMaskPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "admin", "迷雾遮罩区")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.SetFlashFromQuery(r)
	p.HasMap = true // 布局按这个标记引入 Leaflet，漏了地图整块不出现
	masks := s.masksFor()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := FogMaskData{Page: p, Masks: masks, MapInfo: s.mapInfo()}
	s.render(w, "admin/fog-mask", d)
}

// fogMaskSave 新增或更新一块遮罩区。
// 表单里的坐标来自底图（可能是 WGS-84 的天地图），要换算成站点统一的 GCJ-02 再存。
func (s *Server) fogMaskSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	id, _ := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("id")), 10, 64)
	lat, ok1 := parseFloat(r.PostFormValue("lat"))
	lon, ok2 := parseFloat(r.PostFormValue("lon"))
	radius, ok3 := parseFloat(r.PostFormValue("radius_m"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	if !ok1 || !ok2 || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
		redirectFogMask(w, r, "err", "请先在地图上点选圆心")
		return
	}
	if !ok3 || radius < 50 || radius > 50000 {
		redirectFogMask(w, r, "err", "半径请填 50 ~ 50000 米")
		return
	}
	if r.PostFormValue("crs") == "wgs84" {
		lat, lon = geo.WGS84ToGCJ02(lat, lon)
	}
	// 保存成功后失效两处缓存：站长点完保存就该看到访客视角的变化
	defer func() { s.masks.invalidate(); s.fogTiles.clear() }()
	if _, err := fog.SaveMask(s.db, u.ID, fog.Mask{ID: id, Name: name, Lat: lat, Lon: lon, RadiusM: radius}); err != nil {
		s.serverError(w, r, err)
		return
	}
	verb := "已新增"
	if id > 0 {
		verb = "已更新"
	}
	label := name
	if label == "" {
		label = fmt.Sprintf("%.4f, %.4f", lat, lon)
	}
	redirectFogMask(w, r, "ok", fmt.Sprintf("%s遮罩区「%s」（半径 %s 米）", verb, label, trimNum(radius)))
}

// fogMaskDelete 删除一块遮罩区。
func (s *Server) fogMaskDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	id, _ := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("id")), 10, 64)
	if id <= 0 {
		redirectFogMask(w, r, "err", "遮罩区不存在")
		return
	}
	// 删除后同样立刻失效：站长的操作要立即反映到访客视角
	defer func() { s.masks.invalidate(); s.fogTiles.clear() }()
	if err := fog.DeleteMask(s.db, u.ID, id); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirectFogMask(w, r, "ok", "已删除该遮罩区")
}

func redirectFogMask(w http.ResponseWriter, r *http.Request, kind, msg string) {
	http.Redirect(w, r, "/admin/fog/mask?"+kind+"="+urlQueryEscape(msg), http.StatusSeeOther)
}

// trimNum 把 500.0 这种整数半径显示成 500。
func trimNum(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
