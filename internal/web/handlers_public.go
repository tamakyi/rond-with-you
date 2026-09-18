package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"rond-with-you/internal/geo"
	"rond-with-you/internal/setting"
	"rond-with-you/internal/stats"
	"rond-with-you/internal/votes"
)

const (
	placesPerPage = 30
	visitsPerPage = 180
	mapPointLimit = 4000
)

// ---------- 公共上下文 ----------

func (s *Server) pageBase(r *http.Request, nav, title string) (Page, error) {
	ctx := r.Context()
	p := Page{Nav: nav, Title: title, User: userFrom(ctx), PageNo: 1, AssetV: assetVersion,
		TrackScript: trackTag(s.cfg.TrackScript)}
	sets, err := s.sets.Load(ctx)
	if err != nil {
		return p, err
	}
	p.Settings = sets
	ds, err := s.datasetFor(ctx)
	if err != nil {
		return p, err
	}
	p.Dataset, p.HasData = ds, ds != nil
	if ds == nil {
		return p, nil
	}
	// 有专题才在导航里露出入口（含未对外展示的：站长要能从导航进管理页）
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM topics WHERE dataset_id=$1)`, ds.ID).Scan(&p.HasTopics); err != nil {
		return p, err
	}

	q := r.URL.Query()
	// 「家 / 工作」默认隐藏，只有已登录的站长带 hw=1 才放行；
	// 匿名访客手工拼参数也会被忽略。
	p.ShowHomeWork = p.IsAdmin() && q.Get("hw") == "1"
	p.From, p.To = q.Get("from"), q.Get("to")
	p.ActivityIDs = parseInt64s(q["act"])
	p.City = q.Get("city")
	p.TagID = int64(atoiOr(q.Get("tag"), 0))
	p.Keyword = strings.TrimSpace(q.Get("q"))
	p.MinDwell = atoiOr(q.Get("min"), 0)
	p.Sort = q.Get("sort")
	if v := atoiOr(q.Get("verdict"), 0); v == 1 || v == -1 {
		p.Verdict = v
	}
	p.PageNo = atoiOr(q.Get("page"), 1)
	if p.PageNo < 1 {
		p.PageNo = 1
	}

	if p.Activities, err = s.q.Activities(ctx, ds.ID); err != nil {
		return p, err
	}
	if p.TagOptions, err = s.q.Tags(ctx, ds.ID); err != nil {
		return p, err
	}
	// 城市筛选下拉框对访客同样按区域限制：黑名单城市直接不出现在列表里，
	// 而不是点了之后才显示空结果。站长始终看全量。
	regions, exclude := p.Settings.Regions, p.Settings.RegionBlacklist()
	if p.IsAdmin() {
		regions = nil
	}
	if p.CityList, err = s.q.Cities(ctx, ds.ID, regions, exclude); err != nil {
		return p, err
	}
	p.RangeLabel = rangeLabel(p.From, p.To)
	p.FilterBase = r.URL.Path
	p.Presets = buildPresets(p.From, p.To)
	return p, nil
}

// buildPresets 生成筛选器上的快捷时间范围。
func buildPresets(from, to string) []RangePreset {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	f := func(d time.Time) string { return d.Format("2006-01-02") }
	defs := []struct{ label, from, to string }{
		{"全部", "", ""},
		{"近 7 天", f(today.AddDate(0, 0, -6)), f(today)},
		{"近 30 天", f(today.AddDate(0, 0, -29)), f(today)},
		{"近 90 天", f(today.AddDate(0, 0, -89)), f(today)},
		{"今年", f(time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.Local)), f(today)},
		{"去年", f(time.Date(now.Year()-1, 1, 1, 0, 0, 0, 0, time.Local)), f(time.Date(now.Year()-1, 12, 31, 0, 0, 0, 0, time.Local))},
	}
	out := make([]RangePreset, 0, len(defs))
	for _, d := range defs {
		out = append(out, RangePreset{Label: d.label, From: d.from, To: d.to, Active: from == d.from && to == d.to})
	}
	return out
}

func rangeLabel(from, to string) string {
	switch {
	case from == "" && to == "":
		return "全部时间"
	case from != "" && to == "":
		return from + " 起"
	case from == "" && to != "":
		return "至 " + to
	default:
		return from + " ~ " + to
	}
}

// applyNotes 把别名与备注贴到地点上。别名对外替代原名；备注是否给访客看
// 取决于「备注公开」开关，站长自己永远可见。
// 展示用的备注是「rond 备份里的备注 + 站长手写备注」拼起来的。
func (s *Server) applyNotes(ctx context.Context, datasetID int64, places []stats.Place, showNote bool) {
	if len(places) == 0 {
		return
	}
	ns, err := s.notes.All(ctx, datasetID)
	if err != nil {
		return
	}
	for i := range places {
		n, ok := ns[places[i].SrcPK]
		if !ok {
			continue
		}
		if n.Alias != "" {
			places[i].Alias = n.Alias
			places[i].Name = n.Alias
		}
		if showNote {
			places[i].Note = joinNote(n.RondNote, n.Content)
		}
	}
}

// joinNote 拼接展示用备注：rond 备份里的备注在前，手写备注在后。
func joinNote(rond, hand string) string {
	rond, hand = strings.TrimSpace(rond), strings.TrimSpace(hand)
	switch {
	case rond == "":
		return hand
	case hand == "":
		return rond
	}
	return rond + "\n\n" + hand
}

// attachVotes 把「推荐 / 踩雷」的站长结论与访客票数贴到地点上（按 src_pk 关联）。
func (s *Server) attachVotes(ctx context.Context, datasetID int64, places []stats.Place) {
	if len(places) == 0 {
		return
	}
	verdicts, err := s.votes.Verdicts(ctx, datasetID)
	if err != nil {
		return
	}
	tallies, err := s.votes.Tallies(ctx)
	if err != nil {
		return
	}
	for i := range places {
		pk := places[i].SrcPK
		places[i].Verdict = verdicts[pk]
		if t, ok := tallies[pk]; ok {
			places[i].Up, places[i].Down = t.Up, t.Down
		}
	}
}

// filterOf 把页面状态转成查询条件，并叠加站点级隐私设置。
func (s *Server) filterOf(p Page) stats.Filter {
	f := stats.Filter{
		DatasetID:   p.Dataset.ID,
		ActivityIDs: p.ActivityIDs,
		City:        p.City,
		TagID:       p.TagID,
		Keyword:     p.Keyword,
		MinDwell:    p.MinDwell,
		ExcludeHome: p.Settings.HideHome && !p.ShowHomeWork,
		ExcludeWork: p.Settings.HideWork && !p.ShowHomeWork,
		Verdict:     p.Verdict,
	}
	if p.Settings.MinDwellMin > f.MinDwell {
		f.MinDwell = p.Settings.MinDwellMin
	}
	if t, ok := parseDate(p.From); ok {
		f.From = &t
	}
	if t, ok := parseDate(p.To); ok {
		// 结束日期包含当天，按次日 0 点开区间处理
		end := t.AddDate(0, 0, 1)
		f.To = &end
	}
	// 区域限制只对访客生效：站长自己要看全部数据，否则没法核对
	if regions, exclude := visitorRegions(p); len(regions) > 0 {
		f.Regions, f.RegionsExclude = regions, exclude
	}
	return f
}

// regionHidden 报告该地点是否因区域设置对当前访客不可见。
// 页面级查询走 filterOf 的 SQL 条件，但详情页是按 ID 直取的，必须单独判一次，
// 否则访客拿到链接就能看到被隐藏区域的地点。
func regionHidden(sets setting.Settings, isAdmin bool, place stats.Place) bool {
	if isAdmin || len(sets.Regions) == 0 {
		return false
	}
	hit := false
	for _, r := range sets.Regions {
		if r == place.Province || r == place.City || r == place.District {
			hit = true
			break
		}
	}
	if sets.RegionBlacklist() {
		return hit
	}
	return !hit
}

func parseDate(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02", "2006-1-2", "2006/01/02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ---------- 主页 ----------

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "home", "足迹主页")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.HasMap = true
	if p.Dataset == nil {
		s.render(w, "home", HomeData{Page: p})
		return
	}
	// 访客默认页面：把访客直接送到站长指定的页面。
	// 三种情况刻意不跳：站长自己访问、没配、或目标页面没对访客开放（那时 VisitorHomePath 为空）；
	// 另外数据还没导入时不跳——让访客留在主页看到引导，比落到一张空地图有用。
	if !p.IsAdmin() {
		if dest := p.Settings.VisitorHomePath(); dest != "" {
			http.Redirect(w, r, dest, http.StatusFound)
			return
		}
	}
	ctx := r.Context()
	f := s.filterOf(p)
	d := HomeData{Page: p}

	if p.Settings.Has("overview") {
		if d.Overview, err = s.q.Overview(ctx, f); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if p.Settings.Has("types") {
		if d.ActivityStats, err = s.q.ActivityStats(ctx, f); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if p.Settings.Has("cities") {
		if d.CityStats, err = s.q.CityStats(ctx, f, 10); err != nil {
			s.serverError(w, r, err)
			return
		}
		if d.Monthly, err = s.monthBars(ctx, f); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if p.Settings.Has("heatmap") {
		cal, err := s.q.Calendar(ctx, f)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if d.Overview.FirstAt != nil && d.Overview.LastAt != nil {
			d.Calendar = BuildCalendar(*d.Overview.FirstAt, *d.Overview.LastAt, cal)
		} else if f.From != nil && f.To != nil {
			d.Calendar = BuildCalendar(*f.From, f.To.AddDate(0, 0, -1), cal)
		}
	}
	if p.Settings.Has("transport") {
		if d.Transports, err = s.q.TransportStats(ctx, f); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if p.Settings.Has("tags") {
		if d.Tags, err = s.q.TagStats(ctx, f); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if p.Settings.Has("weather") {
		if d.Weather, err = s.q.WeatherStats(ctx, f); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if p.Settings.Has("timeline") {
		if d.Recent, err = s.q.RecentVisits(ctx, f, 12); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if p.Settings.Has("map") {
		places, err := s.q.MapPlaces(ctx, f, 300)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		d.Places = places
		for _, pl := range places {
			d.Points = append(d.Points, toPoint(pl))
		}
	}
	d.MaxActivity = maxOf(d.ActivityStats, func(a stats.ActivityStat) int { return a.Count })
	d.MaxCity = maxOf(d.CityStats, func(c stats.CityStat) int { return c.Count })
	d.MaxTransport = maxOf(d.Transports, func(t stats.TransportStat) int { return t.Count })
	d.MaxTag = maxOf(d.Tags, func(t stats.TagStat) int { return t.Count })
	s.render(w, "home", d)
}

func (s *Server) monthBars(ctx context.Context, f stats.Filter) ([]MonthBar, error) {
	b, err := s.q.Monthly(ctx, f)
	if err != nil {
		return nil, err
	}
	return BuildMonthly(b), nil
}

func toPoint(p stats.Place) MapPoint {
	mp := MapPoint{
		ID: p.ID, Lat: p.Lat, Lon: p.Lon, Name: p.Name, City: p.City,
		Act: p.TopActivity, Color: p.TopColor, Icon: p.TopIcon,
		Count: p.VisitCount, Dwell: p.DwellMinutes, Home: p.IsHome, Work: p.IsWork,
		SrcPK: p.SrcPK, Verdict: p.Verdict, Up: p.Up, Down: p.Down, Note: p.Note,
	}
	if p.LastAt != nil {
		mp.LastAt = p.LastAt.Local().Format("2006-01-02")
	}
	if p.FirstAt != nil {
		mp.First = p.FirstAt.Unix()
	}
	return mp
}

// ---------- 地图 ----------

func (s *Server) mapPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "map", "足迹地图")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.HasMap = true
	d := MapData{Page: p}
	if p.Dataset == nil {
		s.render(w, "map", d)
		return
	}
	places, err := s.q.MapPlaces(r.Context(), s.filterOf(p), mapPointLimit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, pl := range places {
		d.Points = append(d.Points, toPoint(pl))
	}
	d.Legend = buildLegend(d.Points)
	s.render(w, "map", d)
}

// buildLegend 按当前结果里的类型聚合出图例。
func buildLegend(points []MapPoint) []LegendItem {
	idx := map[string]int{}
	var out []LegendItem
	for _, p := range points {
		name := p.Act
		if name == "" {
			name = "未分类"
		}
		if i, ok := idx[name]; ok {
			out[i].Count++
			continue
		}
		idx[name] = len(out)
		out = append(out, LegendItem{Name: name, Color: p.Color, Icon: p.Icon, Count: 1})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

func (s *Server) apiPoints(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "map", "")
	if err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	type resp struct {
		Points []MapPoint `json:"points"`
		Total  int        `json:"total"`
	}
	out := resp{Points: []MapPoint{}}
	if p.Dataset == nil {
		s.json(w, out)
		return
	}
	places, err := s.q.MapPlaces(r.Context(), s.filterOf(p), mapPointLimit)
	if err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	s.applyNotes(r.Context(), p.Dataset.ID, places, p.IsAdmin() || p.Settings.NotesPublic)
	if p.IsAdmin() || p.Settings.MarksPublic {
		s.attachVotes(r.Context(), p.Dataset.ID, places)
	}
	for _, pl := range places {
		out.Points = append(out.Points, toPoint(pl))
	}
	out.Total = len(out.Points)
	s.json(w, out)
}

// ---------- 地点库 ----------

func (s *Server) placesPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "places", "地点库")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := PlacesData{Page: p}
	if p.Dataset == nil {
		s.render(w, "places", d)
		return
	}
	places, total, err := s.q.Places(r.Context(), s.filterOf(p), p.Sort, placesPerPage, (p.PageNo-1)*placesPerPage)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.applyNotes(r.Context(), p.Dataset.ID, places, p.IsAdmin() || p.Settings.NotesPublic)
	if p.IsAdmin() || p.Settings.MarksPublic {
		s.attachVotes(r.Context(), p.Dataset.ID, places)
	}
	d.Places = places
	d.Total = total
	d.PageCount = (total + placesPerPage - 1) / placesPerPage
	s.render(w, "places", d)
}

func (s *Server) placePage(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "places", "地点详情")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.HasMap = true
	if p.Dataset == nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	place, err := s.q.Place(r.Context(), p.Dataset.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if regionHidden(p.Settings, p.IsAdmin(), place) {
		s.placeHiddenPage(w)
		return
	}
	ps := []stats.Place{place}
	s.applyNotes(r.Context(), p.Dataset.ID, ps, p.IsAdmin() || p.Settings.NotesPublic)
	place = ps[0]
	visits, err := s.q.VisitsByPlace(r.Context(), p.Dataset.ID, id, 200)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := PlaceData{Page: p, Place: place, Visits: visits, Points: []MapPoint{toPoint(place)}}
	d.Title = place.Name
	// 备注保存后跳回本页并带上 ok，只有站长能看到这句提示
	if p.IsAdmin() {
		d.Flash = r.URL.Query().Get("ok")
	}
	// 编辑框里编辑的是手写备注，rond 带出来的那一块只读展示
	if ns, err := s.notes.All(r.Context(), p.Dataset.ID); err == nil {
		if n, ok := ns[place.SrcPK]; ok {
			d.MyNote, d.RondNote = n.Content, n.RondNote
		}
	}
	d.Verdict, _ = s.votes.Verdict(r.Context(), p.Dataset.ID, place.SrcPK)
	d.Votes, _ = s.votes.Tally(r.Context(), place.SrcPK)
	d.MyVote = s.votes.Voter(r.Context(), place.SrcPK, s.voterKeyFromRequest(r))
	// 图片：访客也要看得到，所以不分管理员
	if imgs, err := s.placeImagesOf(r.Context(), p.Dataset.ID, place.SrcPK); err == nil {
		d.Images = imgs
	}
	if p.IsAdmin() {
		if backend, err := s.imageBackend(r.Context()); err == nil {
			d.ImageBackend = backend
			d.ImageReady = true
		} else {
			d.ImageBackend = "配置有误"
			d.ImageReady = false
		}
	}
	d.ImageMaxPer = imageMaxPer
	s.render(w, "place", d)
}

// ---------- 时间线 ----------

func (s *Server) timelinePage(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "timeline", "时间线")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := TimelineData{Page: p}
	if p.Dataset == nil {
		s.render(w, "timeline", d)
		return
	}
	// 排序跟着 ?sort= 走：空 = 最新在前，asc = 最早在前（按天分组时天序也会跟着反过来）
	order := "desc"
	if p.Sort == "asc" {
		order = "asc"
	}
	visits, total, err := s.q.Visits(r.Context(), s.filterOf(p), visitsPerPage, (p.PageNo-1)*visitsPerPage, order)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Total = total
	d.PageCount = (total + visitsPerPage - 1) / visitsPerPage
	d.Days = groupByDay(visits)
	s.render(w, "timeline", d)
}

type TimelineDay struct {
	Date  string
	Label string
	Items []stats.VisitRow
	Dwell int64
}

func groupByDay(visits []stats.VisitRow) []TimelineDay {
	var days []TimelineDay
	idx := map[string]int{}
	for _, v := range visits {
		key := v.Arrival.Local().Format("2006-01-02")
		i, ok := idx[key]
		if !ok {
			days = append(days, TimelineDay{Date: key, Label: dayLabel(v.Arrival)})
			i = len(days) - 1
			idx[key] = i
		}
		days[i].Items = append(days[i].Items, v)
		days[i].Dwell += int64(v.DurationMin)
	}
	return days
}

// ---------- 统计 ----------

func (s *Server) statsPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "stats", "数据统计")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := StatsData{Page: p}
	if p.Dataset == nil {
		s.render(w, "stats", d)
		return
	}
	ctx := r.Context()
	f := s.filterOf(p)
	// 这一屏有十几个互不依赖的查询，串行跑就是它们的耗时之和；并发之后约等于最慢的那个。
	// 每个任务只写自己的字段（d 的不同成员地址不同），互不干扰。
	var hourly []stats.Bucket
	if err := s.runJobs(ctx,
		pageJob{"overview", func(ctx context.Context) (err error) {
			d.Overview, err = s.q.Overview(ctx, f)
			return
		}},
		pageJob{"activities", func(ctx context.Context) (err error) {
			d.ActivityStats, err = s.q.ActivityStats(ctx, f)
			return
		}},
		pageJob{"cities", func(ctx context.Context) (err error) {
			d.CityStats, err = s.q.CityStats(ctx, f, 20)
			return
		}},
		pageJob{"provinces", func(ctx context.Context) (err error) {
			d.ProvinceStats, err = s.q.ProvinceStats(ctx, f)
			return
		}},
		pageJob{"monthly", func(ctx context.Context) (err error) {
			d.Monthly, err = s.monthBars(ctx, f)
			return
		}},
		pageJob{"transports", func(ctx context.Context) (err error) {
			d.Transports, err = s.q.TransportStats(ctx, f)
			return
		}},
		pageJob{"tags", func(ctx context.Context) (err error) {
			d.Tags, err = s.q.TagStats(ctx, f)
			return
		}},
		pageJob{"weather", func(ctx context.Context) (err error) {
			d.Weather, err = s.q.WeatherStats(ctx, f)
			return
		}},
		pageJob{"temps", func(ctx context.Context) (err error) {
			var bs []stats.TempBucket
			if bs, err = s.q.HourlyTemp(ctx, f); err != nil {
				return
			}
			d.Temps = TempChart(bs)
			return
		}},
		pageJob{"hourly", func(ctx context.Context) (err error) {
			hourly, err = s.q.Hourly(ctx, f)
			return
		}},
		pageJob{"weekday", func(ctx context.Context) (err error) {
			d.Weekday, err = s.q.Weekday(ctx, f)
			return
		}},
		pageJob{"fog", func(ctx context.Context) error {
			d.Fog = s.fogCoverage(ctx, p.Dataset.ID, f.Regions, f.RegionsExclude, p.IsAdmin())
			return nil
		}},
		pageJob{"verdicts", func(ctx context.Context) error {
			if !p.IsAdmin() && !p.Settings.MarksPublic {
				return nil
			}
			var err error
			if d.VoteRec, d.VoteAvoid, d.VoteUp, d.VoteDown, err = s.voteTotals(ctx, p.Dataset.ID); err != nil {
				return err
			}
			if d.Recommended, err = s.verdictPlaces(ctx, p.Dataset.ID, votes.VerdictRecommend, 8); err != nil {
				return err
			}
			d.Avoided, err = s.verdictPlaces(ctx, p.Dataset.ID, votes.VerdictAvoid, 8)
			return err
		}},
	); err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Hourly = make([]int, 24)
	for _, b := range hourly {
		if h, err := strconv.Atoi(b.Key); err == nil && h >= 0 && h < 24 {
			d.Hourly[h] = b.Count
			if b.Count > d.MaxHourly {
				d.MaxHourly = b.Count
			}
		}
	}
	if d.Weekday, err = s.q.Weekday(ctx, f); err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, m := range d.Monthly {
		if m.Count > d.MaxMonthly {
			d.MaxMonthly = m.Count
		}
	}
	d.MaxActivity = maxOf(d.ActivityStats, func(a stats.ActivityStat) int { return a.Count })
	d.MaxCity = maxOf(d.CityStats, func(c stats.CityStat) int { return c.Count })
	d.MaxWeekday = maxOf(d.Weekday, func(b stats.Bucket) int { return b.Count })
	d.MaxTag = maxOf(d.Tags, func(t stats.TagStat) int { return t.Count })
	s.render(w, "stats", d)
}

// ---------- 年度报告 ----------

// reportYears 返回有到访记录的年份，新的在前。
func (s *Server) reportYears(ctx context.Context, datasetID int64) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT extract(year from arrival AT TIME ZONE '`+
		stats.TZ+`')::int FROM visits WHERE dataset_id=$1 ORDER BY 1 DESC`, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var y int
		if err := rows.Scan(&y); err != nil {
			return nil, err
		}
		out = append(out, y)
	}
	return out, rows.Err()
}

