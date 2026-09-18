package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"rond-with-you/internal/geo"
)

// ---------- 数据编辑：手动补录没带手机时的记录点，改完可导出回 rondbackup ----------

const editPerPage = 40

// editPage 渲染数据编辑页：地点、到访两个可搜索分页列表 + 新增表单。
func (s *Server) editPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.pageBase(r, "admin", "数据编辑")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.HasMap = true
	p.Flatpickr = true
	q := r.URL.Query()
	d := EditData{Page: p, Query: strings.TrimSpace(q.Get("q")), SourceCounts: map[string]int{}}
	d.VisitQuery = strings.TrimSpace(q.Get("vq"))
	d.MovementFilter = MovementFilter{
		Transport: strings.TrimSpace(q.Get("mt")),
		PlaceID:   int64(atoiOr(q.Get("mp"), 0)),
		From:      normalizeDate(q.Get("mfrom")),
		To:        normalizeDate(q.Get("mto")),
	}
	d.SuspectOnly = q.Get("sus") == "1"
	d.SourceFilter = strings.TrimSpace(q.Get("src"))
	if !validPlaceSource(d.SourceFilter) {
		d.SourceFilter = ""
	}
	d.PageNo = atoiOr(q.Get("page"), 1)
	d.OpenSrcPK = atoiOr(q.Get("open"), 0)
	d.VisitPageNo = atoiOr(q.Get("vpage"), 1)
	d.MovementPageNo = atoiOr(q.Get("mpage"), 1)
	d.Flash, d.Err = q.Get("ok"), q.Get("err")
	d.GeocodeReady = s.cfg.GeocodeKey != ""
	d.TransportColors = transportColorNames()
	ds, err := s.datasetFor(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if ds == nil {
		s.render(w, "admin/edit", d)
		return
	}
	// 导出 rondbackup 靠的是「基线库」（导入时留存的 Core Data 库底稿）。
	// 老数据集可能还没留，顺手补建一次——失败才算不可导出，页面给出提示。
	if _, err := s.ensureBaseline(ctx, ds.ID); err != nil {
		log.Printf("基线库不可用（导出 rondbackup 将失败）: %v", err)
	} else {
		d.CanExport = true
	}

	// 活动类型与标签
	if d.Activities, err = s.editOptions(ctx, `SELECT a.id, COALESCE(a.name,'')
		FROM activities a WHERE a.dataset_id=$1 ORDER BY a.id`, ds.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.Tags, err = s.editOptions(ctx, `SELECT t.id, COALESCE(t.name,'')
		FROM tags t WHERE t.dataset_id=$1 ORDER BY t.name`, ds.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	// 交通方式：rond 的 ZTRANSPORT 是全局列表，站点不单独维护，从行程里聚合
	if d.Transports, err = s.editTransports(ctx, ds.ID); err != nil {
		s.serverError(w, r, err)
		return
	}

	// 地点列表（搜索 + 分页）。「只看疑似 WGS-84」的条件用到留档别名 r，
	// 所以下面的 count 与列表都要带上 placeRawJoin。
	where, args := "p.dataset_id=$1", []any{ds.ID}
	if d.SuspectOnly {
		where += " AND " + suspectWGS
	}
	// 坐标来源筛选：none 是「没有留档」的历史行（含 2026-09 那批照片导入的点）
	if d.SourceFilter == placeSourceNone {
		where += " AND p.coord_source IS NULL"
	} else if d.SourceFilter != "" {
		args = append(args, d.SourceFilter)
		where += fmt.Sprintf(" AND p.coord_source = $%d", len(args))
	}
	if d.Query != "" {
		args = append(args, "%"+escapeLike(d.Query)+"%")
		n := len(args)
		where += fmt.Sprintf(" AND (COALESCE(p.name,'') ILIKE $%d OR COALESCE(p.city,'') ILIKE $%d OR COALESCE(p.district,'') ILIKE $%d)", n, n, n)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM places p`+placeRawJoin+` WHERE `+where, args...).Scan(&d.Total); err != nil {
		s.serverError(w, r, err)
		return
	}
	d.PageCount = max1((d.Total + editPerPage - 1) / editPerPage)
	// 行号按「当前筛选后的顺序」算：筛选一变顺序就变，所以不能拿全量排名跳页。
	// 筛选条件下找不到这一行（比如带着搜索词跳进来）就停在第 1 页，不猜。
	if d.OpenSrcPK > 0 {
		args = append(args, d.OpenSrcPK)
		var rowNo int
		// 比的是 src_pk（rond 编号，也是页面链接里那个 #号），不是库内主键 id
		err := s.db.QueryRowContext(ctx, `SELECT rn FROM (
			SELECT p.src_pk, row_number() OVER (ORDER BY p.visit_count DESC, p.id) AS rn
			FROM places p`+placeRawJoin+` WHERE `+where+`) t WHERE t.src_pk=$`+strconv.Itoa(len(args)),
			args...).Scan(&rowNo)
		args = args[:len(args)-1]
		if err == nil {
			d.PageNo = (rowNo-1)/editPerPage + 1
		} else if !errors.Is(err, sql.ErrNoRows) {
			s.serverError(w, r, err)
			return
		}
	}
	if d.PageNo < 1 {
		d.PageNo = 1
	}
	if d.PageNo > d.PageCount {
		d.PageNo = d.PageCount
	}
	// 已有类别去重：新建表单和每行编辑的下拉都用它（提交值仍是原始标识符）
	crows, err := s.db.QueryContext(ctx, `SELECT DISTINCT poi_category FROM places
		WHERE dataset_id=$1 AND COALESCE(poi_category,'')<>'' ORDER BY poi_category`, ds.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for crows.Next() {
		var c string
		if err := crows.Scan(&c); err != nil {
			crows.Close()
			s.serverError(w, r, err)
			return
		}
		d.Categories = append(d.Categories, EditOption{Label: c, Name: poiCategoryLabel(c)})
	}
	crows.Close()

	lrows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT p.id, p.src_pk, COALESCE(p.name,''), COALESCE(p.poi_category,''),
		COALESCE(p.province,''), COALESCE(p.city,''), COALESCE(p.district,''), COALESCE(p.thoroughfare,''),
		COALESCE(p.lat,0), COALESCE(p.lon,0), COALESCE(p.visit_count,0),
		CASE WHEN `+suspectWGS+` THEN TRUE ELSE FALSE END,
		((r.raw->>'ZLATITUDE'))::float8, ((r.raw->>'ZLONGITUDE'))::float8,
		COALESCE(p.coord_source,''), COALESCE(p.coord_sys,''), p.src_lat, p.src_lon
		FROM places p`+placeRawJoin+` WHERE %s ORDER BY p.visit_count DESC, p.id LIMIT $%d OFFSET $%d`,
		where, len(args)+1, len(args)+2),
		append(args, editPerPage, (d.PageNo-1)*editPerPage)...)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for lrows.Next() {
		var e EditPlace
		if err := lrows.Scan(&e.ID, &e.SrcPK, &e.Name, &e.Category, &e.Province, &e.City,
			&e.District, &e.Thoroughfare, &e.Lat, &e.Lon, &e.VisitCount, &e.SuspectWGS,
			&e.OrigLat, &e.OrigLon, &e.CoordSource, &e.CoordSys, &e.SrcLat, &e.SrcLon); err != nil {
			lrows.Close()
			s.serverError(w, r, err)
			return
		}
		// 站点上的坐标改过之后，留档里的那份就是「导入时的原值」，用它做前后对照
		if e.OrigLat.Valid && e.OrigLon.Valid {
			e.MovedM = int(geo.Haversine(e.OrigLat.Float64, e.OrigLon.Float64, e.Lat, e.Lon)*1000 + 0.5)
		}
		e.CategoryOptions = categoryOptions(d.Categories, e.Category)
		d.Places = append(d.Places, e)
	}
	lrows.Close()

	// 整个数据集里疑似「没做过偏移」的点数（列表分页，所以单独统计一次）
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM places p`+placeRawJoin+
		` WHERE p.dataset_id=$1 AND `+suspectWGS, ds.ID).Scan(&d.SuspectCount); err != nil {
		s.serverError(w, r, err)
		return
	}

	// 各坐标来源的数量，给筛选下拉当提示用（三种来源各一次查询太浪费，一次 group by 拿到）
	srows, err := s.db.QueryContext(ctx, `SELECT COALESCE(coord_source,''), count(*) FROM places
		WHERE dataset_id=$1 GROUP BY 1`, ds.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for srows.Next() {
		var k string
		var n int
		if err := srows.Scan(&k, &n); err != nil {
			srows.Close()
			s.serverError(w, r, err)
			return
		}
		if k == "" {
			k = placeSourceNone
		}
		d.SourceCounts[k] = n
	}
	srows.Close()

	// 最近到访（搜索 + 分页）。搜索词与地点列表独立（vq），
	// 匹配地点名 / 城市 / 备注 / 活动名，纯数字再按 src_pk 精确命中一次。
	vwhere := "v.dataset_id=$1"
	vargs := []any{ds.ID}
	if d.VisitQuery != "" {
		vargs = append(vargs, "%"+escapeLike(d.VisitQuery)+"%", d.VisitQuery)
		like, exact := len(vargs)-1, len(vargs)
		// 日期也参与模糊匹配：补录时常按「哪天」找，搜 2026-08-14 或 08-14 都该有结果
		vwhere += fmt.Sprintf(` AND (COALESCE(p.name,'') ILIKE $%d OR COALESCE(p.city,'') ILIKE $%d
			OR COALESCE(v.remark,'') ILIKE $%d OR COALESCE(a.name,'') ILIKE $%d
			OR to_char(v.arrival, 'YYYY-MM-DD') ILIKE $%d OR v.src_pk::text = $%d)`,
			like, like, like, like, like, exact)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM visits v
		LEFT JOIN places p ON p.id=v.place_id LEFT JOIN activities a ON a.id=v.activity_id
		WHERE `+vwhere, vargs...).Scan(&d.VisitTotal); err != nil {
		s.serverError(w, r, err)
		return
	}
	d.VisitPageCount = max1((d.VisitTotal + editPerPage - 1) / editPerPage)
	if d.VisitPageNo < 1 {
		d.VisitPageNo = 1
	}
	if d.VisitPageNo > d.VisitPageCount {
		d.VisitPageNo = d.VisitPageCount
	}
	vargs = append(vargs, editPerPage, (d.VisitPageNo-1)*editPerPage)
	vrows, err := s.db.QueryContext(ctx, `SELECT v.src_pk, COALESCE(p.name,''), COALESCE(p.city,''),
		COALESCE(a.name,''), COALESCE(a.id,0), v.arrival, v.departure, COALESCE(v.remark,''), COALESCE(v.emoji,''),
		v.bookmarked, v.is_user_added
		FROM visits v LEFT JOIN places p ON p.id=v.place_id LEFT JOIN activities a ON a.id=v.activity_id
		WHERE `+vwhere+fmt.Sprintf(` ORDER BY v.arrival DESC LIMIT $%d OFFSET $%d`, len(vargs)-1, len(vargs)),
		vargs...)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for vrows.Next() {
		var e EditVisit
		var dep sql.NullTime
		if err := vrows.Scan(&e.SrcPK, &e.PlaceName, &e.City, &e.ActivityName, &e.ActivityID,
			&e.Arrival, &dep, &e.Remark, &e.Emoji, &e.Bookmarked, &e.UserAdded); err != nil {
			vrows.Close()
			s.serverError(w, r, err)
			return
		}
		e.ArrivalStr = e.Arrival.Local().Format("2006-01-02T15:04")
		if dep.Valid {
			e.DepartureStr = dep.Time.Local().Format("2006-01-02T15:04")
		}
		d.Visits = append(d.Visits, e)
	}
	vrows.Close()

	// 行程记录（搜索 + 分页）。两端到访可能为空——真机里就有 35 条这样的位移——
	// 所以 join 一律 LEFT，搜索条件也要能容忍 NULL。
	const movementFrom = ` FROM movements m
		LEFT JOIN visits vf ON vf.dataset_id=m.dataset_id AND vf.src_pk=m.from_visit_src
		LEFT JOIN places pf ON pf.id=vf.place_id
		LEFT JOIN visits vt ON vt.dataset_id=m.dataset_id AND vt.src_pk=m.to_visit_src
		LEFT JOIN places pt ON pt.id=vt.place_id`
	mwhere, margs := "m.dataset_id=$1", []any{ds.ID}
	mf := d.MovementFilter
	switch {
	case mf.Transport == movementFilterNone:
		mwhere += " AND m.transport_src IS NULL"
	case mf.Transport != "":
		if n, err := strconv.Atoi(mf.Transport); err == nil && n > 0 {
			margs = append(margs, n)
			mwhere += fmt.Sprintf(" AND m.transport_src = $%d", len(margs))
		}
	}
	if mf.PlaceID > 0 {
		margs = append(margs, mf.PlaceID)
		n := len(margs)
		mwhere += fmt.Sprintf(" AND (m.from_place_id = $%d OR m.to_place_id = $%d)", n, n)
	}
	// 时间范围按 started_at 比：上界取「到」的次日零点，含当天
	if t, ok := parseDate(mf.From); ok {
		margs = append(margs, t)
		mwhere += fmt.Sprintf(" AND m.started_at >= $%d", len(margs))
	}
	if t, ok := parseDate(mf.To); ok {
		margs = append(margs, t.AddDate(0, 0, 1))
		mwhere += fmt.Sprintf(" AND m.started_at < $%d", len(margs))
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*)`+movementFrom+` WHERE `+mwhere, margs...).
		Scan(&d.MovementTotal); err != nil {
		s.serverError(w, r, err)
		return
	}
	d.MovementPageCount = max1((d.MovementTotal + editPerPage - 1) / editPerPage)
	if d.MovementPageNo < 1 {
		d.MovementPageNo = 1
	}
	if d.MovementPageNo > d.MovementPageCount {
		d.MovementPageNo = d.MovementPageCount
	}
	mrows, err := s.db.QueryContext(ctx, `SELECT m.src_pk, COALESCE(m.transport_src,0),
		COALESCE(m.transport_name,''), COALESCE(m.transport_color,''),
		COALESCE(m.from_visit_src,0), COALESCE(m.to_visit_src,0),
		COALESCE(pf.name,''), COALESCE(pf.city,''), COALESCE(pt.name,''), COALESCE(pt.city,''),
		m.started_at, m.ended_at, m.duration_min, COALESCE(m.distance_km,0)`+movementFrom+
		fmt.Sprintf(` WHERE %s ORDER BY m.started_at DESC NULLS LAST, m.src_pk DESC LIMIT $%d OFFSET $%d`,
			mwhere, len(margs)+1, len(margs)+2),
		append(margs, editPerPage, (d.MovementPageNo-1)*editPerPage)...)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for mrows.Next() {
		var e EditMovement
		var start, end sql.NullTime
		var dur sql.NullInt64
		var fromName, toName string
		if err := mrows.Scan(&e.SrcPK, &e.TransportSrc, &e.TransportName, &e.TransportColor,
			&e.FromSrc, &e.ToSrc, &fromName, &e.FromCity, &toName, &e.ToCity,
			&start, &end, &dur, &e.DistanceKm); err != nil {
			mrows.Close()
			s.serverError(w, r, err)
			return
		}
		e.FromLabel = visitEndpointLabel(e.FromSrc, fromName)
		e.ToLabel = visitEndpointLabel(e.ToSrc, toName)
		if start.Valid {
			e.StartStr = start.Time.Local().Format("2006-01-02T15:04")
		}
		if end.Valid {
			e.EndStr = end.Time.Local().Format("2006-01-02T15:04")
		}
		// 时长以 duration_min 为准；历史行没有这一列（NULL/0）时按起止时间现算，
		// 免得列表里一堆行程都显示 0 分钟
		if dur.Valid && dur.Int64 > 0 {
			e.DurationMin = dur.Int64
		} else if start.Valid && end.Valid {
			e.DurationMin = int64(end.Time.Sub(start.Time).Minutes())
		}
		if start.Valid && end.Valid {
			e.Span = movementSpan(start.Time.Local(), end.Time.Local())
		}
		d.Movements = append(d.Movements, e)
	}
	mrows.Close()

	// 新增到访用的地点下拉（按到访数取前 300）。只给「城市 · 名称」时同名地点
	// 完全无法区分，把省市区、街道、到访数和 src_pk 一起带出来——src_pk 还能
	// 直接对着行程表单里要填的值抄。
	prows, err := s.db.QueryContext(ctx, `SELECT id, src_pk, COALESCE(name,''), COALESCE(city,''),
		COALESCE(province,''), COALESCE(NULLIF(sublocality,''), NULLIF(district,''), ''),
		COALESCE(thoroughfare,''), visit_count FROM places
		WHERE dataset_id=$1 ORDER BY visit_count DESC, id LIMIT 300`, ds.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for prows.Next() {
		var o EditOption
		var city, province, area, thoroughfare string
		var srcPK, visitCount int
		if err := prows.Scan(&o.ID, &srcPK, &o.Name, &city, &province, &area, &thoroughfare, &visitCount); err != nil {
			prows.Close()
			s.serverError(w, r, err)
			return
		}
		o.Label = fmt.Sprintf("#%d %s — %s（%d次）", srcPK, o.Name,
			placeDesc(city, province, area, thoroughfare), visitCount)
		d.PlaceOptions = append(d.PlaceOptions, o)
	}
	prows.Close()

	// 最近到访：行程的起止端点、天气的关联到访都从这里选（手填 src_pk 太难用）。
	// 取最近 500 条——行程补录基本发生在近期，真要更早的可以先用筛选缩小列表。
	oprows, err := s.db.QueryContext(ctx, `SELECT v.src_pk, v.arrival, COALESCE(p.name,'') FROM visits v
		LEFT JOIN places p ON p.id=v.place_id WHERE v.dataset_id=$1 ORDER BY v.arrival DESC LIMIT 500`, ds.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for oprows.Next() {
		var pk int
		var arrival time.Time
		var pname string
		if err := oprows.Scan(&pk, &arrival, &pname); err != nil {
			oprows.Close()
			s.serverError(w, r, err)
			return
		}
		if pname == "" {
			pname = "（未命名地点）"
		}
		label := fmt.Sprintf("#%d %s %s", pk, arrival.Local().Format("2006-01-02 15:04"), pname)
		d.VisitOptions = append(d.VisitOptions, EditOption{SrcPK: pk, Label: label, Name: label})
	}
	oprows.Close()

	s.render(w, "admin/edit", d)
}

// categoryOptions 组出某一行的类别下拉：库里的已有类别，当前值不在其中时补进列表，
// 否则真机里那些非常见类别会被静默改掉或显示为空。
func categoryOptions(all []EditOption, cur string) []EditOption {
	cur = strings.TrimSpace(cur)
	out := make([]EditOption, 0, len(all)+1)
	found := false
	for _, c := range all {
		if c.Label == cur {
			found = true
		}
		out = append(out, c)
	}
	if cur != "" && !found {
		out = append(out, EditOption{Label: cur, Name: poiCategoryLabel(cur)})
	}
	return out
}

// pickCategory 处理类别下拉：选到「自定义…」时用旁边输入框的值。
func pickCategory(r *http.Request) string {
	v := strings.TrimSpace(r.PostFormValue("category"))
	if v == categoryCustomValue {
		return strings.TrimSpace(r.PostFormValue("category_custom"))
	}
	return v
}

// categoryCustomValue 是类别下拉里「自定义…」这一项的提交值。
const categoryCustomValue = "__custom__"

// poiCategoryText 把 rond/MapKit 的 POI 分类标识符换成中文。
// 库里存的原值是 MKPOICategoryXxx（rond 靠它显示图标），表单里给人看中文。
var poiCategoryText = map[string]string{
	"MKPOICategoryRestaurant":      "餐厅",
	"MKPOICategoryStore":           "商店",
	"MKPOICategoryPublicTransport": "公共交通",
	"MKPOICategoryHotel":           "酒店",
	"MKPOICategoryLandmark":        "地标",
	"MKPOICategoryFoodMarket":      "菜市场",
	"MKPOICategoryBeach":           "海滩",
	"MKPOICategoryCampground":      "露营地",
	"MKPOICategoryZoo":             "动物园",
	"MKPOICategoryMuseum":          "博物馆",
	"MKPOICategoryStadium":         "体育场",
	"MKPOICategoryPark":            "公园",
	"MKPOICategoryCafe":            "咖啡馆",
	"MKPOICategoryBar":             "酒吧",
	"MKPOICategoryBakery":          "面包店",
	"MKPOICategorySchool":          "学校",
	"MKPOICategoryHospital":        "医院",
	"MKPOICategoryPharmacy":        "药店",
	"MKPOICategoryBank":            "银行",
	"MKPOICategoryATM":             "取款机",
	"MKPOICategoryGasStation":      "加油站",
	"MKPOICategoryParking":         "停车场",
	"MKPOICategoryAirport":         "机场",
	"MKPOICategoryTrainStation":    "火车站",
	"MKPOICategoryMarina":          "码头",
	"MKPOICategoryNationalPark":    "国家公园",
	"MKPOICategoryTheater":         "剧院",
	"MKPOICategoryCinema":          "电影院",
	"MKPOICategoryLibrary":         "图书馆",
	"MKPOICategoryFitnessCenter":   "健身房",
	"MKPOICategoryAmusementPark":   "游乐园",
	"MKPOICategoryNightlife":       "夜生活",
	"MKPOICategoryRestroom":        "洗手间",
}

func poiCategoryLabel(v string) string {
	if l, ok := poiCategoryText[v]; ok {
		return l
	}
	return strings.TrimPrefix(v, "MKPOICategory")
}

// placeDesc 把省市区拼成一段（相邻重复段只留一个，直辖市省名=市名时不会出现
// 「北京市北京市」这种），街道有值再接上。
func placeDesc(city, province, area, thoroughfare string) string {
	segs := make([]string, 0, 3)
	for _, s := range []string{province, city, area} {
		if s == "" {
			continue
		}
		if len(segs) > 0 && segs[len(segs)-1] == s {
			continue
		}
		segs = append(segs, s)
	}
	if thoroughfare != "" {
		segs = append(segs, thoroughfare)
	}
	return strings.Join(segs, " ")
}

// editOptions 读取 (id, name) 形式的选项列表，供表单下拉/勾选使用。
func (s *Server) editOptions(ctx context.Context, query string, args ...any) ([]EditOption, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EditOption
	for rows.Next() {
		var o EditOption
		if err := rows.Scan(&o.ID, &o.Name); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// pruneEntityRaw 清掉留档里已经不在库中的行。这些行导出时本来就不会写回，
// 留着反而危险：新建取的是 MAX(src_pk)+1，一旦删掉编号最大的那条，下一次新建
// 就会复用同一个 src_pk，于是新行继承旧留档的 rond 私有字段（ZRADIUS/ZRAW 等）。
// skipped=TRUE 的行必须保留——它们本来就是「因缺必填字段没进库、导出时要补回」的行。
// 清理失败只记日志：留档是保真度增强，不该阻断用户的删除操作。
func (s *Server) pruneEntityRaw(ctx context.Context, datasetID int64) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM entity_raw r
		WHERE r.dataset_id=$1 AND NOT r.skipped AND (
		     (r.entity='ZLOCATION'      AND NOT EXISTS (SELECT 1 FROM places p     WHERE p.dataset_id=r.dataset_id AND p.src_pk=r.src_pk))
		  OR (r.entity='ZVISIT'         AND NOT EXISTS (SELECT 1 FROM visits v     WHERE v.dataset_id=r.dataset_id AND v.src_pk=r.src_pk))
		  OR (r.entity='ZMOVEMENT'      AND NOT EXISTS (SELECT 1 FROM movements m  WHERE m.dataset_id=r.dataset_id AND m.src_pk=r.src_pk))
		  OR (r.entity='ZHOURLYWEATHER' AND NOT EXISTS (SELECT 1 FROM weather w    WHERE w.dataset_id=r.dataset_id AND w.src_pk=r.src_pk))
		  OR (r.entity='ZACTIVITY'      AND NOT EXISTS (SELECT 1 FROM activities a WHERE a.dataset_id=r.dataset_id AND a.src_pk=r.src_pk))
		  OR (r.entity='ZTAG'           AND NOT EXISTS (SELECT 1 FROM tags t       WHERE t.dataset_id=r.dataset_id AND t.src_pk=r.src_pk))
		)`, datasetID); err != nil {
		log.Printf("清理原始行留档失败: %v", err)
	}
}

