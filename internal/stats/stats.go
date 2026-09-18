// Package stats 汇总所有面向页面的查询。
// 所有查询都以 dataset_id 为根作用域，并共享同一套筛选条件，保证各页面口径一致。
package stats

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// TZ 显示用时区。备份数据全部来自国内，按北京时间切分自然日 / 小时。
const TZ = "Asia/Shanghai"

// maxDwellMin 是统计停留时长时的单次上限（7 天）。
// rond 会把「一直待在同一个地方」记成一条超长到访（实测有 35 天的），
// 不封顶的话一条就能把总时长和平均时长整个带偏；这类记录本身是真的，
// 所以是裁剪而不是剔除。与 handlers_edit.go 的 refreshPlaceAgg 保持一致。
const maxDwellMin = 10080

type Filter struct {
	DatasetID   int64
	From        *time.Time
	To          *time.Time
	ActivityIDs []int64
	City        string
	Province    string
	TagID       int64
	MinDwell    int
	ExcludeHome bool
	ExcludeWork bool
	Keyword     string
	// Regions 是访客的区域限制列表：省 / 市 / 区任意层级，空表示不限制。
	// RegionsExclude 为 true 时是黑名单（命中即排除），false 时是白名单（命中才保留）。
	Regions        []string
	RegionsExclude bool
	// Verdict 非 0 时只保留站长标记为该结论的地点（1 推荐 / -1 踩雷）。
	Verdict int
}

type where struct {
	conds []string
	args  []any
	// off 是占位符起始偏移，用于把同一套条件嵌进外层查询的子查询里。
	off int
}

func (w *where) ph(v any) string {
	w.args = append(w.args, v)
	return fmt.Sprintf("$%d", len(w.args)+w.off)
}

func (w *where) cond(expr string, vals ...any) {
	parts := strings.Split(expr, "?")
	var b strings.Builder
	b.WriteString(parts[0])
	for i, v := range vals {
		b.WriteString(w.ph(v))
		if i+1 < len(parts) {
			b.WriteString(parts[i+1])
		}
	}
	w.conds = append(w.conds, b.String())
}

func (w *where) inList(col string, ids []int64) {
	if len(ids) == 0 {
		return
	}
	ph := make([]string, 0, len(ids))
	for _, id := range ids {
		ph = append(ph, w.ph(id))
	}
	w.conds = append(w.conds, col+" IN ("+strings.Join(ph, ",")+")")
}

// expr 返回不带 WHERE 前缀的条件表达式，供子查询拼接。
func (w *where) expr() string {
	return strings.Join(w.conds, " AND ")
}

func (w *where) SQL() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + w.expr()
}

func (w *where) andSQL() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " AND " + strings.Join(w.conds, " AND ")
}

// visitsWhere 构造作用于 visits 表别名 v 的过滤条件。
// off 用于嵌套：子查询里的占位符从 off+1 开始编号，避免与外层冲突。
func (f Filter) visitsWhere(off int) *where {
	w := &where{off: off}
	w.cond("v.dataset_id = ?", f.DatasetID)
	if f.From != nil {
		w.cond("v.arrival >= ?", *f.From)
	}
	if f.To != nil {
		w.cond("v.arrival < ?", *f.To)
	}
	w.inList("v.activity_id", f.ActivityIDs)
	if f.MinDwell > 0 {
		w.cond("COALESCE(v.duration_min,0) >= ?", f.MinDwell)
	}
	if f.ExcludeHome {
		w.cond("NOT v.is_home")
	}
	if f.ExcludeWork {
		w.cond("NOT v.is_work")
	}
	if f.City != "" {
		w.cond("EXISTS (SELECT 1 FROM places px WHERE px.id=v.place_id AND px.city = ?)", f.City)
	}
	if f.Province != "" {
		w.cond("EXISTS (SELECT 1 FROM places px WHERE px.id=v.place_id AND px.province = ?)", f.Province)
	}
	if f.TagID > 0 {
		w.cond("EXISTS (SELECT 1 FROM visit_tags vtx WHERE vtx.visit_id=v.id AND vtx.tag_id = ?)", f.TagID)
	}
	if len(f.Regions) > 0 {
		// 省 / 市 / 区三级各来一份占位符：填「广东省」命中全省，填「贵阳市」只命中该市。
		// 这里不用 COALESCE：条件包在 EXISTS 里，NULL 只是让子查询查不到行，
		// 「NOT EXISTS」照样能正确保留不匹配的记录。
		var prov, city, dist []string
		for _, r := range f.Regions {
			prov = append(prov, w.ph(r))
			city = append(city, w.ph(r))
			dist = append(dist, w.ph(r))
		}
		expr := `EXISTS (SELECT 1 FROM places pxr WHERE pxr.id=v.place_id
			AND (pxr.province IN (` + strings.Join(prov, ",") + `) OR pxr.city IN (` +
			strings.Join(city, ",") + `) OR pxr.district IN (` + strings.Join(dist, ",") + `)))`
		if f.RegionsExclude {
			expr = "NOT " + expr
		}
		w.conds = append(w.conds, expr)
	}
	if f.Keyword != "" {
		kw := "%" + f.Keyword + "%"
		w.cond(`EXISTS (SELECT 1 FROM places px WHERE px.id=v.place_id
			AND (px.name ILIKE ? OR px.city ILIKE ? OR px.poi_category ILIKE ?))`, kw, kw, kw)
	}
	return w
}