func (s *Server) reportPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "report", "年度报告")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := ReportData{Page: p}
	if p.Dataset == nil {
		s.render(w, "report", d)
		return
	}
	ctx := r.Context()
	years, err := s.reportYears(ctx, p.Dataset.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Years = years
	year := atoiOr(r.URL.Query().Get("y"), 0)
	known := false
	for _, y := range years {
		if y == year {
			known = true
			break
		}
	}
	if !known && len(years) > 0 {
		year = years[0]
	}
	d.Year = year
	if year <= 0 {
		s.render(w, "report", d)
		return
	}

	f := s.filterOf(p)
	from := time.Date(year, 1, 1, 0, 0, 0, 0, time.Local)
	to := from.AddDate(1, 0, 0)
	f.From, f.To = &from, &to

	if d.Overview, err = s.q.Overview(ctx, f); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.TopCities, err = s.q.CityStats(ctx, f, 8); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.Transports, err = s.q.TransportStats(ctx, f); err != nil {
		s.serverError(w, r, err)
		return
	}
	cargs := []any{p.Dataset.ID, from, to}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM places p
		WHERE p.dataset_id=$1 AND p.first_visit_at >= $2 AND p.first_visit_at < $3`+
		regionSQL("p", f.Regions, f.RegionsExclude, &cargs),
		cargs...).Scan(&d.NewPlaces); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.North, err = s.extremePlace(ctx, p.Dataset.ID, from, to, "DESC", f.Regions, f.RegionsExclude); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.South, err = s.extremePlace(ctx, p.Dataset.ID, from, to, "ASC", f.Regions, f.RegionsExclude); err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Fog = s.fogCoverage(ctx, p.Dataset.ID, f.Regions, f.RegionsExclude, p.IsAdmin())
	if d.Cities, err = s.firstArrivalCities(ctx, p.Dataset.ID, from, to, f.Regions, f.RegionsExclude); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, "report", d)
}

// regionSQL 生成访客的区域限制条件（作用在 places 别名 p 上）。
// 省 / 市 / 区任意层级命中即算命中，空列表返回空串。
// exclude 为 true 时是黑名单（命中即排除），false 时是白名单（命中才保留）。args 就地追加占位符。
// visitorRegions 返回对访客生效的区域限制（站长恒为空）。
// 公开查询一律走它——「漏套区域限制」这类 bug 已经在轨迹、专题、迷雾统计上各踩过一次了，
// 收敛到一个入口，新增查询时照抄一行就行。
func visitorRegions(p Page) ([]string, bool) {
	if p.IsAdmin() {
		return nil, false
	}
	return p.Settings.Regions, p.Settings.RegionBlacklist()
}

// regionSQL 生成区域限制条件（省 / 市 / 区任一命中即算命中）。alias 是 places 表的别名——
// 多数调用点是 p，行程轨迹要同时限制两端（pf / pt），所以不能写死。
func regionSQL(alias string, regions []string, exclude bool, args *[]any) string {
	if len(regions) == 0 {
		return ""
	}
	var conds []string
	for _, r := range regions {
		*args = append(*args, r)
		ph := fmt.Sprintf("$%d", len(*args))
		// 必须 COALESCE：district 常为 NULL，NULL = 'x' 得到 NULL，
		// 「不匹配」会变成未知值，黑名单里的 NOT (...) 也跟着变 NULL，整行被误删。
		conds = append(conds, "COALESCE("+alias+".province,'') = "+ph,
			"COALESCE("+alias+".city,'') = "+ph, "COALESCE("+alias+".district,'') = "+ph)
	}
	cond := "(" + strings.Join(conds, " OR ") + ")"
	if exclude {
		cond = "NOT " + cond
	}
	return " AND " + cond
}

// firstArrivalCities 返回「首次到达城市」里程碑：有史以来第一次到访落在
// [from,to) 区间内的城市，按首次到达日期升序。区域限制对访客同样生效。
func (s *Server) firstArrivalCities(ctx context.Context, datasetID int64, from, to time.Time, regions []string, exclude bool) ([]CityMilestone, error) {
	args := []any{datasetID, from, to}
	regSQL := regionSQL("p", regions, exclude, &args)
	q := `SELECT COALESCE(p.city,''), COALESCE(p.province,''), min(v.arrival)::date
		FROM visits v JOIN places p ON p.id = v.place_id
		WHERE v.dataset_id = $1 AND p.city IS NOT NULL AND p.city <> ''` + regSQL + `
		GROUP BY p.city, p.province
		HAVING min(v.arrival) >= $2 AND min(v.arrival) < $3
		ORDER BY min(v.arrival) ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CityMilestone
	for rows.Next() {
		var cm CityMilestone
		var first sql.NullTime
		if err := rows.Scan(&cm.City, &cm.Province, &first); err != nil {
			return nil, err
		}
		if first.Valid {
			cm.First = first.Time.Local().Format("2006-01-02")
		}
		out = append(out, cm)
	}
	return out, rows.Err()
}