// ---------- 地点 ----------

// editActivityUpdate 重命名活动类型。
func (s *Server) editActivityUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		s.json(w, map[string]any{"error": "还没有可编辑的数据集"})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	id, _ := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("id")), 10, 64)
	name := strings.TrimSpace(r.PostFormValue("name"))
	if id <= 0 || name == "" {
		s.json(w, map[string]any{"error": "请选择活动并填写新名称"})
		return
	}
	res, err := s.db.ExecContext(ctx, `UPDATE activities SET name=$3 WHERE id=$1 AND dataset_id=$2`, id, ds.ID, name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		s.json(w, map[string]any{"error": "活动不存在"})
		return
	}
	s.json(w, map[string]any{"id": id, "name": name})
}

// editActivityDelete 删除活动类型。引用它的到访变成「未分类」——
// rond 里 ZVISIT.ZACTIVITY_ 本就允许为空，导出时该列写 NULL。
func (s *Server) editActivityDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		s.json(w, map[string]any{"error": "还没有可编辑的数据集"})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	id, _ := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("id")), 10, 64)
	if id <= 0 {
		s.json(w, map[string]any{"error": "请选择要删除的活动"})
		return
	}
	// is_home / is_work 是从活动上抄到到访的冗余标记，活动没了就该一起清掉
	if _, err := s.db.ExecContext(ctx, `UPDATE visits SET activity_id=NULL, is_home=FALSE, is_work=FALSE
		WHERE dataset_id=$1 AND activity_id=$2`, ds.ID, id); err != nil {
		s.serverError(w, r, err)
		return
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM activities WHERE id=$1 AND dataset_id=$2`, id, ds.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		s.json(w, map[string]any{"error": "活动不存在"})
		return
	}
	s.pruneEntityRaw(ctx, ds.ID)
	s.refreshDatasetAgg(ctx, ds.ID)
	s.json(w, map[string]any{"ok": true})
}

// editPlaceCreate 是最前面那个融合表单（新建地点 / 给已有地点补到访）的处理函数。
func (s *Server) editPlaceCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectEdit(w, r, "err", "还没有可编辑的数据集")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}

	// 融合表单：mode=existing 只给已有地点补一条到访；mode=new 建地点，
	// 到访时段选填（留空则只记录地点）。
	//
	// 先把所有输入校验完再落库：建地点与建到访是一个事务，校验没过就什么都不写。
	// 以前是先建地点、再校验时段，时段不合法就留下一个没有到访的地点——它导进 rond
	// 后「最近活跃时间」永远是「无数据」，用户却以为自己填了时间。
	var placeID int64
	var nextSrc int
	isNewPlace := r.PostFormValue("mode") != "existing"

	// 到访时段选填：留空则只记录地点（补已有地点时留空没有意义，给出提示）
	var arrival time.Time
	var departure *time.Time
	hasVisit := strings.TrimSpace(r.PostFormValue("arrival")) != ""
	if hasVisit {
		var ok bool
		var msg string
		if arrival, departure, ok, msg = parseVisitTimes(r); !ok {
			redirectEdit(w, r, "err", msg)
			return
		}
	} else if !isNewPlace {
		redirectEdit(w, r, "err", "给已有地点补到访时，请填写到达时间")
		return
	}
	// 活动可选：历史数据里本就存在无活动的到访
	actID, isHome, isWork := s.resolveActivity(ctx, ds.ID, r.PostFormValue("activity_id"))
	tags := r.PostForm["tags"]

	if !isNewPlace {
		placeID, err = strconv.ParseInt(strings.TrimSpace(r.PostFormValue("existing_place")), 10, 64)
		if err != nil || placeID <= 0 {
			redirectEdit(w, r, "err", "请选择要补到访的地点")
			return
		}
		if err := s.db.QueryRowContext(ctx, `SELECT src_pk FROM places WHERE id=$1 AND dataset_id=$2`, placeID, ds.ID).Scan(new(int)); err != nil {
			redirectEdit(w, r, "err", "所选地点不存在")
			return
		}
		if _, err := s.insertVisit(ctx, s.db, ds.ID, u.ID, placeID, arrival, departure, actID, isHome, isWork,
			r.PostFormValue("bookmarked") == "on",
			strings.TrimSpace(r.PostFormValue("remark")), strings.TrimSpace(r.PostFormValue("emoji")), tags); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.refreshDatasetAgg(ctx, ds.ID)
		redirectEdit(w, r, "ok", "已为所选地点新增到访")
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		redirectEdit(w, r, "err", "请填写地点名称")
		return
	}
	lat, ok1 := parseFloat(r.PostFormValue("lat"))
	lon, ok2 := parseFloat(r.PostFormValue("lon"))
	if !ok1 || !ok2 || (lat == 0 && lon == 0) {
		redirectEdit(w, r, "err", "请在地图上选点、搜索地点或填写有效的经纬度")
		return
	}
	var visitSrc int
	if err := s.withTx(ctx, func(ex execer) error {
		// 编号不能只看 places 的 MAX：留档里还躺着 skipped 行（导入时缺必填字段、
		// 没有对应实体行却占着号），entity_raw 的 Z_PK 可能比 places 还大。
		// 只看 MAX(places) 会复用那些号，导出时新行被当旧行、继承对方的未管理列。
		pk, err := s.nextSrcPK(ctx, ex, ds.ID, placeSeq)
		if err != nil {
			return err
		}
		// 同号若还留着旧留档（nextSrcPK 的留档查询是兜底，查不动就跳过），先清掉
		if err := clearStaleRaw(ctx, ex, ds.ID, "ZLOCATION", pk); err != nil {
			return err
		}
		nextSrc = pk
		// 这一页的坐标来自地图点选或高德搜索，本来就是 GCJ-02，原样记为来源。
		// 之后用户若用行内「坐标系换算」改掉了，那边的 UPDATE 会把留档一并修正。
		if err := ex.QueryRowContext(ctx, `INSERT INTO places
			(dataset_id, src_pk, name, poi_category, lat, lon, country_code, province, city, district, sublocality, thoroughfare, timezone,
			 coord_source, coord_sys, src_lat, src_lon)
			VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),NULLIF($13,''),
			        'manual','gcj02',$5,$6)
			RETURNING id`,
			ds.ID, nextSrc, name, pickCategory(r),
			lat, lon, strings.TrimSpace(r.PostFormValue("country")), strings.TrimSpace(r.PostFormValue("province")),
			strings.TrimSpace(r.PostFormValue("city")), strings.TrimSpace(r.PostFormValue("district")),
			strings.TrimSpace(r.PostFormValue("sublocality")), strings.TrimSpace(r.PostFormValue("thoroughfare")),
			strings.TrimSpace(r.PostFormValue("timezone"))).Scan(&placeID); err != nil {
			return err
		}
		if !hasVisit {
			return nil // 只记地点，不建到访
		}
		visitSrc, err = s.insertVisit(ctx, ex, ds.ID, u.ID, placeID, arrival, departure, actID, isHome, isWork,
			r.PostFormValue("bookmarked") == "on",
			strings.TrimSpace(r.PostFormValue("remark")), strings.TrimSpace(r.PostFormValue("emoji")), tags)
		return err
	}); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.refreshDatasetAgg(ctx, ds.ID)
	if hasVisit {
		redirectEdit(w, r, "ok", fmt.Sprintf("已新增地点（src_pk=%d）及到访（src_pk=%d）", nextSrc, visitSrc))
	} else {
		redirectEdit(w, r, "ok", fmt.Sprintf("已新增地点（src_pk=%d）", nextSrc))
	}
}

func (s *Server) editPlaceUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectEdit(w, r, "err", "还没有可编辑的数据集")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	src, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("src_pk")))
	if err != nil {
		redirectEdit(w, r, "err", "地点标识无效")
		return
	}
	lat, ok1 := parseFloat(r.PostFormValue("lat"))
	lon, ok2 := parseFloat(r.PostFormValue("lon"))
	if !ok1 || !ok2 {
		redirectEdit(w, r, "err", "经纬度无效")
		return
	}
	// 原坐标：保存后把「移动了多远、往哪个方向」说清楚，坐标换算是这页的常用操作，
	// 改完得能立刻看出效果（改成多少、挪了多少）
	var oldLat, oldLon float64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(lat,0), COALESCE(lon,0) FROM places
		WHERE dataset_id=$1 AND src_pk=$2`, ds.ID, src).Scan(&oldLat, &oldLon); err != nil {
		redirectEdit(w, r, "err", "地点不存在")
		return
	}
	// 表单的经纬度框只到 6 位小数，而真机基线的点带长小数（如 26.46800422211455）。
	// 原样回贴再写回去就等于把坐标截短了——只改个备注、改个名称也会悄悄挪点，
	// 导出跟原包逐位对照就对不上。与 keepStoredTime 同一套办法：落在同一个
	// 6 位小数上就沿用库里那份。
	lat = keepStoredCoord(lat, oldLat)
	lon = keepStoredCoord(lon, oldLon)
	// 名称留空时保留原值：历史数据里存在无名称的地点，不该因为编辑就逼着补名字
	//
	// 其余列按「表单提交了才写」处理：行内编辑会把当前值原样带上，所以等价于全覆盖；
	// 而批量换算这类只提交坐标的调用（tools/photo_coord_audit.py）不会把地址细分、
	// 坐标来源留档一起抹掉。坐标来源留档的取值规则见下面 updatePlaceRow 的注释。
	form := placeForm{
		name: strings.TrimSpace(r.PostFormValue("name")), category: pickCategory(r), lat: lat, lon: lon,
		province: strings.TrimSpace(r.PostFormValue("province")),
		city:     strings.TrimSpace(r.PostFormValue("city")),
		district: strings.TrimSpace(r.PostFormValue("district")),
	}
	if _, err := s.updatePlaceRow(ctx, ds.ID, src, form, placeExtraFromForm(r), oldLat, oldLon); err != nil {
		s.serverError(w, r, err)
		return
	}
	if moved := geo.Haversine(oldLat, oldLon, lat, lon) * 1000; moved >= 5 {
		redirectEdit(w, r, "ok", fmt.Sprintf("地点已更新：坐标移动到 %.6f, %.6f（%.0f 米，%s方向）",
			lat, lon, moved, bearingLabel(geo.Bearing(oldLat, oldLon, lat, lon))))
		return
	}
	redirectEdit(w, r, "ok", "地点已更新")
}