// hasVisitConds 判断除时间范围外，是否还有「必须落到某次到访上」才能判定的条件。
//
// 用于天气：删掉到访时 weather.visit_src 会被置空而不是删行（见 editVisitDelete），
// 这些失去归属的天气行仍然属于「某段时间里经历的天气」。所以只按时间筛选时不该
// 额外要求它们能关联到到访，否则一删到访、那几天的天气就凭空少一截。
func (f Filter) hasVisitConds() bool {
	return len(f.ActivityIDs) > 0 || f.MinDwell > 0 || f.ExcludeHome || f.ExcludeWork ||
		f.City != "" || f.Province != "" || f.TagID > 0 || len(f.Regions) > 0 || f.Keyword != ""
}

// weatherWhere 构造作用于 weather 表别名 w 的过滤条件。
//
// 时间用**观测时刻 w.at**（不是所属到访的 arrival）：筛一段时间，要的是「这段时间里
// 经历的天气」，横跨边界的那次到访也只算落在区间内的那些小时。
// 城市 / 类型 / 标签 / 区域 / 关键词这些条件则挂到它所属的那次到访上判定
// （实测 557/557 行都带 visit_src 且能落到 places）。
func (f Filter) weatherWhere() *where {
	w := &where{}
	w.cond("w.dataset_id = ?", f.DatasetID)
	if f.From != nil {
		w.cond("w.at >= ?", *f.From)
	}
	if f.To != nil {
		w.cond("w.at < ?", *f.To)
	}
	if f.hasVisitConds() {
		rest := f
		rest.From, rest.To = nil, nil // 时间已经按 w.at 判过了
		vw := rest.visitsWhere(len(w.args))
		w.conds = append(w.conds,
			"EXISTS (SELECT 1 FROM visits v WHERE v.src_pk = w.visit_src AND "+vw.expr()+")")
		w.args = append(w.args, vw.args...)
	}
	return w
}

// movementsWhere 构造作用于 movements 表的过滤条件（off 的语义同 visitsWhere）。
//
// 与 visitsWhere 一样必须带区域限制：movements 本身没有城市字段，要按两端的 places
// 判定——任一端落在被隐藏的区域，这段里程 / 这次出行就不该出现在访客看到的数据里
// （地图上的轨迹图层用的是同一套判定）。
func (f Filter) movementsWhere(off int) *where {
	w := &where{off: off}
	w.cond("movements.dataset_id = ?", f.DatasetID)
	if f.From != nil {
		w.cond("movements.started_at >= ?", *f.From)
	}
	if f.To != nil {
		w.cond("movements.started_at < ?", *f.To)
	}
	if f.ExcludeHome || f.ExcludeWork {
		var flags []string
		if f.ExcludeHome {
			flags = append(flags, "vf.is_home")
		}
		if f.ExcludeWork {
			flags = append(flags, "vf.is_work")
		}
		w.cond(`NOT EXISTS (
			SELECT 1 FROM visits vf WHERE vf.dataset_id = movements.dataset_id
			AND (vf.src_pk = movements.from_visit_src OR vf.src_pk = movements.to_visit_src)
			AND (` + strings.Join(flags, " OR ") + `))`)
	}
	if len(f.Regions) > 0 {
		var prov, city, dist []string
		for _, r := range f.Regions {
			prov = append(prov, w.ph(r))
			city = append(city, w.ph(r))
			dist = append(dist, w.ph(r))
		}
		expr := `EXISTS (SELECT 1 FROM places pmv
			WHERE pmv.id IN (movements.from_place_id, movements.to_place_id)
			AND (pmv.province IN (` + strings.Join(prov, ",") + `) OR pmv.city IN (` +
			strings.Join(city, ",") + `) OR pmv.district IN (` + strings.Join(dist, ",") + `)))`
		if f.RegionsExclude {
			expr = "NOT " + expr
		}
		w.conds = append(w.conds, expr)
	}
	return w
}