// extremePlace 取最北/最南的地点（年度内有到访记录的地点里按纬度取极值）。要带 region 限制：它会让地点名直接露给访客，
// 漏了的话黑名单区域的地点名照样看得到。
func (s *Server) extremePlace(ctx context.Context, datasetID int64, from, to time.Time, dir string,
	regions []string, exclude bool) (*stats.Place, error) {
	var p stats.Place
	args := []any{datasetID, from, to}
	q := `SELECT ` + stats.PlaceCols + ` FROM places p
		WHERE p.dataset_id=$1 AND p.lat IS NOT NULL
		AND EXISTS (SELECT 1 FROM visits v WHERE v.place_id=p.id AND v.arrival >= $2 AND v.arrival < $3)` +
		regionSQL("p", regions, exclude, &args) + `
		ORDER BY p.lat ` + dir + ` LIMIT 1`
	if err := stats.ScanPlace(s.db.QueryRowContext(ctx, q, args...), &p); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// ---------- 行程轨迹 API ----------

// CityAgg 是城市级聚合：按城市汇总到访与时长，并给出「首次到达」日期（里程碑用）。
type CityAgg struct {
	City       string  `json:"city"`
	Province   string  `json:"province"`
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
	Visits     int     `json:"visits"`
	Places     int     `json:"places"`
	DwellMin   int64   `json:"dwell"`
	FirstVisit string  `json:"first,omitempty"`
	LastVisit  string  `json:"last,omitempty"`
}

// apiCityAgg 返回城市级聚合，地图用来画按到访量加权的城市圈。
func (s *Server) apiCityAgg(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "map", "")
	if err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	out := struct {
		Cities []CityAgg `json:"cities"`
	}{Cities: []CityAgg{}}
	if p.Dataset == nil {
		s.json(w, out)
		return
	}
	// 城市聚合直接查 places，绕过了 filterOf，所以区域限制要自己挂一次，
	// 否则被隐藏区域的城市名和坐标照样会出现在地图上
	args := []any{p.Dataset.ID}
	regSQL := ""
	if !p.IsAdmin() {
		regSQL = regionSQL("p", p.Settings.Regions, p.Settings.RegionBlacklist(), &args)
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT COALESCE(p.city,''), COALESCE(p.province,''),
		AVG(p.lat)::float8, AVG(p.lon)::float8,
		COALESCE(sum(p.visit_count),0), count(*), COALESCE(sum(p.dwell_minutes),0),
		min(p.first_visit_at), max(p.last_visit_at)
		FROM places p
		JOIN visits v ON v.place_id = p.id AND v.dataset_id = p.dataset_id
		WHERE p.dataset_id = $1 AND p.city IS NOT NULL AND p.city <> '' AND p.lat IS NOT NULL`+regSQL+`
			AND NOT EXISTS (SELECT 1 FROM visits vh WHERE vh.place_id = p.id
				AND (vh.is_home OR vh.is_work))
		GROUP BY p.city, p.province
		ORDER BY 5 DESC`, args...)
	if err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var c CityAgg
		var first, last sql.NullTime
		if err := rows.Scan(&c.City, &c.Province, &c.Lat, &c.Lon, &c.Visits, &c.Places,
			&c.DwellMin, &first, &last); err != nil {
			s.json(w, map[string]string{"error": err.Error()})
			return
		}
		if first.Valid {
			c.FirstVisit = first.Time.Local().Format("2006-01-02")
		}
		if last.Valid {
			c.LastVisit = last.Time.Local().Format("2006-01-02")
		}
		out.Cities = append(out.Cities, c)
	}
	if err := rows.Err(); err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	s.json(w, out)
}

// apiTrack 返回当前筛选范围内的位移段（起终点坐标 + 交通方式），供地图画轨迹线。
// 坐标与地点一样是 GCJ-02，前端按底图坐标系统一投影。
func (s *Server) apiTrack(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "map", "")
	if err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	type TrackSeg struct {
		A     [2]float64 `json:"a"`
		B     [2]float64 `json:"b"`
		Mode  string     `json:"m,omitempty"`
		Color string     `json:"k,omitempty"`
		Km    float64    `json:"km,omitempty"`
	}
	out := struct {
		Segs  []TrackSeg  `json:"segs"`
		Modes []ModeCount `json:"modes"`
	}{Segs: []TrackSeg{}, Modes: []ModeCount{}}
	if p.Dataset == nil {
		s.json(w, out)
		return
	}
	f := s.filterOf(p)

	conds := []string{"m.dataset_id = $1"}
	args := []any{p.Dataset.ID}
	if f.From != nil {
		args = append(args, *f.From)
		conds = append(conds, fmt.Sprintf("m.started_at >= $%d", len(args)))
	}
	if f.To != nil {
		args = append(args, *f.To)
		conds = append(conds, fmt.Sprintf("m.started_at < $%d", len(args)))
	}
	var flags []string
	if f.ExcludeHome {
		flags = append(flags, "vf.is_home")
	}
	if f.ExcludeWork {
		flags = append(flags, "vf.is_work")
	}
	if len(flags) > 0 {
		conds = append(conds, `NOT EXISTS (
			SELECT 1 FROM visits vf WHERE vf.dataset_id = m.dataset_id
			AND (vf.src_pk = m.from_visit_src OR vf.src_pk = m.to_visit_src)
			AND (`+strings.Join(flags, " OR ")+`))`)
	}
	// 区域限制要套在**两端**上：只要有一端落在被隐藏的区域，整段轨迹都会暴露那个坐标，
	// 所以这种段直接不返回（只按起点过滤的话，终点在隐藏区域的线段照样画得出来）。
	regSQL := ""
	if len(f.Regions) > 0 {
		regSQL = regionSQL("pf", f.Regions, f.RegionsExclude, &args) +
			regionSQL("pt", f.Regions, f.RegionsExclude, &args)
	}
	q := `SELECT pf.lat, pf.lon, pt.lat, pt.lon,
		COALESCE(m.transport_name,''), COALESCE(m.transport_color,''), COALESCE(m.distance_km,0)
		FROM movements m
		JOIN places pf ON pf.id = m.from_place_id AND pf.lat IS NOT NULL
		JOIN places pt ON pt.id = m.to_place_id AND pt.lat IS NOT NULL
		WHERE ` + strings.Join(conds, " AND ") + regSQL + `
		ORDER BY m.started_at LIMIT 5000`
	modeTally := map[string]int{}
	modeColor := map[string]string{}
	rows, err := s.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var seg TrackSeg
		var color string
		if err := rows.Scan(&seg.A[0], &seg.A[1], &seg.B[0], &seg.B[1], &seg.Mode, &color, &seg.Km); err != nil {
			s.json(w, map[string]string{"error": err.Error()})
			return
		}
		// 没记录交通方式的段按「步行 / 其他」归到灰色，避免和默认青色混淆
		if seg.Mode == "" {
			seg.Mode, color = modeFallback, "gray"
		}
		if c, ok := appleColors[color]; ok {
			seg.Color = c
		} else if strings.HasPrefix(color, "#") {
			seg.Color = color
		}
		modeTally[seg.Mode]++
		modeColor[seg.Mode] = seg.Color
		out.Segs = append(out.Segs, seg)
	}
	if err := rows.Err(); err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	for name, n := range modeTally {
		out.Modes = append(out.Modes, ModeCount{Name: name, Count: n, Color: modeColor[name]})
	}
	sort.Slice(out.Modes, func(i, j int) bool { return out.Modes[i].Count > out.Modes[j].Count })
	s.json(w, out)
}

// GPXTrack 是给前端画真实路径用的一条轨迹。
type GPXTrack struct {
	ID       int64        `json:"id"`
	Name     string       `json:"name"`
	Points   [][2]float64 `json:"points"`
	LengthKm float64      `json:"km"`
	Started  string       `json:"started,omitempty"`
}

// apiGpx 返回已导入的 GPX 轨迹。库里存的是 WGS-84（GPX 标准），
// 底图是 GCJ-02 时按点转成火星坐标，保证和足迹点对齐。
func (s *Server) apiGpx(w http.ResponseWriter, r *http.Request) {
	crs := r.URL.Query().Get("crs")
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, name, points, length_km, started_at
		FROM gpx_tracks ORDER BY started_at DESC NULLS LAST, id DESC`)
	if err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := struct {
		Tracks []GPXTrack `json:"tracks"`
	}{Tracks: []GPXTrack{}}
	for rows.Next() {
		var t GPXTrack
		var raw []byte
		var started sql.NullTime
		if err := rows.Scan(&t.ID, &t.Name, &raw, &t.LengthKm, &started); err != nil {
			s.json(w, map[string]string{"error": err.Error()})
			return
		}
		var pts [][2]float64
		if err := json.Unmarshal(raw, &pts); err != nil {
			continue
		}
		if crs == "gcj02" {
			for i := range pts {
				lat, lon := geo.WGS84ToGCJ02(pts[i][0], pts[i][1])
				pts[i] = [2]float64{lat, lon}
			}
		}
		t.Points = pts
		if started.Valid {
			t.Started = started.Time.Local().Format("2006-01-02 15:04")
		}
		out.Tracks = append(out.Tracks, t)
	}
	if err := rows.Err(); err != nil {
		s.json(w, map[string]string{"error": err.Error()})
		return
	}
	s.json(w, out)
}