// placeForm 是地点行编辑里必填的那几个字段。
type placeForm struct {
	name                     string
	category                 string
	lat, lon                 float64
	province, city, district string
}

// placeExtra 是可选的细分字段与坐标来源留档；nil 表示「表单没提交，保持原值」。
type placeExtra struct {
	thoroughfare, sublocality, country, timezone *string
	coordSource, coordSys                        *string
	// srcLat/srcLon 只在「表单明确给了原始坐标」时有值（行内换算会把换算前的值带上），
	// 让留档里的原始坐标保持是原始坐标，而不是被换算后的显示坐标顶掉。
	srcLat, srcLon *float64
}

// placeNone 是表单里「未记录」这个选项的提交值：要显式清空一列时用。
const placeNone = "none"

func optForm(v string) *string {
	if v == placeNone {
		s := ""
		return &s
	}
	return &v
}

func placeExtraFromForm(r *http.Request) placeExtra {
	has := func(k string) bool { _, ok := r.PostForm[k]; return ok }
	var e placeExtra
	if has("thoroughfare") {
		e.thoroughfare = optForm(strings.TrimSpace(r.PostFormValue("thoroughfare")))
	}
	if has("sublocality") {
		e.sublocality = optForm(strings.TrimSpace(r.PostFormValue("sublocality")))
	}
	if has("country") {
		e.country = optForm(strings.TrimSpace(r.PostFormValue("country")))
	}
	if has("timezone") {
		e.timezone = optForm(strings.TrimSpace(r.PostFormValue("timezone")))
	}
	if has("coord_source") {
		e.coordSource = optForm(strings.TrimSpace(r.PostFormValue("coord_source")))
	}
	if has("coord_sys") {
		e.coordSys = optForm(strings.TrimSpace(r.PostFormValue("coord_sys")))
	}
	if has("src_lat") && has("src_lon") {
		if a, ok := parseFloat(r.PostFormValue("src_lat")); ok {
			if b, ok2 := parseFloat(r.PostFormValue("src_lon")); ok2 {
				e.srcLat, e.srcLon = &a, &b
			}
		}
	}
	return e
}