type DB struct{ *sql.DB }

// ---------- 总览 ----------

type Overview struct {
	Places     int
	Cities     int
	Provinces  int
	Days       int
	Visits     int
	DwellMin   int64
	DistanceKm float64
	Movements  int
	FirstAt    *time.Time
	LastAt     *time.Time
}

func (d DB) Overview(ctx context.Context, f Filter) (Overview, error) {
	var o Overview
	w := f.visitsWhere(0)
	q := `SELECT count(DISTINCT v.place_id),
		count(DISTINCT p.city), count(DISTINCT p.province),
		count(DISTINCT (v.arrival AT TIME ZONE '` + TZ + `')::date),
		count(*), COALESCE(sum(LEAST(COALESCE(v.duration_min,0), 10080)),0), min(v.arrival), max(v.arrival)
		FROM visits v LEFT JOIN places p ON p.id = v.place_id` + w.SQL()
	err := d.QueryRowContext(ctx, q, w.args...).Scan(&o.Places, &o.Cities, &o.Provinces,
		&o.Days, &o.Visits, &o.DwellMin, &o.FirstAt, &o.LastAt)
	if err != nil {
		return o, err
	}

	mw := f.movementsWhere(0)
	if err := d.QueryRowContext(ctx, `SELECT COALESCE(sum(distance_km),0), count(*) FROM movements`+mw.SQL(),
		mw.args...).Scan(&o.DistanceKm, &o.Movements); err != nil {
		return o, err
	}
	return o, nil
}

// ---------- 类型分布 ----------

type ActivityStat struct {
	ID     int64
	Name   string
	Color  string
	Icon   string
	Count  int
	Dwell  int64
	Places int
}

