package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 专题 = 某段时间 + 某个区域内的足迹。地点来自「时间范围内的到访」，
// 迷雾不落库、由地图瓦片按视口自然呈现——所以专题只要把地图视野限定在
// 区域（或地点包围盒）上，「区域内的迷雾」就成立了。

const (
	// 表单现在精确到分钟；纯日期形式仍然接受（老表单缓存 / 手工拼接参数）
	topicTimeLayout     = "2006-01-02"
	topicDateTimeLayout = "2006-01-02T15:04"
)

// parseTopicTime 解析表单时间，两种格式都认。
func parseTopicTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.ParseInLocation(topicDateTimeLayout, raw, time.Local); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation(topicTimeLayout, raw, time.Local); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// ---------- 查询 ----------

// loadTopics 读专题列表。onlyEnabled 时只取对外展示的（前台用）。
func (s *Server) loadTopics(ctx context.Context, datasetID int64, onlyEnabled bool) ([]Topic, error) {
	q := `SELECT id, title, subtitle, description, start_at, end_at, center_lat, center_lon,
		radius_km, show_fog, enabled, sort_order
		FROM topics WHERE dataset_id=$1`
	if onlyEnabled {
		q += ` AND enabled`
	}
	q += ` ORDER BY sort_order, id`
	rows, err := s.db.QueryContext(ctx, q, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Topic
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.ID, &t.Title, &t.Subtitle, &t.Description, &t.StartAt, &t.EndAt,
			&t.CenterLat, &t.CenterLon, &t.RadiusKm, &t.ShowFog, &t.Enabled, &t.SortOrder); err != nil {
			return nil, err
		}
		t.StartDate = t.StartAt.Local().Format(topicDateTimeLayout)
		t.EndDate = t.EndAt.Local().Format(topicDateTimeLayout)
		t.RangeLabel = topicRangeLabel(t.StartAt, t.EndAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

// topicRangeLabel 给完整时间范围。整天（00:00 ~ 23:59）时只显示日期——
// 这种情况多是「按天圈的范围」，写成「00:00 ~ 23:59」只是噪音；
// 只要有一端带了具体时刻，就完整显示到分钟。
func topicRangeLabel(start, end time.Time) string {
	a, b := start.Local(), end.Local()
	wholeDays := a.Hour() == 0 && a.Minute() == 0 && b.Hour() == 23 && b.Minute() >= 59
	if wholeDays {
		if a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day() {
			return a.Format("2006-01-02")
		}
		return fmt.Sprintf("%s ~ %s", a.Format("2006-01-02"), b.Format("2006-01-02"))
	}
	if a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day() {
		return fmt.Sprintf("%s %s ~ %s", a.Format("2006-01-02"), a.Format("15:04"), b.Format("15:04"))
	}
	return fmt.Sprintf("%s ~ %s", a.Format("2006-01-02 15:04"), b.Format("2006-01-02 15:04"))
}

// topicHaversineSQL 是写进 SQL 的球面距离（公里）表达式，参数依次是中心纬度、中心经度。
// 放在 SQL 里是为了让「区域过滤」和聚合统计一次算完，不必先查一遍再回 Go 过滤。
const topicHaversineSQL = `6371 * 2 * asin(sqrt(
	power(sin(radians((%s - p.lat) / 2)), 2) +
	cos(radians(p.lat)) * cos(radians(%s)) * power(sin(radians((%s - p.lon) / 2)), 2)))`

// topicContent 取专题里的地点（含到访次数与停留），并汇总到访数 / 天数 / 城市数。
func (s *Server) topicContent(ctx context.Context, t *Topic, datasetID int64,
	regions []string, exclude bool) ([]MapPoint, int, int, int, error) {
	args := []any{datasetID, t.StartAt, t.EndAt}
	area := ""
	if t.HasArea() {
		args = append(args, t.CenterLat, t.CenterLon, t.RadiusKm)
		n := len(args)
		area = fmt.Sprintf(" AND %s <= $%d",
			fmt.Sprintf(topicHaversineSQL, fmt.Sprintf("$%d", n-2), fmt.Sprintf("$%d", n-2), fmt.Sprintf("$%d", n-1)), n)
	}
	// 到 area 为止，这条查询自己的占位符就用到这么多；下面的 regionSQL 会在此基础上
	// 继续往后编号并追加参数。记下来给下面那条「天数」查询用 —— 它没有区域条件，
	// 不能把这份（已被追加过的）args 整个传进去。
	baseArgs := len(args)
	q := `SELECT p.id, p.src_pk, COALESCE(p.name,''), COALESCE(p.lat,0), COALESCE(p.lon,0),
			COALESCE(p.city,''),
			COALESCE((SELECT a.name FROM visits v2 LEFT JOIN activities a ON a.id=v2.activity_id
				WHERE v2.place_id=p.id AND v2.activity_id IS NOT NULL
				GROUP BY a.name ORDER BY count(*) DESC LIMIT 1),''),
			COALESCE((SELECT a.color FROM visits v2 LEFT JOIN activities a ON a.id=v2.activity_id
				WHERE v2.place_id=p.id AND v2.activity_id IS NOT NULL
				GROUP BY a.color ORDER BY count(*) DESC LIMIT 1),''),
			count(*), COALESCE(sum(LEAST(COALESCE(v.duration_min,0), 10080)),0), max(v.arrival)
		FROM places p JOIN visits v ON v.place_id = p.id
		WHERE p.dataset_id=$1 AND v.arrival >= $2 AND v.arrival <= $3` + area +
		regionSQL("p", regions, exclude, &args) + `
		GROUP BY p.id ORDER BY count(*) DESC, p.id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	defer rows.Close()
	var points []MapPoint
	cities := map[string]bool{}
	totalVisits := 0
	var totalDwell int64
	for rows.Next() {
		var m MapPoint
		var last time.Time
		if err := rows.Scan(&m.ID, &m.SrcPK, &m.Name, &m.Lat, &m.Lon, &m.City,
			&m.Act, &m.Color, &m.Count, &m.Dwell, &last); err != nil {
			return nil, 0, 0, 0, err
		}
		m.LastAt = last.Local().Format("2006-01-02")
		points = append(points, m)
		totalVisits += m.Count
		totalDwell += m.Dwell
		if m.City != "" {
			cities[m.City] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, 0, err
	}

	// 天数：按本地日期去重（专题可能只覆盖几天，直接在 SQL 里算更准）。
	//
	// 参数必须按**这条 SQL 真正用到的占位符**来凑：$1..$3 固定，area 用 $4..$6。
	// 以前这里直接把上面那条 SQL 的 args 整个传进来，而它已经被 regionSQL 追加过区域
	// 参数 —— 这条 SQL 当时又没有区域条件，参数就比占位符多，pgx 直接报
	// 「expected 3 arguments, got 4」。而且专题列表的统计会把这个错误吞掉，
	// 表现成「专题内容全变 0、点进去才 500」，很难查。
	days := 0
	dargs := append([]any{}, args[:baseArgs]...)
	daysRegion := regionSQL("p", regions, exclude, &dargs)
	if err := s.db.QueryRowContext(ctx, `SELECT count(DISTINCT (v.arrival AT TIME ZONE 'Asia/Shanghai')::date)
		FROM visits v JOIN places p ON p.id = v.place_id
		WHERE p.dataset_id=$1 AND v.arrival >= $2 AND v.arrival <= $3`+area+daysRegion, dargs...).Scan(&days); err != nil {
		return nil, 0, 0, 0, err
	}
	return points, totalVisits, days, len(cities), nil
}

// fillTopicStats 给专题填上内容统计（列表页展示用）。
func (s *Server) fillTopicStats(ctx context.Context, t *Topic, datasetID int64, regions []string, exclude bool) {
	points, visits, days, cities, err := s.topicContent(ctx, t, datasetID, regions, exclude)
	if err != nil {
		// 以前这里直接 return：专题列表会静默显示成「0 个地点」，看不出是出错了。
		log.Printf("专题 #%d「%s」统计失败: %v", t.ID, t.Title, err)
		return
	}
	t.PlaceCount, t.VisitCount, t.DayCount, t.CityCount = len(points), visits, days, cities
	for _, p := range points {
		t.DwellMin += p.Dwell
	}
}

// parseTopicForm 解析后台表单。日期按本地时区补成当天 00:00 / 23:59:59，
// 这样「9月1日到9月3日」能把 3 号整天的记录都收进来。
func parseTopicForm(r *http.Request) (Topic, error) {
	var t Topic
	t.Title = strings.TrimSpace(r.PostFormValue("title"))
	t.Subtitle = strings.TrimSpace(r.PostFormValue("subtitle"))
	t.Description = strings.TrimSpace(r.PostFormValue("description"))
	if t.Title == "" {
		return t, fmt.Errorf("请填写专题标题")
	}
	startRaw := strings.TrimSpace(r.PostFormValue("start_at"))
	start, ok := parseTopicTime(startRaw)
	if !ok {
		return t, fmt.Errorf("请填写有效的开始时间")
	}
	endRaw := strings.TrimSpace(r.PostFormValue("end_at"))
	end, ok := parseTopicTime(endRaw)
	if !ok {
		return t, fmt.Errorf("请填写有效的结束时间")
	}
	// 只给了日期（没给时刻）时把结束补到当天最后一刻，不然那天整天的记录会被漏掉
	if len(endRaw) <= len(topicTimeLayout) {
		end = end.Add(24*time.Hour - time.Second)
	}
	if end.Before(start) {
		return t, fmt.Errorf("结束时间不能早于开始时间")
	}
	t.StartAt, t.EndAt = start, end
	t.CenterLat, _ = strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("center_lat")), 64)
	t.CenterLon, _ = strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("center_lon")), 64)
	t.RadiusKm, _ = strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("radius_km")), 64)
	if t.RadiusKm < 0 {
		t.RadiusKm = 0
	}
	if t.RadiusKm > 0 && t.CenterLat == 0 && t.CenterLon == 0 {
		return t, fmt.Errorf("限定区域时要填中心点坐标")
	}
	t.ShowFog = r.PostFormValue("show_fog") == "on"
	t.Enabled = r.PostFormValue("enabled") == "on"
	t.SortOrder = atoiOr(r.PostFormValue("sort_order"), 0)
	t.StartDate, t.EndDate = start.Format(topicDateTimeLayout), t.EndAt.Format(topicDateTimeLayout)
	return t, nil
}

// ---------- 后台：专题管理 ----------

func (s *Server) topicsAdminPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.pageBase(r, "admin", "专题管理")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Flatpickr = true
	d := TopicAdminData{Page: p}
	d.Flash, d.Err = r.URL.Query().Get("ok"), r.URL.Query().Get("err")
	ds, err := s.datasetFor(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if ds != nil {
		list, err := s.loadTopics(ctx, ds.ID, false)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for i := range list {
			s.fillTopicStats(ctx, &list[i], ds.ID, nil, false) // 后台管理页：站长看全部
		}
		d.Topics = list
		// 编辑模式：?edit=<id>
		if id := atoiOr(r.URL.Query().Get("edit"), 0); id > 0 {
			for _, t := range list {
				if t.ID == int64(id) {
					d.Form = t
				}
			}
		}
	}
	if d.Form.Title == "" {
		// 新建时给个合理默认：最近 30 天
		now := time.Now()
		d.Form.StartDate = now.AddDate(0, 0, -30).Format(topicDateTimeLayout)
		d.Form.EndDate = now.Format(topicDateTimeLayout)
		d.Form.ShowFog, d.Form.Enabled = true, true
		d.IsNew = true
	}
	s.render(w, "admin/topics", d)
}

func (s *Server) adminTopicSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectTopics(w, r, "err", "还没有可编辑的数据集")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	t, err := parseTopicForm(r)
	if err != nil {
		redirectTopics(w, r, "err", err.Error())
		return
	}
	id := atoiOr(r.PostFormValue("id"), 0)
	if id > 0 {
		if _, err := s.db.ExecContext(ctx, `UPDATE topics SET title=$3, subtitle=$4, description=$5,
			start_at=$6, end_at=$7, center_lat=$8, center_lon=$9, radius_km=$10,
			show_fog=$11, enabled=$12, sort_order=$13
			WHERE id=$1 AND dataset_id=$2`,
			id, ds.ID, t.Title, t.Subtitle, t.Description, t.StartAt, t.EndAt,
			t.CenterLat, t.CenterLon, t.RadiusKm, t.ShowFog, t.Enabled, t.SortOrder); err != nil {
			s.serverError(w, r, err)
			return
		}
		redirectTopics(w, r, "ok", "专题已更新")
		return
	}
	var newID int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO topics
		(user_id, dataset_id, title, subtitle, description, start_at, end_at,
		 center_lat, center_lon, radius_km, show_fog, enabled, sort_order)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		u.ID, ds.ID, t.Title, t.Subtitle, t.Description, t.StartAt, t.EndAt,
		t.CenterLat, t.CenterLon, t.RadiusKm, t.ShowFog, t.Enabled, t.SortOrder).Scan(&newID); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirectTopics(w, r, "ok", "专题已创建")
}

func (s *Server) adminTopicDelete(w http.ResponseWriter, r *http.Request) {
	s.topicMutate(w, r, func(ctx context.Context, dsID int64, id int) error {
		_, err := s.db.ExecContext(ctx, `DELETE FROM topics WHERE id=$1 AND dataset_id=$2`, id, dsID)
		return err
	}, "专题已删除")
}

func (s *Server) adminTopicToggle(w http.ResponseWriter, r *http.Request) {
	s.topicMutate(w, r, func(ctx context.Context, dsID int64, id int) error {
		_, err := s.db.ExecContext(ctx, `UPDATE topics SET enabled = NOT enabled WHERE id=$1 AND dataset_id=$2`, id, dsID)
		return err
	}, "展示状态已切换")
}

// topicMutate 收敛删除 / 开关两个动作里重复的取参与错误处理。
func (s *Server) topicMutate(w http.ResponseWriter, r *http.Request, fn func(context.Context, int64, int) error, okMsg string) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectTopics(w, r, "err", "还没有可编辑的数据集")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	id := atoiOr(r.PostFormValue("id"), 0)
	if id <= 0 {
		redirectTopics(w, r, "err", "专题标识无效")
		return
	}
	if err := fn(ctx, ds.ID, id); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirectTopics(w, r, "ok", okMsg)
}

// ---------- 前台：专题列表与详情 ----------

func (s *Server) topicsListPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.pageBase(r, "topics", "专题")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := TopicListData{Page: p}
	if p.Dataset != nil {
		list, err := s.loadTopics(ctx, p.Dataset.ID, !p.IsAdmin())
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		regions, exclude := visitorRegions(p)
		for i := range list {
			s.fillTopicStats(ctx, &list[i], p.Dataset.ID, regions, exclude)
		}
		d.Topics = list
	}
	s.render(w, "topics", d)
}

func (s *Server) topicPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.pageBase(r, "topics", "专题")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.HasMap = true
	id := atoiOr(r.PathValue("id"), 0)
	if id <= 0 || p.Dataset == nil {
		http.NotFound(w, r)
		return
	}
	var t Topic
	err = s.db.QueryRowContext(ctx, `SELECT id, title, subtitle, description, start_at, end_at,
		center_lat, center_lon, radius_km, show_fog, enabled, sort_order
		FROM topics WHERE id=$1 AND dataset_id=$2`, id, p.Dataset.ID).
		Scan(&t.ID, &t.Title, &t.Subtitle, &t.Description, &t.StartAt, &t.EndAt,
			&t.CenterLat, &t.CenterLon, &t.RadiusKm, &t.ShowFog, &t.Enabled, &t.SortOrder)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// 未对外展示的专题只有站长能看
	if !t.Enabled && !p.IsAdmin() {
		http.NotFound(w, r)
		return
	}
	t.StartDate = t.StartAt.Local().Format(topicDateTimeLayout)
	t.EndDate = t.EndAt.Local().Format(topicDateTimeLayout)
	t.RangeLabel = topicRangeLabel(t.StartAt, t.EndAt)

	regions, exclude := visitorRegions(p)
	points, visits, days, cities, err := s.topicContent(ctx, &t, p.Dataset.ID, regions, exclude)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	t.PlaceCount, t.VisitCount, t.DayCount, t.CityCount = len(points), visits, days, cities
	for _, mp := range points {
		t.DwellMin += mp.Dwell
	}
	p.Title = t.Title

	// 站点设置里的「备注对外可见」同样适用于专题页
	if p.IsAdmin() || p.Settings.NotesPublic {
		s.applyNotesToPoints(ctx, p.Dataset.ID, points)
	}

	js, err := json.Marshal(points)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	fitCircle, fitBBox := fitParams(t, points)
	d := TopicData{
		Page: p, Topic: t, Points: points, IsAdmin: p.IsAdmin(),
		PointsJSON: template.JS(js), FitCircleJSON: fitCircle, FitBBoxJSON: fitBBox,
	}
	s.render(w, "topic", d)
}

// applyNotesToPoints 把别名/备注贴到地图点上（专题页的迷你地点列表也用）。
func (s *Server) applyNotesToPoints(ctx context.Context, datasetID int64, points []MapPoint) {
	ns, err := s.notes.All(ctx, datasetID)
	if err != nil || len(ns) == 0 {
		return
	}
	for i := range points {
		if n, ok := ns[points[i].SrcPK]; ok {
			if n.Alias != "" {
				points[i].Name = n.Alias
			}
			points[i].Note = n.Content
		}
	}
}

// fitParams 生成把地图视野框到专题范围的脚本参数：限定区域时用圆心 + 半径，
// 否则用地点包围盒——两者都能保证「区域内的迷雾」正好落在视野里。
func fitParams(t Topic, points []MapPoint) (circle, bbox template.JS) {
	if t.HasArea() {
		b, _ := json.Marshal(map[string]any{
			"lat": t.CenterLat, "lon": t.CenterLon, "radiusKm": t.RadiusKm,
		})
		return template.JS(b), template.JS("null")
	}
	if len(points) == 0 {
		return template.JS("null"), template.JS("null")
	}
	minLat, maxLat := points[0].Lat, points[0].Lat
	minLon, maxLon := points[0].Lon, points[0].Lon
	for _, p := range points {
		minLat, maxLat = math.Min(minLat, p.Lat), math.Max(maxLat, p.Lat)
		minLon, maxLon = math.Min(minLon, p.Lon), math.Max(maxLon, p.Lon)
	}
	b, _ := json.Marshal(map[string]any{
		"minLat": minLat, "maxLat": maxLat, "minLon": minLon, "maxLon": maxLon,
	})
	return template.JS("null"), template.JS(b)
}

// redirectTopics 把结果带回专题管理页（保留可能的 ?edit= 上下文由页面自己处理）。
func redirectTopics(w http.ResponseWriter, r *http.Request, kind, msg string) {
	back := r.Referer()
	if !strings.Contains(back, "/admin/topics") {
		back = "/admin/topics"
	}
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	http.Redirect(w, r, back+sep+kind+"="+urlQueryEscape(msg), http.StatusSeeOther)
}