// updatePlaceRow 把一行地点的改动落到库里。
//
// 坐标来源留档（coord_source / coord_sys / src_lat / src_lon）的规则：
//   - 表单没提交的列一律不动——批量工具只改坐标，不该顺手把留档清掉；
//   - 表单提交了 coord_sys：非空就记下来，空（placeNone）就清空这一组留档；
//   - 原始坐标 src_lat/src_lon：换算时由表单把换算前的值带上来，优先用它；
//     没带、但坐标确实动过，就认为提交上来的是显示坐标本身（等价于 gcj02）。
//     坐标没动则一个字都不碰，否则每次改个名字都会把「导入时的原始坐标」冲掉，
//     行内那个「比导入时移动了 N 米」的对照就没了。
func (s *Server) updatePlaceRow(ctx context.Context, datasetID int64, src int, f placeForm,
	e placeExtra, oldLat, oldLon float64) (int64, error) {
	set := []string{
		"name=COALESCE(NULLIF($3,''), name)", "poi_category=NULLIF($4,'')",
		"lat=$5", "lon=$6", "province=NULLIF($7,'')", "city=NULLIF($8,'')", "district=NULLIF($9,'')",
	}
	args := []any{datasetID, src, f.name, f.category, f.lat, f.lon, f.province, f.city, f.district}
	add := func(expr string, v any) {
		args = append(args, v)
		set = append(set, fmt.Sprintf(expr, len(args)))
	}
	setNull := func(expr string) { set = append(set, expr) }
	if e.thoroughfare != nil {
		add("thoroughfare=NULLIF($%d,'')", *e.thoroughfare)
	}
	if e.sublocality != nil {
		add("sublocality=NULLIF($%d,'')", *e.sublocality)
	}
	if e.country != nil {
		add("country_code=NULLIF($%d,'')", *e.country)
	}
	if e.timezone != nil {
		add("timezone=NULLIF($%d,'')", *e.timezone)
	}
	if e.coordSource != nil {
		add("coord_source=NULLIF($%d,'')", *e.coordSource)
	}
	if e.coordSys != nil {
		add("coord_sys=NULLIF($%d,'')", *e.coordSys)
		if *e.coordSys == "" {
			// 明确选了「未记录」：整组留档一起清掉，别留下对不上的半截数据
			setNull("src_lat=NULL")
			setNull("src_lon=NULL")
		} else if e.srcLat != nil {
			add("src_lat=$%d", *e.srcLat)
			add("src_lon=$%d", *e.srcLon)
		} else if f.lat != oldLat || f.lon != oldLon {
			add("src_lat=$%d", f.lat)
			add("src_lon=$%d", f.lon)
		}
	} else if (f.lat != oldLat || f.lon != oldLon) && e.srcLat != nil {
		add("src_lat=$%d", *e.srcLat)
		add("src_lon=$%d", *e.srcLon)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE places SET `+strings.Join(set, ", ")+
		` WHERE dataset_id=$1 AND src_pk=$2`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Server) editPlaceDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectEdit(w, r, "err", "还没有可编辑的数据集")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	src, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("src_pk")))
	if err != nil {
		redirectEdit(w, r, "err", "地点标识无效")
		return
	}
	// 该地点名下到访的 src_pk 集合（删地点会级联删掉这些到访）
	const visitSrcs = `SELECT src_pk FROM visits WHERE dataset_id=$1
		AND place_id=(SELECT id FROM places WHERE dataset_id=$1 AND src_pk=$2)`
	// 行程以两端到访为锚点：端点没了还留着就是悬空引用，rond 里会指向不存在的到访
	if _, err := s.db.ExecContext(ctx, `DELETE FROM movements WHERE dataset_id=$1
		AND (from_visit_src IN (`+visitSrcs+`) OR to_visit_src IN (`+visitSrcs+`))`, ds.ID, src); err != nil {
		s.serverError(w, r, err)
		return
	}
	// 天气本身有独立价值，只解除关联，不连坐删除
	if _, err := s.db.ExecContext(ctx, `UPDATE weather SET visit_src=NULL
		WHERE dataset_id=$1 AND visit_src IN (`+visitSrcs+`)`, ds.ID, src); err != nil {
		s.serverError(w, r, err)
		return
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM places WHERE dataset_id=$1 AND src_pk=$2`, ds.ID, src); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.pruneEntityRaw(ctx, ds.ID)
	s.refreshDatasetAgg(ctx, ds.ID)
	redirectEdit(w, r, "ok", "地点及其到访、关联行程已删除")
}