func (d DB) ActivityStats(ctx context.Context, f Filter) ([]ActivityStat, error) {
	w := f.visitsWhere(0)
	q := `SELECT COALESCE(a.id,0), COALESCE(a.name,'未分类'), COALESCE(a.color,''),
		COALESCE(a.icon,''), count(*), COALESCE(sum(LEAST(COALESCE(v.duration_min,0), 10080)),0),
		count(DISTINCT v.place_id)
		FROM visits v LEFT JOIN activities a ON a.id = v.activity_id` + w.SQL() + `
		GROUP BY 1,2,3,4 ORDER BY 5 DESC`
	rows, err := d.QueryContext(ctx, q, w.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActivityStat
	for rows.Next() {
		var a ActivityStat
		if err := rows.Scan(&a.ID, &a.Name, &a.Color, &a.Icon, &a.Count, &a.Dwell, &a.Places); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Activities 列出全部活动类型，供筛选器使用。
func (d DB) Activities(ctx context.Context, datasetID int64) ([]ActivityStat, error) {
	rows, err := d.QueryContext(ctx, `SELECT a.id, a.name, COALESCE(a.color,''), COALESCE(a.icon,''),
		(SELECT count(*) FROM visits v WHERE v.activity_id=a.id) n,
		COALESCE((SELECT sum(LEAST(COALESCE(v.duration_min,0), 10080)) FROM visits v WHERE v.activity_id=a.id),0)
		FROM activities a WHERE a.dataset_id=$1 ORDER BY n DESC, a.name`, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActivityStat
	for rows.Next() {
		var a ActivityStat
		if err := rows.Scan(&a.ID, &a.Name, &a.Color, &a.Icon, &a.Count, &a.Dwell); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ActivityCounts 返回「类型 → 到访次数」，给筛选器上的类型标签显示数字用。
//
// 用的是**除类型自身以外**的全部当前条件。以前标签上的数字取自 Activities(datasetID)、
// 一个条件都不带，选了城市之后还是全库的数（站长实测反馈：「这个城市下家/工作各去过
// 几次」看不出来）；但也不能把 ActivityIDs 一起套进去——那样没选中的类型会全变成 0，
// 标签就失去意义了。
// 区域限制照常生效（Regions 在 visitsWhere 里），访客不该从计数反推被隐藏区域的量级。
func (d DB) ActivityCounts(ctx context.Context, f Filter) (map[int64]int, error) {
	f.ActivityIDs = nil
	w := f.visitsWhere(0)
	rows, err := d.QueryContext(ctx,
		`SELECT COALESCE(v.activity_id,0), count(*) FROM visits v`+w.SQL()+` GROUP BY 1`, w.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ---------- 城市 / 省份 ----------

type CityStat struct {
	City   string
	Count  int
	Places int
	Dwell  int64
}

func (d DB) CityStats(ctx context.Context, f Filter, limit int) ([]CityStat, error) {
	w := f.visitsWhere(0)
	w.cond("p.city IS NOT NULL AND p.city <> ''")
	q := `SELECT p.city, count(*), count(DISTINCT v.place_id), COALESCE(sum(LEAST(COALESCE(v.duration_min,0), 10080)),0)
		FROM visits v JOIN places p ON p.id = v.place_id` + w.SQL() + `
		GROUP BY p.city ORDER BY 2 DESC LIMIT ` + fmt.Sprint(limit)
	rows, err := d.QueryContext(ctx, q, w.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CityStat
	for rows.Next() {
		var c CityStat
		if err := rows.Scan(&c.City, &c.Count, &c.Places, &c.Dwell); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type ProvinceStat struct {
	Province string
	Cities   int
	Count    int
	Places   int
}

func (d DB) ProvinceStats(ctx context.Context, f Filter) ([]ProvinceStat, error) {
	w := f.visitsWhere(0)
	w.cond("p.province IS NOT NULL AND p.province <> ''")
	q := `SELECT p.province, count(DISTINCT p.city), count(*), count(DISTINCT v.place_id)
		FROM visits v JOIN places p ON p.id = v.place_id` + w.SQL() + `
		GROUP BY p.province ORDER BY 3 DESC`
	rows, err := d.QueryContext(ctx, q, w.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProvinceStat
	for rows.Next() {
		var p ProvinceStat
		if err := rows.Scan(&p.Province, &p.Cities, &p.Count, &p.Places); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Cities 返回去重后的城市名列表（按到访地点数降序），供筛选下拉框使用。
// regions 非空时对访客按区域过滤：exclude=true 为黑名单（命中即排除），
// false 为白名单（命中才保留）。省/市/区三级都参与匹配，且必须 COALESCE——
// district 常为 NULL，NULL = 'x' 得到 NULL，黑名单的 NOT (...) 会变 NULL，
// 整行被静默丢弃。传空 regions 表示不限制。
func (d DB) Cities(ctx context.Context, datasetID int64, regions []string, exclude bool) ([]string, error) {
	args := []any{datasetID}
	regSQL := ""
	if len(regions) > 0 {
		var conds []string
		for _, r := range regions {
			args = append(args, r)
			ph := fmt.Sprintf("$%d", len(args))
			conds = append(conds, "COALESCE(p.province,'') = "+ph,
				"COALESCE(p.city,'') = "+ph, "COALESCE(p.district,'') = "+ph)
		}
		cond := "(" + strings.Join(conds, " OR ") + ")"
		if exclude {
			cond = "NOT " + cond
		}
		regSQL = " AND " + cond
	}
	rows, err := d.QueryContext(ctx, `SELECT p.city FROM places p WHERE p.dataset_id=$1 AND p.city IS NOT NULL AND p.city<>''`+regSQL+`
		GROUP BY p.city ORDER BY count(*) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------- 时间分布 ----------

type Bucket struct {
	Key   string
	Label string
	Count int
	Dwell int64
}

func (d DB) Monthly(ctx context.Context, f Filter) ([]Bucket, error) {
	w := f.visitsWhere(0)
	q := `SELECT to_char(date_trunc('month', v.arrival AT TIME ZONE '` + TZ + `'), 'YYYY-MM'),
		count(*), COALESCE(sum(LEAST(COALESCE(v.duration_min,0), 10080)),0)
		FROM visits v` + w.SQL() + ` GROUP BY 1 ORDER BY 1`
	return d.buckets(ctx, q, w.args, false)
}

// Calendar 返回每天的记录数，用于活跃日历。
func (d DB) Calendar(ctx context.Context, f Filter) (map[string]int, error) {
	w := f.visitsWhere(0)
	q := `SELECT to_char((v.arrival AT TIME ZONE '` + TZ + `')::date, 'YYYY-MM-DD'), count(*)
		FROM visits v` + w.SQL() + ` GROUP BY 1`
	rows, err := d.QueryContext(ctx, q, w.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

func (d DB) Hourly(ctx context.Context, f Filter) ([]Bucket, error) {
	w := f.visitsWhere(0)
	q := `SELECT to_char(extract(hour from v.arrival AT TIME ZONE '` + TZ + `')::int, 'FM00'),
		count(*), COALESCE(sum(LEAST(COALESCE(v.duration_min,0), 10080)),0)
		FROM visits v` + w.SQL() + ` GROUP BY 1 ORDER BY 1`
	return d.buckets(ctx, q, w.args, false)
}

func (d DB) Weekday(ctx context.Context, f Filter) ([]Bucket, error) {
	w := f.visitsWhere(0)
	q := `SELECT extract(isodow from v.arrival AT TIME ZONE '` + TZ + `')::int::text,
		count(*), COALESCE(sum(LEAST(COALESCE(v.duration_min,0), 10080)),0)
		FROM visits v` + w.SQL() + ` GROUP BY 1 ORDER BY 1`
	b, err := d.buckets(ctx, q, w.args, false)
	if err != nil {
		return nil, err
	}
	names := map[string]string{"1": "周一", "2": "周二", "3": "周三", "4": "周四", "5": "周五", "6": "周六", "7": "周日"}
	for i := range b {
		b[i].Label = names[b[i].Key]
	}
	return b, nil
}

func (d DB) buckets(ctx context.Context, q string, args []any, withLabel bool) ([]Bucket, error) {
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Key, &b.Count, &b.Dwell); err != nil {
			return nil, err
		}
		b.Label = b.Key
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---------- 出行方式 ----------

type TransportStat struct {
	Name     string
	Color    string
	Icon     string
	Count    int
	Distance float64
	DwellMin int64
}

func (d DB) TransportStats(ctx context.Context, f Filter) ([]TransportStat, error) {
	w := f.movementsWhere(0)
	q := `SELECT COALESCE(transport_name,'未知'), COALESCE(transport_color,''), COALESCE(transport_icon,''),
		count(*), COALESCE(sum(distance_km),0), COALESCE(sum(duration_min),0)
		FROM movements` + w.SQL() + ` GROUP BY 1,2,3 ORDER BY 4 DESC`
	rows, err := d.QueryContext(ctx, q, w.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TransportStat
	for rows.Next() {
		var t TransportStat
		if err := rows.Scan(&t.Name, &t.Color, &t.Icon, &t.Count, &t.Distance, &t.DwellMin); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------- 标签 ----------

type TagStat struct {
	ID    int64
	Name  string
	Color string
	Group string
	Count int
}

func (d DB) TagStats(ctx context.Context, f Filter) ([]TagStat, error) {
	w := f.visitsWhere(0)
	q := `SELECT t.id, t.name, COALESCE(t.color,''), COALESCE(t.group_name,''), count(*)
		FROM visit_tags vt JOIN tags t ON t.id = vt.tag_id JOIN visits v ON v.id = vt.visit_id` + w.SQL() + `
		GROUP BY 1,2,3,4 ORDER BY 5 DESC`
	rows, err := d.QueryContext(ctx, q, w.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TagStat
	for rows.Next() {
		var t TagStat
		if err := rows.Scan(&t.ID, &t.Name, &t.Color, &t.Group, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (d DB) Tags(ctx context.Context, datasetID int64) ([]TagStat, error) {
	rows, err := d.QueryContext(ctx, `SELECT t.id, t.name, COALESCE(t.color,''), COALESCE(t.group_name,''),
		(SELECT count(*) FROM visit_tags vt WHERE vt.tag_id=t.id)
		FROM tags t WHERE t.dataset_id=$1 ORDER BY 5 DESC, 2`, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TagStat
	for rows.Next() {
		var t TagStat
		if err := rows.Scan(&t.ID, &t.Name, &t.Color, &t.Group, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------- 地点 ----------

type Place struct {
	ID           int64
	Name         string
	POICategory  string
	Lat          float64
	Lon          float64
	City         string
	Province     string
	District     string
	Timezone     string
	VisitCount   int
	DwellMinutes int64
	FirstAt      *time.Time
	LastAt       *time.Time
	TopActivity  string
	TopColor     string
	TopIcon      string
	IsHome       bool
	IsWork       bool
	// SrcPK 是 rond 内部的地点 ID，跨数据集快照稳定，用于挂备注与别名
	SrcPK int
	// Alias / Note 来自 place_notes：别名对外替代原名显示，备注仅站长可见
	Alias string
	Note  string
	// Verdict / Up / Down 是「推荐 / 踩雷」的站长结论与访客票数，
	// 由查询后单独填充（它们按 src_pk 存在另外两张表里）。
	Verdict int
	Up      int
	Down    int
}

const placeCols = `p.id, COALESCE(p.name,''), COALESCE(p.poi_category,''), COALESCE(p.lat,0), COALESCE(p.lon,0),
	COALESCE(p.city,''), COALESCE(p.province,''), COALESCE(p.district,''), COALESCE(p.timezone,''),
	p.visit_count, p.dwell_minutes, p.first_visit_at, p.last_visit_at,
	COALESCE((SELECT a.name FROM visits v2 JOIN activities a ON a.id=v2.activity_id
		WHERE v2.place_id=p.id GROUP BY a.name ORDER BY count(*) DESC LIMIT 1),''),
	COALESCE((SELECT a.color FROM visits v2 JOIN activities a ON a.id=v2.activity_id
		WHERE v2.place_id=p.id GROUP BY a.color ORDER BY count(*) DESC LIMIT 1),''),
	COALESCE((SELECT a.icon FROM visits v2 JOIN activities a ON a.id=v2.activity_id
		WHERE v2.place_id=p.id GROUP BY a.icon ORDER BY count(*) DESC LIMIT 1),''),
	EXISTS (SELECT 1 FROM visits v3 WHERE v3.place_id=p.id AND v3.is_home),
	EXISTS (SELECT 1 FROM visits v4 WHERE v4.place_id=p.id AND v4.is_work),
	p.src_pk`

// PlaceCols 与 ScanPlace 配对，供包外构造只取单个地点的查询。
const PlaceCols = placeCols

func scanPlace(s interface{ Scan(...any) error }) (Place, error) {
	var p Place
	err := s.Scan(&p.ID, &p.Name, &p.POICategory, &p.Lat, &p.Lon, &p.City, &p.Province,
		&p.District, &p.Timezone, &p.VisitCount, &p.DwellMinutes, &p.FirstAt, &p.LastAt,
		&p.TopActivity, &p.TopColor, &p.TopIcon, &p.IsHome, &p.IsWork, &p.SrcPK)
	return p, err
}

// ScanPlace 把一行按 placeCols 列序扫描进 Place。
func ScanPlace(s interface{ Scan(...any) error }, p *Place) error {
	got, err := scanPlace(s)
	if err != nil {
		return err
	}
	*p = got
	return nil
}

// Places 分页返回地点库；筛选条件最终落在 visits 上，确保与地图口径一致。
func (d DB) Places(ctx context.Context, f Filter, sortBy string, limit, offset int) ([]Place, int, error) {
	inner := f.visitsWhere(1)
	inner.cond("v.place_id = p.id")
	order := "p.visit_count DESC, p.dwell_minutes DESC, p.name"
	switch sortBy {
	case "dwell":
		order = "p.dwell_minutes DESC, p.visit_count DESC"
	case "recent":
		order = "p.last_visit_at DESC NULLS LAST"
	case "name":
		order = "p.name"
	case "first":
		order = "p.first_visit_at ASC NULLS LAST"
	}

	args := append([]any{f.DatasetID}, inner.args...)
	base := `FROM places p WHERE p.dataset_id = $1 AND EXISTS (SELECT 1 FROM visits v WHERE ` + inner.expr() + `)`
	if f.Verdict != 0 {
		// 结论存在 place_votes，按 src_pk 关联；占位符接在内层条件之后。
		// dataset_id 也要对上：src_pk 只在单个数据集内唯一。
		args = append(args, f.Verdict)
		base += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM place_votes pv
			WHERE pv.src_pk = p.src_pk AND pv.dataset_id = p.dataset_id AND pv.verdict = $%d)`, len(args))
	}
	q := `SELECT ` + placeCols + ` ` + base + ` ORDER BY ` + order + fmt.Sprintf(` LIMIT %d OFFSET %d`, limit, offset)

	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Place
	for rows.Next() {
		p, err := scanPlace(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) `+base, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// MapPlaces 返回地图所需的轻量地点集合。
func (d DB) MapPlaces(ctx context.Context, f Filter, limit int) ([]Place, error) {
	inner := f.visitsWhere(1)
	inner.cond("v.place_id = p.id")
	q := `SELECT ` + placeCols + ` FROM places p WHERE p.dataset_id = $1
		AND EXISTS (SELECT 1 FROM visits v WHERE ` + inner.expr() + `)
		ORDER BY p.visit_count DESC LIMIT ` + fmt.Sprint(limit)
	args := append([]any{f.DatasetID}, inner.args...)
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Place
	for rows.Next() {
		p, err := scanPlace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d DB) Place(ctx context.Context, datasetID, id int64) (Place, error) {
	q := `SELECT ` + placeCols + ` FROM places p WHERE p.dataset_id=$1 AND p.id=$2`
	return scanPlace(d.QueryRowContext(ctx, q, datasetID, id))
}

// ---------- 访问记录 ----------

type VisitRow struct {
	ID           int64
	SrcPK        int
	Arrival      time.Time
	Departure    *time.Time
	DurationMin  int
	PlaceID      *int64
	PlaceName    string
	City         string
	POICategory  string
	ActivityName string
	Color        string
	Icon         string
	IsHome       bool
	IsWork       bool
	Remark       string
	Tags         []string
}

const visitCols = `v.id, v.src_pk, v.arrival, v.departure, COALESCE(v.duration_min,0),
	v.place_id, COALESCE(p.name,''), COALESCE(p.city,''), COALESCE(p.poi_category,''),
	COALESCE(a.name,''), COALESCE(a.color,''), COALESCE(a.icon,''),
	v.is_home, v.is_work, COALESCE(v.remark,''),
	COALESCE((SELECT string_agg(t.name, '、' ORDER BY t.name) FROM visit_tags vt JOIN tags t ON t.id=vt.tag_id
		WHERE vt.visit_id = v.id), '')`

func scanVisit(s interface{ Scan(...any) error }) (VisitRow, error) {
	var v VisitRow
	var tags string
	err := s.Scan(&v.ID, &v.SrcPK, &v.Arrival, &v.Departure, &v.DurationMin,
		&v.PlaceID, &v.PlaceName, &v.City, &v.POICategory,
		&v.ActivityName, &v.Color, &v.Icon, &v.IsHome, &v.IsWork, &v.Remark, &tags)
	if tags != "" {
		v.Tags = strings.Split(tags, "、")
	}
	return v, err
}

func (d DB) Visits(ctx context.Context, f Filter, limit, offset int, order string) ([]VisitRow, int, error) {
	w := f.visitsWhere(0)
	dir := "DESC"
	if order == "asc" {
		dir = "ASC"
	}
	q := `SELECT ` + visitCols + ` FROM visits v
		LEFT JOIN places p ON p.id = v.place_id
		LEFT JOIN activities a ON a.id = v.activity_id` + w.SQL() +
		` ORDER BY v.arrival ` + dir + fmt.Sprintf(` LIMIT %d OFFSET %d`, limit, offset)
	rows, err := d.QueryContext(ctx, q, w.args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []VisitRow
	for rows.Next() {
		v, err := scanVisit(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	cq := `SELECT count(*) FROM visits v` + w.SQL()
	if err := d.QueryRowContext(ctx, cq, w.args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// RecentVisits 最近足迹，主页使用。
func (d DB) RecentVisits(ctx context.Context, f Filter, limit int) ([]VisitRow, error) {
	w := f.visitsWhere(0)
	q := `SELECT ` + visitCols + ` FROM visits v
		LEFT JOIN places p ON p.id = v.place_id
		LEFT JOIN activities a ON a.id = v.activity_id` + w.SQL() +
		` ORDER BY v.arrival DESC LIMIT ` + fmt.Sprint(limit)
	rows, err := d.QueryContext(ctx, q, w.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VisitRow
	for rows.Next() {
		v, err := scanVisit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (d DB) VisitsByPlace(ctx context.Context, datasetID, placeID int64, limit int) ([]VisitRow, error) {
	q := `SELECT ` + visitCols + ` FROM visits v
		LEFT JOIN places p ON p.id = v.place_id
		LEFT JOIN activities a ON a.id = v.activity_id
		WHERE v.dataset_id=$1 AND v.place_id=$2 ORDER BY v.arrival DESC LIMIT ` + fmt.Sprint(limit)
	rows, err := d.QueryContext(ctx, q, datasetID, placeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VisitRow
	for rows.Next() {
		v, err := scanVisit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (d DB) Bookmarked(ctx context.Context, datasetID int64, limit int) ([]VisitRow, error) {
	q := `SELECT ` + visitCols + ` FROM visits v
		LEFT JOIN places p ON p.id = v.place_id
		LEFT JOIN activities a ON a.id = v.activity_id
		WHERE v.dataset_id=$1 AND v.bookmarked ORDER BY v.arrival DESC LIMIT ` + fmt.Sprint(limit)
	rows, err := d.QueryContext(ctx, q, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VisitRow
	for rows.Next() {
		v, err := scanVisit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---------- 天气 ----------

type WeatherStat struct {
	Condition string
	Symbol    string
	Count     int
	AvgTemp   float64
	MinTemp   float64
	MaxTemp   float64
}

func (d DB) WeatherStats(ctx context.Context, f Filter) ([]WeatherStat, error) {
	// 这里以前只用了 f.DatasetID，筛选（时间范围 / 城市 / 类型 …）对它完全不起作用——
	// 统计页筛完之后「气温」还是全量汇总，就是这么来的。
	w := f.weatherWhere()
	q := `SELECT COALESCE(w.condition,'未知'), COALESCE(w.symbol,''), count(*),
		COALESCE(avg(w.temperature_c),0)-273.15, COALESCE(min(w.temperature_c),0)-273.15, COALESCE(max(w.temperature_c),0)-273.15
		FROM weather w` + w.SQL() + ` GROUP BY 1,2 ORDER BY 3 DESC LIMIT 12`
	rows, err := d.QueryContext(ctx, q, w.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WeatherStat
	for rows.Next() {
		var w WeatherStat
		if err := rows.Scan(&w.Condition, &w.Symbol, &w.Count, &w.AvgTemp, &w.MinTemp, &w.MaxTemp); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// TempBucket 是逐月平均气温。
//
// 原先这里借用 Bucket，把「平均气温」塞进了 Dwell 字段（因为 buckets() 的第三列一律读成
// Dwell），读代码的人根本猜不到，所以单开一个结构。
type TempBucket struct {
	Key   string  // 2026-09
	Label string  // 标签用短写（26-09），十几根柱子横排时 7 个字符会挤在一起
	Hours int     // 这个月有多少小时有记录
	AvgC  float64 // 平均气温（摄氏度）
}

// HourlyTemp 逐月平均气温（同样跟着筛选走）。
func (d DB) HourlyTemp(ctx context.Context, f Filter) ([]TempBucket, error) {
	w := f.weatherWhere()
	w.conds = append(w.conds, "w.temperature_c IS NOT NULL")
	q := `SELECT to_char(date_trunc('month', w.at AT TIME ZONE '` + TZ + `'), 'YYYY-MM'),
		count(*), COALESCE(avg(w.temperature_c)-273.15, 0)
		FROM weather w` + w.SQL() + ` GROUP BY 1 ORDER BY 1`
	rows, err := d.QueryContext(ctx, q, w.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TempBucket
	for rows.Next() {
		var b TempBucket
		if err := rows.Scan(&b.Key, &b.Hours, &b.AvgC); err != nil {
			return nil, err
		}
		b.AvgC = math.Round(b.AvgC*10) / 10
		if len(b.Key) >= 7 { // 2026-09 → 26-09
			b.Label = b.Key[2:]
		} else {
			b.Label = b.Key
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ConditionName 把 Apple 的天气枚举翻成中文。
func ConditionName(c string) string {
	m := map[string]string{
		"clear": "晴", "mostlyClear": "晴间多云", "partlyCloudy": "多云", "mostlyCloudy": "阴天多云",
		"cloudy": "阴", "foggy": "有雾", "haze": "霾", "mostlyCloudyNight": "夜间多云",
		"drizzle": "毛毛雨", "rain": "雨", "heavyRain": "大雨", "showers": "阵雨",
		"thunderstorms": "雷雨", "snow": "雪", "heavySnow": "大雪", "sleet": "雨夹雪",
		"windy": "大风", "blizzard": "暴风雪", "freezingRain": "冻雨", "hot": "高温", "cold": "寒冷",
	}
	if v, ok := m[c]; ok {
		return v
	}
	return c
}