// ---------- 到访 ----------
// （新增到访已并入 editPlaceCreate 的融合表单，这里只剩更新 / 删除）

// editActivityCreate 内联新建一个活动类型，JSON 返回给融合表单即时加进下拉。
func (s *Server) editActivityCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		s.json(w, map[string]any{"error": "还没有可编辑的数据集"})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.json(w, map[string]any{"error": "请填写活动名称"})
		return
	}
	var id int64
	if err := s.withTx(ctx, func(ex execer) error {
		// 编号同其他实体：留档里的 skipped 行可能占着比 activities 更大的号。
		// 活动还会带 ZACTIVITY.ZUID_（导出侧给新建行补的确定性 UUID），编号撞了就会串行。
		pk, err := s.nextSrcPK(ctx, ex, ds.ID, activitySeq)
		if err != nil {
			return err
		}
		if err := clearStaleRaw(ctx, ex, ds.ID, "ZACTIVITY", pk); err != nil {
			return err
		}
		return ex.QueryRowContext(ctx, `INSERT INTO activities (dataset_id, src_pk, name, color)
			VALUES ($1, $2, $3, 'blue') RETURNING id`, ds.ID, pk, name).Scan(&id)
	}); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.refreshDatasetAgg(ctx, ds.ID)
	s.json(w, map[string]any{"id": id, "name": name})
}

func (s *Server) editVisitUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectEdit(w, r, "err", "还没有可编辑的数据集")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	src, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("src_pk")))
	if err != nil {
		redirectEdit(w, r, "err", "到访标识无效")
		return
	}
	arrival, departure, ok, msg := parseVisitTimes(r)
	if !ok {
		redirectEdit(w, r, "err", msg)
		return
	}
	actID, isHome, isWork := s.resolveActivity(ctx, ds.ID, r.PostFormValue("activity_id"))
	var oldPlace int64
	var curArrival time.Time
	var curDeparture sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(place_id,0), arrival, departure FROM visits
		WHERE dataset_id=$1 AND src_pk=$2`, ds.ID, src).Scan(&oldPlace, &curArrival, &curDeparture); err != nil {
		redirectEdit(w, r, "err", "到访记录不存在")
		return
	}
	// 时间字段是分钟精度：只改类型/备注时不能把真机的小数秒（20:03:21.323）截成整分钟
	arrival = keepStoredTime(arrival, &curArrival)
	if departure != nil && curDeparture.Valid {
		kept := keepStoredTime(*departure, &curDeparture.Time)
		departure = &kept
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE visits SET activity_id=$3, arrival=$4, departure=$5, duration_min=$6,
		is_home=$7, is_work=$8, bookmarked=$9, remark=NULLIF($10,''), emoji=NULLIF($11,'')
		WHERE dataset_id=$1 AND src_pk=$2`,
		ds.ID, src, actID, arrival, departure, durationMin(arrival, departure), isHome, isWork,
		r.PostFormValue("bookmarked") == "on",
		strings.TrimSpace(r.PostFormValue("remark")), strings.TrimSpace(r.PostFormValue("emoji"))); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.saveVisitTags(ctx, s.db, ds.ID, src, r.PostForm["tags"])
	if oldPlace > 0 {
		s.refreshPlaceAgg(ctx, s.db, oldPlace)
	}
	s.refreshDatasetAgg(ctx, ds.ID)
	redirectEdit(w, r, "ok", "到访已更新")
}

// adminVisitBookmark 切换一条到访的收藏标记。列表里直接点星标就能取消，
// 不用展开行内编辑——之前勾上收藏后找不到取消的地方。
func (s *Server) adminVisitBookmark(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectEdit(w, r, "err", "还没有可编辑的数据集")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	src, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("src_pk")))
	if err != nil {
		redirectEdit(w, r, "err", "到访标识无效")
		return
	}
	res, err := s.db.ExecContext(ctx, `UPDATE visits SET bookmarked = NOT bookmarked
		WHERE dataset_id=$1 AND src_pk=$2`, ds.ID, src)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		redirectEdit(w, r, "err", "到访记录不存在")
		return
	}
	var on bool
	if err := s.db.QueryRowContext(ctx, `SELECT bookmarked FROM visits WHERE dataset_id=$1 AND src_pk=$2`,
		ds.ID, src).Scan(&on); err == nil && on {
		redirectEdit(w, r, "ok", "已标记为收藏")
		return
	}
	redirectEdit(w, r, "ok", "已取消收藏")
}
func (s *Server) editVisitDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectEdit(w, r, "err", "还没有可编辑的数据集")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	src, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("src_pk")))
	if err != nil {
		redirectEdit(w, r, "err", "到访标识无效")
		return
	}
	var placeID int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(place_id,0) FROM visits WHERE dataset_id=$1 AND src_pk=$2`, ds.ID, src).Scan(&placeID); err != nil {
		redirectEdit(w, r, "err", "到访记录不存在")
		return
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM movements WHERE dataset_id=$1
		AND (from_visit_src=$2 OR to_visit_src=$2)`, ds.ID, src); err != nil {
		s.serverError(w, r, err)
		return
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE weather SET visit_src=NULL WHERE dataset_id=$1 AND visit_src=$2`, ds.ID, src); err != nil {
		s.serverError(w, r, err)
		return
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM visits WHERE dataset_id=$1 AND src_pk=$2`, ds.ID, src); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.pruneEntityRaw(ctx, ds.ID)
	if placeID > 0 {
		s.refreshPlaceAgg(ctx, s.db, placeID)
	}
	s.refreshDatasetAgg(ctx, ds.ID)
	redirectEdit(w, r, "ok", "到访已删除")
}

// ---------- 行程 ----------

// editTransports 读出该数据集里用到的交通方式（含被引用的行程数），
// 供下拉选择与「管理方式」用。ZTRANSPORT 在 rond 里是全局表，站点这边不落库，
// 名称/颜色/图标是冗余存在每条行程上的，因此同一方式的不同行程可能不一致——
// 取值规则必须与导出侧 queryTransports 一致（取最近编辑过的那条的非空值）。
func (s *Server) editTransports(ctx context.Context, datasetID int64) ([]EditTransport, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT transport_src, count(*),
		(array_agg(transport_name  ORDER BY id DESC) FILTER (WHERE transport_name  IS NOT NULL))[1],
		(array_agg(transport_color ORDER BY id DESC) FILTER (WHERE transport_color IS NOT NULL))[1],
		(array_agg(transport_icon  ORDER BY id DESC) FILTER (WHERE transport_icon  IS NOT NULL))[1]
		FROM movements WHERE dataset_id=$1 AND transport_src IS NOT NULL
		GROUP BY transport_src ORDER BY transport_src`, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EditTransport
	for rows.Next() {
		var t EditTransport
		var name, color, icon sql.NullString
		if err := rows.Scan(&t.SrcPK, &t.Count, &name, &color, &icon); err != nil {
			return nil, err
		}
		t.Name, t.Color, t.Icon = name.String, color.String, icon.String
		out = append(out, t)
	}
	return out, rows.Err()
}

// visitEndpointLabel 把行程一端的到访渲染成「#编号 地点名」。
// 没有关联到访（真机里就有）时给一句人话，别留空白格。
func visitEndpointLabel(src int, name string) string {
	if src <= 0 {
		return "（无关联到访）"
	}
	if name == "" {
		name = "（未命名地点）"
	}
	return fmt.Sprintf("#%d %s", src, name)
}

// movementEnds 是行程一端的到访在库里的状态，用来校验/推导行程区间。
type movementEnds struct {
	SrcPK   int
	PlaceID int64
	Arrive  time.Time
	Depart  *time.Time
}

// visitEnds 读出一条到访的到达/离开时间与地点。
func (s *Server) visitEnds(ctx context.Context, ex execer, datasetID int64, src int) (movementEnds, bool) {
	var e movementEnds
	var dep sql.NullTime
	e.SrcPK = src
	if err := ex.QueryRowContext(ctx, `SELECT COALESCE(place_id,0), arrival, departure FROM visits
		WHERE dataset_id=$1 AND src_pk=$2`, datasetID, src).Scan(&e.PlaceID, &e.Arrive, &dep); err != nil {
		return e, false
	}
	if dep.Valid {
		e.Depart = &dep.Time
	}
	return e, true
}

// movementTimes 是库里一条位移的起止时间。
type movementTimes struct{ Start, End *time.Time }

// movementWindowInput 是行程时间区间的输入。StartRaw / EndRaw 是表单提交的
// datetime-local 值（分钟精度，留空表示按两端到访推断）；CurStart / CurEnd 是库里
// 已有的值（改已有行程时才有）。
type movementWindowInput struct {
	From, To         movementEnds
	StartRaw, EndRaw string
	CurStart, CurEnd *time.Time
}

// movementWindow 校验并补齐行程的起止时间。
//
// 真机规律（实测 760 条起点、756 条终点全部满足）：位移不早于起点到访的离开时间，
// 也不晚于终点到访的到达时间。留空就按这两端补默认值——ZSTART_/ZEND_ 在真机里
// 791/791 都有值，是必填语义，导出不能写 NULL。
func movementWindow(in movementWindowInput) (time.Time, time.Time, string) {
	lo := in.From.Arrive
	if in.From.Depart != nil {
		lo = *in.From.Depart
	}
	hi := in.To.Arrive
	start, startSet, msg := resolveMovementEnd(in.StartRaw, in.CurStart, lo, "开始时间")
	if msg != "" {
		return time.Time{}, time.Time{}, msg
	}
	end, endSet, msg := resolveMovementEnd(in.EndRaw, in.CurEnd, hi, "结束时间")
	if msg != "" {
		return time.Time{}, time.Time{}, msg
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Sprintf(
			"结束时间（%s）必须晚于开始时间（%s）", end.Format("2006-01-02 15:04"), start.Format("2006-01-02 15:04"))
	}
	// 表单只填到分钟：手填的 20:03 落在真机的 20:03:21 上会被判成「早于离开时间」，
	// 这是取整误差不是时序错误，给一分钟余量；真机数据的违例是 0 条，余量不会放过真问题
	if startSet && start.Before(lo.Add(-time.Minute)) {
		return time.Time{}, time.Time{}, fmt.Sprintf(
			"开始时间不能早于起点到访的离开时间（%s）", lo.Format("2006-01-02 15:04"))
	}
	if endSet && end.After(hi.Add(time.Minute)) {
		return time.Time{}, time.Time{}, fmt.Sprintf(
			"结束时间不能晚于终点到访的到达时间（%s）", hi.Format("2006-01-02 15:04"))
	}
	return start, end, ""
}

// resolveMovementEnd 解析一端的时间。留空就用默认值；**与库里已有的值落在同一分钟
// 视为「没改」，直接沿用原值**——表单只有分钟精度，照提交值写回会把真机的小数秒
// （实测 20:03:21.323）截成 20:03:00，于是「只改交通方式」的保存会同时改掉时间，
// 截断后还早于起点到访的离开时间，被上面的校验直接拒掉。
func resolveMovementEnd(raw string, cur *time.Time, def time.Time, label string) (time.Time, bool, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if cur != nil {
			return *cur, false, ""
		}
		return def, false, ""
	}
	t, ok := parseLocalDT(raw)
	if !ok {
		return time.Time{}, false, label + "格式不正确"
	}
	kept := keepStoredTime(t, cur)
	return kept, kept.Equal(t), ""
}

// sameMinute 比的是本地时区下的「哪一分钟」：表单值是按本地时区解析出来的，
// 而库里取回的 timestamptz 常在 UTC，先统一到本地再比。
func sameMinute(a, b time.Time) bool {
	const m = "2006-01-02T15:04"
	return a.In(time.Local).Format(m) == b.In(time.Local).Format(m)
}

// keepStoredTime 表单提交的时间与库里已有的值落在同一分钟时，沿用库里那份。
// 页面里的时间输入只到分钟，照提交值写回会把真机的小数秒截掉——改个备注、换个类型
// 都会顺带把 ZARRIVALDATE_/ZSTART_ 磨掉一点，导出的包就跟原包对不上了。
func keepStoredTime(t time.Time, cur *time.Time) time.Time {
	if cur != nil && sameMinute(t, *cur) {
		return *cur
	}
	return t
}

// keepStoredCoord 与 keepStoredTime 同一套办法，只是精度从「分钟」换成「6 位小数」：
// 表单控件只带 6 位，而真机留档的点是长小数。提交值与库里那份落在同一个 6 位小数上，
// 就说明用户没有真的改坐标，沿用库里那份，避免把小数位截掉。
func keepStoredCoord(sub, cur float64) float64 {
	if math.Round(sub*1e6) == math.Round(cur*1e6) {
		return cur
	}
	return sub
}

// movementTransport 是表单里选定的交通方式最终要写进库的样子。
// Src==0 表示这条行程没有交通方式（真机里有 390/791 条如此）。
type movementTransport struct {
	Src   int
	Name  string
	Color string
	Icon  string
}

// transportNewValue 是交通方式下拉里「新建方式…」这一项的提交值。
const transportNewValue = "__new__"

// resolveMovementTransport 解析表单里的交通方式。取值为「（无） / 已有方式 /
// 新建方式…」三种；新建时分配新编号。名称/颜色/图标留空表示沿用该方式的现有值——
// 方式必须有个名字，否则 rond 里会出现一个没有名字的模式。
func (s *Server) resolveMovementTransport(ctx context.Context, ex execer, datasetID int64, srcRaw, name, color, icon string) (movementTransport, string) {
	srcRaw = strings.TrimSpace(srcRaw)
	if srcRaw == "" {
		return movementTransport{}, ""
	}
	if srcRaw == transportNewValue {
		name = strings.TrimSpace(name)
		if name == "" {
			return movementTransport{}, "新建交通方式时请填写方式名称"
		}
		next, err := s.nextSrcPK(ctx, ex, datasetID, transportSeq)
		if err != nil {
			return movementTransport{}, "无法分配交通方式编号：" + err.Error()
		}
		return movementTransport{Src: next, Name: name, Color: strings.TrimSpace(color), Icon: strings.TrimSpace(icon)}, ""
	}
	src, err := strconv.Atoi(srcRaw)
	if err != nil || src <= 0 {
		return movementTransport{}, "交通方式无效"
	}
	var cur EditTransport
	var cname, ccolor, cicon sql.NullString
	if err := ex.QueryRowContext(ctx, `SELECT
		(array_agg(transport_name  ORDER BY id DESC) FILTER (WHERE transport_name  IS NOT NULL))[1],
		(array_agg(transport_color ORDER BY id DESC) FILTER (WHERE transport_color IS NOT NULL))[1],
		(array_agg(transport_icon  ORDER BY id DESC) FILTER (WHERE transport_icon  IS NOT NULL))[1]
		FROM movements WHERE dataset_id=$1 AND transport_src=$2`,
		datasetID, src).Scan(&cname, &ccolor, &cicon); err != nil {
		return movementTransport{}, "交通方式不存在"
	}
	cur.Name, cur.Color, cur.Icon = cname.String, ccolor.String, cicon.String
	t := movementTransport{Src: src, Name: cur.Name, Color: cur.Color, Icon: cur.Icon}
	// 留空=保持原值：表单里这三个字段是给「改名/换色」用的，清空不该把方式变成无名模式
	if v := strings.TrimSpace(name); v != "" {
		t.Name = v
	}
	if v := strings.TrimSpace(color); v != "" {
		t.Color = v
	}
	if v := strings.TrimSpace(icon); v != "" {
		t.Icon = v
	}
	return t, ""
}

// syncTransport 把方式的名称/颜色/图标统一写回该方式名下的所有行程。
// 这些列是冗余存在每条行程上的，不统一的话导出时的聚合结果取决于哪条行程先被读到：
// rond 里会出现两个同名方式，或者同一方式一会儿叫这个名字一会儿叫那个。
func syncTransport(ctx context.Context, ex execer, datasetID int64, t movementTransport) error {
	if t.Src <= 0 {
		return nil
	}
	_, err := ex.ExecContext(ctx, `UPDATE movements SET transport_name=NULLIF($3,''),
		transport_color=NULLIF($4,''), transport_icon=NULLIF($5,'')
		WHERE dataset_id=$1 AND transport_src=$2`,
		datasetID, t.Src, t.Name, t.Color, t.Icon)
	return err
}

// editMovementCreate 新增一条行程。
func (s *Server) editMovementCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectEdit(w, r, "err", "还没有可编辑的数据集")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	src, ok, msg := s.validateMovementForm(ctx, s.db, ds.ID, r, 0)
	if !ok {
		redirectEdit(w, r, "err", msg)
		return
	}
	tr, msg := s.resolveMovementTransport(ctx, s.db, ds.ID,
		r.PostFormValue("transport_src"), r.PostFormValue("transport_name"),
		r.PostFormValue("transport_color"), r.PostFormValue("transport_icon"))
	if msg != "" {
		redirectEdit(w, r, "err", msg)
		return
	}
	if err := s.withTx(ctx, func(ex execer) error {
		pk, err := s.nextSrcPK(ctx, ex, ds.ID, movementSeq)
		if err != nil {
			return err
		}
		if err := clearStaleRaw(ctx, ex, ds.ID, "ZMOVEMENT", pk); err != nil {
			return err
		}
		var trSrc, trName, trColor, trIcon any
		if tr.Src > 0 {
			trSrc, trName, trColor, trIcon = tr.Src, nullStr(tr.Name), nullStr(tr.Color), nullStr(tr.Icon)
		}
		if _, err := ex.ExecContext(ctx, `INSERT INTO movements
			(dataset_id, user_id, src_pk, transport_src, transport_name, transport_color, transport_icon,
			 from_visit_src, to_visit_src, from_place_id, to_place_id, started_at, ended_at, duration_min, distance_km)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			ds.ID, u.ID, pk, trSrc, trName, trColor, trIcon,
			src.FromVisit, src.ToVisit, nullInt64(src.FromPlace), nullInt64(src.ToPlace),
			src.Start, src.End, src.DurationMin, src.DistanceKm); err != nil {
			return err
		}
		return syncTransport(ctx, ex, ds.ID, tr)
	}); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirectEdit(w, r, "ok", "已新增行程")
}

// editMovementUpdate 修改一条行程的方式 / 区间，派生字段（两端地点、里程、时长）一并重算。
// 位移类型 ZTYPE_ 不在可改范围内：真机里手改过行程的记录也保留原来的类型值。
func (s *Server) editMovementUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectEdit(w, r, "err", "还没有可编辑的数据集")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	pk, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("src_pk")))
	if err != nil || pk <= 0 {
		redirectEdit(w, r, "err", "行程标识无效")
		return
	}
	src, ok, msg := s.validateMovementForm(ctx, s.db, ds.ID, r, pk)
	if !ok {
		redirectEdit(w, r, "err", msg)
		return
	}
	tr, msg := s.resolveMovementTransport(ctx, s.db, ds.ID,
		r.PostFormValue("transport_src"), r.PostFormValue("transport_name"),
		r.PostFormValue("transport_color"), r.PostFormValue("transport_icon"))
	if msg != "" {
		redirectEdit(w, r, "err", msg)
		return
	}
	var trSrc, trName, trColor, trIcon any
	if tr.Src > 0 {
		trSrc, trName, trColor, trIcon = tr.Src, nullStr(tr.Name), nullStr(tr.Color), nullStr(tr.Icon)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE movements SET
		transport_src=$3, transport_name=$4, transport_color=$5, transport_icon=$6,
		from_visit_src=$7, to_visit_src=$8, from_place_id=$9, to_place_id=$10,
		started_at=$11, ended_at=$12, duration_min=$13, distance_km=$14
		WHERE dataset_id=$1 AND src_pk=$2`,
		ds.ID, pk, trSrc, trName, trColor, trIcon,
		src.FromVisit, src.ToVisit, nullInt64(src.FromPlace), nullInt64(src.ToPlace),
		src.Start, src.End, src.DurationMin, src.DistanceKm)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		redirectEdit(w, r, "err", "行程不存在")
		return
	}
	if err := syncTransport(ctx, s.db, ds.ID, tr); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirectEdit(w, r, "ok", "行程已更新")
}

// editMovementDelete 删除一条行程。留档要一起清掉：下一个新建可能复用它的 src_pk，
// 留着旧留档会让新行继承这条的 rond 私有字段（ZTYPE_/ZNOTE_ 之类）。
func (s *Server) editMovementDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectEdit(w, r, "err", "还没有可编辑的数据集")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	pk, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("src_pk")))
	if err != nil || pk <= 0 {
		redirectEdit(w, r, "err", "行程标识无效")
		return
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM movements WHERE dataset_id=$1 AND src_pk=$2`, ds.ID, pk)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		redirectEdit(w, r, "err", "行程不存在")
		return
	}
	s.pruneEntityRaw(ctx, ds.ID)
	redirectEdit(w, r, "ok", "行程已删除")
}

// movementForm 是校验通过的行程写入参数。
type movementForm struct {
	FromVisit, ToVisit int
	FromPlace, ToPlace int64
	Start, End         time.Time
	DurationMin        int
	DistanceKm         float64
}

// validateMovementForm 校验表单并算出所有派生字段。pk>0 表示改已有行程（先确认它存在）。
//
// 这里的每个校验都对着真机数据：起止到访都存在且不同、位移不早于起点离开、
// 不晚于终点到达、时长非负。两端的 place_id 与里程也必须一起算出来——
// 漏了的话这条行程在网页地图上画不出来，而且区域黑名单是按两端地点判定的，
// 地点为空会让它绕过过滤直接露给访客。
func (s *Server) validateMovementForm(ctx context.Context, ex execer, datasetID int64, r *http.Request, pk int) (movementForm, bool, string) {
	var f movementForm
	var cur movementTimes
	if pk > 0 {
		var st, en sql.NullTime
		if err := ex.QueryRowContext(ctx, `SELECT started_at, ended_at FROM movements
			WHERE dataset_id=$1 AND src_pk=$2`, datasetID, pk).Scan(&st, &en); err != nil {
			return f, false, "行程不存在"
		}
		if st.Valid {
			cur.Start = &st.Time
		}
		if en.Valid {
			cur.End = &en.Time
		}
	}
	from, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("from_visit")))
	to, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("to_visit")))
	if from <= 0 || to <= 0 {
		return f, false, "请选择起始到访与结束到访"
	}
	if from == to {
		return f, false, "起始到访与结束到访不能是同一条记录"
	}
	fromEnd, okFrom := s.visitEnds(ctx, ex, datasetID, from)
	if !okFrom {
		return f, false, "起始到访不存在"
	}
	toEnd, okTo := s.visitEnds(ctx, ex, datasetID, to)
	if !okTo {
		return f, false, "结束到访不存在"
	}
	start, end, msg := movementWindow(movementWindowInput{
		From: fromEnd, To: toEnd,
		StartRaw: r.PostFormValue("start"), EndRaw: r.PostFormValue("end"),
		CurStart: cur.Start, CurEnd: cur.End,
	})
	if msg != "" {
		return f, false, msg
	}
	f = movementForm{
		FromVisit: from, ToVisit: to, FromPlace: fromEnd.PlaceID, ToPlace: toEnd.PlaceID,
		Start: start, End: end, DurationMin: int(end.Sub(start).Minutes()),
	}
	// 里程：与导入路径一致，用两端地点的 GCJ-02 坐标算直线距离
	fromCoord, okFromCoord := s.placeCoord(ctx, ex, datasetID, fromEnd.PlaceID)
	toCoord, okToCoord := s.placeCoord(ctx, ex, datasetID, toEnd.PlaceID)
	if okFromCoord && okToCoord && fromCoord != toCoord {
		f.DistanceKm = geo.Haversine(fromCoord[0], fromCoord[1], toCoord[0], toCoord[1])
	}
	return f, true, ""
}

// placeCoord 取地点的显示坐标（GCJ-02）。
func (s *Server) placeCoord(ctx context.Context, ex execer, datasetID, placeID int64) ([2]float64, bool) {
	if placeID <= 0 {
		return [2]float64{}, false
	}
	var lat, lon float64
	if err := ex.QueryRowContext(ctx, `SELECT COALESCE(lat,0), COALESCE(lon,0) FROM places
		WHERE dataset_id=$1 AND id=$2`, datasetID, placeID).Scan(&lat, &lon); err != nil {
		return [2]float64{}, false
	}
	if lat == 0 && lon == 0 {
		return [2]float64{}, false
	}
	return [2]float64{lat, lon}, true
}

// srcPKSeq 描述一个「站点会新建行」的编号空间。
type srcPKSeq struct {
	table  string // PostgreSQL 表名
	col    string // 取最大值的列
	entity string // coredata_meta 里的实体名
	raw    string // entity_raw 里的表名（Core Data 表名）
}

var (
	placeSeq     = srcPKSeq{"places", "src_pk", "Location", "ZLOCATION"}
	visitSeq     = srcPKSeq{"visits", "src_pk", "Visit", "ZVISIT"}
	activitySeq  = srcPKSeq{"activities", "src_pk", "Activity", "ZACTIVITY"}
	weatherSeq   = srcPKSeq{"weather", "src_pk", "HourlyWeather", "ZHOURLYWEATHER"}
	movementSeq  = srcPKSeq{"movements", "src_pk", "Movement", "ZMOVEMENT"}
	transportSeq = srcPKSeq{"movements", "transport_src", "Transport", "ZTRANSPORT"}
)

// nextSrcPK 给站点新建的行分配编号：库里该列的最大值、导入时 Core Data 的最大主键、
// 整行留档里的最大主键，三者取大再加一。
//
// 只按库里现有数据取最大值时，「删掉编号最大的那条」会让下一次新建复用同一个号——
// 导出时那个号在基线里已经存在，新行会被当成旧行去 UPDATE，继承对方的 rond 私有字段
// （位移的 ZTYPE_ 就是这么来的）；对交通方式更糟：会静默改掉一个已有模式的名字。
// 所以这里必须同时看导入时的编号上限，而它只在导入那一刻能读到。
func (s *Server) nextSrcPK(ctx context.Context, ex execer, datasetID int64, q srcPKSeq) (int, error) {
	var max int
	if err := ex.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COALESCE(MAX(%s),0) FROM %s WHERE dataset_id=$1`, q.col, q.table), datasetID).Scan(&max); err != nil {
		return 0, err
	}
	// 导入时抓到的 Z_PRIMARYKEY.Z_MAX：老数据集没抓到（NULL）时退回 0
	var metaMax int
	if err := ex.QueryRowContext(ctx, `SELECT COALESCE((coredata_meta->'entities'->$2->>'max')::int,0)
		FROM datasets WHERE id=$1`, datasetID, q.entity).Scan(&metaMax); err == nil && metaMax > max {
		max = metaMax
	}
	// 留档是逐行的原始记录，可作为兜底（表不存在或查不动就忽略）
	var rawMax int
	if err := ex.QueryRowContext(ctx, `SELECT COALESCE(MAX((raw->>'Z_PK')::numeric),0)::int FROM entity_raw
		WHERE dataset_id=$1 AND entity=$2 AND (raw->>'Z_PK') ~ '^[0-9]+$'`, datasetID, q.raw).
		Scan(&rawMax); err == nil && rawMax > max {
		max = rawMax
	}
	return max + 1, nil
}

// clearStaleRaw 删掉指定实体/编号的旧留档。新建行的编号若与已删记录重合，
// 留着旧留档会让新行在导出时被当成那条旧记录写回。
func clearStaleRaw(ctx context.Context, ex execer, datasetID int64, entity string, pk int) error {
	_, err := ex.ExecContext(ctx, `DELETE FROM entity_raw WHERE dataset_id=$1 AND entity=$2 AND src_pk=$3`,
		datasetID, entity, pk)
	return err
}

func nullInt64(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

// ---------- 天气 ----------

func (s *Server) editWeatherCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectEdit(w, r, "err", "还没有可编辑的数据集")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	at, ok := parseLocalDT(r.PostFormValue("at"))
	if !ok {
		redirectEdit(w, r, "err", "请填写有效的天气时间")
		return
	}
	var visitSrc any
	if v := strings.TrimSpace(r.PostFormValue("visit_src")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			visitSrc = n
		}
	}
	temp, _ := parseFloat(r.PostFormValue("temperature_c"))
	var nextSrc int
	if err := s.withTx(ctx, func(ex execer) error {
		// 与地点同一套：留档里可能躺着比 weather 表更大的号（skipped 行），只看 MAX 会复用
		pk, err := s.nextSrcPK(ctx, ex, ds.ID, weatherSeq)
		if err != nil {
			return err
		}
		if err := clearStaleRaw(ctx, ex, ds.ID, "ZHOURLYWEATHER", pk); err != nil {
			return err
		}
		nextSrc = pk
		_, err = ex.ExecContext(ctx, `INSERT INTO weather
			(dataset_id, user_id, src_pk, visit_src, at, temperature_c, condition, symbol)
			VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''))`,
			ds.ID, u.ID, nextSrc, visitSrc, at, temp,
			strings.TrimSpace(r.PostFormValue("condition")), strings.TrimSpace(r.PostFormValue("symbol")))
		return err
	}); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirectEdit(w, r, "ok", fmt.Sprintf("已新增天气（src_pk=%d）", nextSrc))
}

// ---------- 辅助 ----------

// resolveActivity 把表单里的活动 id 解析为 (activity_id, isHome, isWork)。未选择时返回 nil/false。
func (s *Server) resolveActivity(ctx context.Context, datasetID int64, raw string) (any, bool, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return nil, false, false
	}
	var home, work bool
	if err := s.db.QueryRowContext(ctx, `SELECT is_home, is_work FROM activities WHERE id=$1 AND dataset_id=$2`,
		id, datasetID).Scan(&home, &work); err != nil {
		return nil, false, false
	}
	return id, home, work
}

// execer 是 *sql.DB 与 *sql.Tx 的公共子集。
// 「建地点 + 建到访」必须落在同一个事务里，这些写方法因此不能写死用连接池。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// withTx 在一个事务里执行 fn：fn 返回错误就整体回滚，避免只落一半（比如地点建了、到访没建，
// 导出去在 rond 里就是一个永远没有活跃时间的点）。
func (s *Server) withTx(ctx context.Context, fn func(ex execer) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// insertVisit 插入一条手动补录的到访（带标签），返回其 src_pk。
// 「新增到访」与「新增地点时顺带建到访」共用，保证两条路径的落库行为一致。
func (s *Server) insertVisit(ctx context.Context, ex execer, datasetID, userID, placeID int64,
	arrival time.Time, departure *time.Time, actID any, isHome, isWork, bookmarked bool,
	remark, emoji string, tags []string) (int, error) {
	// 与地点同一套编号规则：留档里的 skipped 行可能占着比 visits 更大的号
	nextSrc, err := s.nextSrcPK(ctx, ex, datasetID, visitSeq)
	if err != nil {
		return 0, err
	}
	if err := clearStaleRaw(ctx, ex, datasetID, "ZVISIT", nextSrc); err != nil {
		return 0, err
	}
	if _, err := ex.ExecContext(ctx, `INSERT INTO visits
		(dataset_id, user_id, src_pk, place_id, activity_id, arrival, departure, duration_min, is_home, is_work,
		 bookmarked, is_user_added, remark, emoji)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,TRUE,NULLIF($12,''),NULLIF($13,''))`,
		datasetID, userID, nextSrc, placeID, actID, arrival, departure, durationMin(arrival, departure), isHome, isWork,
		bookmarked, remark, emoji); err != nil {
		return 0, err
	}
	s.saveVisitTags(ctx, ex, datasetID, nextSrc, tags)
	s.refreshPlaceAgg(ctx, ex, placeID)
	return nextSrc, nil
}

// saveVisitTags 重写到访的标签关联。
func (s *Server) saveVisitTags(ctx context.Context, ex execer, datasetID int64, visitSrc int, tagIDs []string) {
	var visitID int64
	if err := ex.QueryRowContext(ctx, `SELECT id FROM visits WHERE dataset_id=$1 AND src_pk=$2`, datasetID, visitSrc).Scan(&visitID); err != nil {
		return
	}
	ex.ExecContext(ctx, `DELETE FROM visit_tags WHERE visit_id=$1`, visitID)
	for _, t := range tagIDs {
		id, err := strconv.ParseInt(t, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		ex.ExecContext(ctx, `INSERT INTO visit_tags (visit_id, tag_id)
			SELECT $1, t.id FROM tags t WHERE t.id=$2 AND t.dataset_id=$3 ON CONFLICT DO NOTHING`, visitID, id, datasetID)
	}
}

// refreshPlaceAgg 重算某地点的到访聚合（次数 / 停留 / 首末时间）。
func (s *Server) refreshPlaceAgg(ctx context.Context, ex execer, placeID int64) {
	ex.ExecContext(ctx, `UPDATE places p SET
		visit_count = COALESCE(v.c,0), dwell_minutes = COALESCE(v.d,0),
		first_visit_at = v.f, last_visit_at = v.l
		FROM (SELECT $1::bigint AS pid) x
		LEFT JOIN (SELECT place_id, count(*) c, COALESCE(sum(LEAST(COALESCE(duration_min,0), 10080)),0) d, min(arrival) f, max(arrival) l
			FROM visits WHERE place_id=$1 GROUP BY place_id) v ON v.place_id = x.pid
		WHERE p.id = x.pid`, placeID)
}

// refreshDatasetAgg 重算数据集的到访数 / 地点数 / 时间跨度（页脚「数据截至」与后台 flash 用）。
// 编辑页增删记录只动业务表，不刷新的话 datasets 上的缓存计数会越漂越远。
func (s *Server) refreshDatasetAgg(ctx context.Context, datasetID int64) {
	s.db.ExecContext(ctx, `UPDATE datasets d SET
		visit_count = COALESCE(v.c,0), first_visit_at = v.f, last_visit_at = v.l,
		place_count = COALESCE(p.c,0)
		FROM (SELECT $1::bigint AS did) x
		LEFT JOIN (SELECT dataset_id, count(*) c, min(arrival) f, max(arrival) l
			FROM visits WHERE dataset_id=$1 GROUP BY dataset_id) v ON v.dataset_id = x.did
		LEFT JOIN (SELECT dataset_id, count(*) c
			FROM places WHERE dataset_id=$1 GROUP BY dataset_id) p ON p.dataset_id = x.did
		WHERE d.id = x.did`, datasetID)
}

func parseFloat(v string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// parseLocalDT 解析 datetime-local 的值（本地时区）。
func parseLocalDT(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseVisitTimes 解析到访的到达/离开时间，并校验先后顺序。
func parseVisitTimes(r *http.Request) (time.Time, *time.Time, bool, string) {
	arrival, ok := parseLocalDT(r.PostFormValue("arrival"))
	if !ok {
		return time.Time{}, nil, false, "请填写有效的到达时间"
	}
	// 离开时间可选：补录时常常只知道「什么时候到的」，还没离开或记不清就留空
	var departure *time.Time
	if v := strings.TrimSpace(r.PostFormValue("departure")); v != "" {
		t, ok := parseLocalDT(v)
		if !ok {
			return time.Time{}, nil, false, "离开时间格式不正确"
		}
		if t.Before(arrival) {
			return time.Time{}, nil, false, "离开时间不能早于到达时间"
		}
		departure = &t
	}
	return arrival, departure, true, ""
}

func durationMin(arrival time.Time, departure *time.Time) any {
	if departure == nil {
		return nil
	}
	if d := int(departure.Sub(arrival).Minutes()); d >= 0 {
		return d
	}
	return nil
}

// nullStr 把空串当「没填」存成 NULL，与导入路径的写法保持一致
// （导出时再按留档决定写 NULL 还是空串，见 rond.keepEmptyString）。
func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// redirectEdit 回到数据编辑页并带上提示（保留来源页的筛选与分页）。
func redirectEdit(w http.ResponseWriter, r *http.Request, kind, msg string) {
	back := r.Referer()
	if !strings.Contains(back, "/admin/edit") {
		back = "/admin/edit"
	}
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	http.Redirect(w, r, back+sep+kind+"="+urlQueryEscape(msg), http.StatusSeeOther)
}

// ---------- 坐标换算 ----------

// bearingNames 是方位角到中文方位的映射（每 45° 一个，居中对齐）。
var bearingNames = [8]string{"北", "东北", "东", "东南", "南", "西南", "西", "西北"}

func bearingLabel(deg float64) string {
	if deg < 0 {
		deg += 360
	}
	return bearingNames[int((deg+22.5)/45)%8]
}

// apiCoords 换算一个坐标。站点上的坐标一律是 GCJ-02，但真机里有些点其实存的是
// 原始 GPS（实测 323 个地点里有 18 个 ZRAWLATITUDE 与 ZLATITUDE 完全相同，
// 即当初没做偏移，位置整体偏了 500 多米）。换算放在服务端：geo 包是唯一实现，
// 前端再抄一份迟早会跟导出/迷雾那边不一致。
func (s *Server) apiCoords(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lat, ok1 := parseFloat(q.Get("lat"))
	lon, ok2 := parseFloat(q.Get("lon"))
	if !ok1 || !ok2 || (lat == 0 && lon == 0) {
		s.json(w, map[string]any{"error": "经纬度无效"})
		return
	}
	var nlat, nlon float64
	switch q.Get("mode") {
	case "wgs2gcj":
		nlat, nlon = geo.WGS84ToGCJ02(lat, lon)
	case "gcj2wgs":
		nlat, nlon = geo.GCJ02ToWGS84(lat, lon)
	default:
		s.json(w, map[string]any{"error": "未知的换算方向"})
		return
	}
	dist := geo.Haversine(lat, lon, nlat, nlon) * 1000
	s.json(w, map[string]any{
		"lat": nlat, "lon": nlon,
		"dist_m":  int(dist + 0.5),
		"bearing": bearingLabel(geo.Bearing(lat, lon, nlat, nlon)),
	})
}

// suspectWGS 是判断「这个地点的显示坐标其实是 WGS-84」的 SQL 条件：
// 导入留档里的原始 GPS 与显示坐标完全相同。真机正常会写 gcj(raw)==显示坐标
// （305/323），剩下 18 个 raw 与显示坐标一字不差，就是没做偏移的那批。
// 中国境外的点本来就不该偏移，用外接矩形把它们排除掉，免得误判。
const suspectWGS = `(r.raw->>'ZRAWLATITUDE') ~ '^-?[0-9.]+$'
	AND (r.raw->>'ZRAWLONGITUDE') ~ '^-?[0-9.]+$'
	AND abs((r.raw->>'ZRAWLATITUDE')::float8 - COALESCE(p.lat,0)) < 1e-9
	AND abs((r.raw->>'ZRAWLONGITUDE')::float8 - COALESCE(p.lon,0)) < 1e-9
	AND p.lat BETWEEN 3.86 AND 53.55 AND p.lon BETWEEN 73.66 AND 135.05`

// 地点列表与 entity_raw 的关联：留档里既有原始 GPS（判断坐标系是否没偏移），
// 也有导入时的显示坐标（做「坐标前后变动」对照）。
const placeRawJoin = ` LEFT JOIN entity_raw r
	ON r.dataset_id=p.dataset_id AND r.entity='ZLOCATION' AND r.src_pk=p.src_pk`

// 坐标来源的取值。placeSourceNone 不是库里的值，而是筛选/展示时代表
// 「coord_source 为空」这一档——2026-09 之前建的点都没有留档。
const (
	placeSourceNone = "none"
)

// placeSourceLabels 是来源的展示名，顺序即筛选下拉的顺序。
var placeSourceLabels = []struct{ Value, Label string }{
	{"photo", "照片导入"},
	{"rond", "rond 导入"},
	{"manual", "后台新增"},
}

func validPlaceSource(v string) bool {
	if v == "" || v == placeSourceNone {
		return true
	}
	for _, s := range placeSourceLabels {
		if s.Value == v {
			return true
		}
	}
	return false
}

// coordSysLabel 把坐标系写成一行短标签，给列表用。
func coordSysLabel(v string) string {
	switch v {
	case "wgs84":
		return "WGS-84"
	case "gcj02":
		return "GCJ-02"
	}
	return ""
}

// movementFilterNone 是「只看没有交通方式的行程」这个筛选项的提交值。
const movementFilterNone = "none"

// normalizeDate 把日期收敛成 date 输入框认的 YYYY-MM-DD，认不出来就当没填
// （筛选条件不该让整页 500）。
func normalizeDate(v string) string {
	t, ok := parseDate(strings.TrimSpace(v))
	if !ok {
		return ""
	}
	return t.Format("2006-01-02")
}

// escapeLike 转义 ILIKE 的通配符，否则用户搜 "%" 会命中全部、搜 "_" 会变成任意单字符。
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
