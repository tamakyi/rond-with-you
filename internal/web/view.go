package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"rond-with-you/internal/auth"
	"rond-with-you/internal/fog"
	"rond-with-you/internal/setting"
	"rond-with-you/internal/stats"
	"rond-with-you/internal/votes"
)

// SetFlashFromQuery 取重定向带回来的一次性提示（?ok= / ?err=）。
//
// 凡是「处理完跳回本页」的页面都必须调一次：`adminFlash` 之类的帮助函数把消息挂在
// URL 上，页面不读就静默丢了——后台首页就漏过这一句，上传备份、导入迷雾、开两步验证
// 全都没有任何提示。
func (p *Page) SetFlashFromQuery(r *http.Request) {
	q := r.URL.Query()
	p.Flash, p.Err = q.Get("ok"), q.Get("err")
}

// Page 是所有页面共享的上下文（站点设置、登录态、筛选器状态）。
type Page struct {
	Settings setting.Settings
	User     *auth.User
	Nav      string
	Title    string
	Flash    string
	Err      string
	HasData  bool
	Dataset  *Dataset
	// AssetV 是静态资源版本号，链接带 ?v=AssetV 时才允许长期缓存
	AssetV string
	// TrackScript 是站点配置里的第三方统计脚本（已包成可直接输出的 HTML）
	TrackScript template.HTML

	Activities  []stats.ActivityStat
	TagOptions  []stats.TagStat
	CityList    []string
	From        string
	To          string
	ActivityIDs []int64
	City        string
	TagID       int64
	Keyword     string
	MinDwell    int
	Sort        string
	// Verdict 是地点库的「推荐 / 踩雷」筛选（1 推荐 / -1 踩雷 / 0 不限）
	Verdict    int
	PageNo     int
	PageCount  int
	Total      int
	RangeLabel string
	FilterBase string
	Presets    []RangePreset

	// ShowHomeWork 表示本次请求允许包含「家 / 工作」。
	// 只有登录后的站长主动打开开关才为 true，匿名访客拿不到。
	ShowHomeWork bool

	// HasMap 决定是否加载 Leaflet，避免没有地图的页面白下载一份地图库。
	HasMap bool

	// Flatpickr 决定是否加载日期时间选择器（目前只有数据编辑页在用）。
	Flatpickr bool

	// HasTopics 表示该数据集有专题，导航里才露出「专题」入口。
	HasTopics bool
}

// IsAdmin 报告当前访问者是否已登录后台。
func (p Page) IsAdmin() bool { return p.User != nil }

// RangePreset 是筛选器上的快捷时间范围。
type RangePreset struct {
	Label  string
	From   string
	To     string
	Active bool
}

func (p Page) IsAct(id int64) bool {
	for _, v := range p.ActivityIDs {
		if v == id {
			return true
		}
	}
	return false
}

func (p Page) filterPairs() []string {
	var kv []string
	if p.ShowHomeWork {
		kv = append(kv, "hw", "1")
	}
	if p.From != "" {
		kv = append(kv, "from", p.From)
	}
	if p.To != "" {
		kv = append(kv, "to", p.To)
	}
	for _, id := range p.ActivityIDs {
		kv = append(kv, "act", strconv.FormatInt(id, 10))
	}
	if p.City != "" {
		kv = append(kv, "city", p.City)
	}
	if p.TagID > 0 {
		kv = append(kv, "tag", strconv.FormatInt(p.TagID, 10))
	}
	if p.MinDwell > 0 {
		kv = append(kv, "min", strconv.Itoa(p.MinDwell))
	}
	if p.Keyword != "" {
		kv = append(kv, "q", p.Keyword)
	}
	if p.Sort != "" {
		kv = append(kv, "sort", p.Sort)
	}
	if p.Verdict != 0 {
		kv = append(kv, "verdict", strconv.Itoa(p.Verdict))
	}
	return kv
}

// Link 在保留当前筛选条件的前提下生成链接。
// Link 以当前筛选参数为底生成链接。extra 里的键会**覆盖**同名项，而不是追加——
// 否则会拼出 ?sort=dwell&sort=name 这种重复参数，而取参数只认第一个，
// 表现为「点了另一个排序没反应」。extra 里值为空的键会被丢掉，
// 这样「回到默认排序」只要传 sort=""（buildURL 会跳过空值）。
func (p Page) Link(base string, extra ...string) string {
	kv := p.filterPairs()
	drop := map[string]bool{}
	for i := 0; i+1 < len(extra); i += 2 {
		drop[extra[i]] = true
	}
	out := make([]string, 0, len(kv)+len(extra))
	for i := 0; i+1 < len(kv); i += 2 {
		if drop[kv[i]] {
			continue
		}
		out = append(out, kv[i], kv[i+1])
	}
	return buildURL(base, append(out, extra...))
}

// ActLink 点击类型标签时切换该类型的选中状态。
func (p Page) ActLink(id int64) string {
	var kv []string
	has := false
	for _, v := range p.ActivityIDs {
		if v == id {
			has = true
			continue
		}
		kv = append(kv, "act", strconv.FormatInt(v, 10))
	}
	if !has {
		kv = append(kv, "act", strconv.FormatInt(id, 10))
	}
	rest := p.filterPairs()
	for i := 0; i+1 < len(rest); i += 2 {
		if rest[i] != "act" {
			kv = append(kv, rest[i], rest[i+1])
		}
	}
	return buildURL(p.FilterBase, kv)
}

// TagLink 切换标签筛选。
func (p Page) TagLink(id int64) string {
	kv := p.filterPairs()
	out := kv[:0]
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] != "tag" {
			out = append(out, kv[i], kv[i+1])
		}
	}
	if p.TagID != id {
		out = append(out, "tag", strconv.FormatInt(id, 10))
	}
	return buildURL(p.FilterBase, out)
}

// CityLink 切换城市筛选。
func (p Page) CityLink(city string) string {
	kv := p.filterPairs()
	out := kv[:0]
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] != "city" {
			out = append(out, kv[i], kv[i+1])
		}
	}
	if p.City != city {
		out = append(out, "city", city)
	}
	return buildURL(p.FilterBase, out)
}

// VerdictLink 切换「推荐 / 踩雷」筛选（v=1 推荐 / -1 踩雷）。
func (p Page) VerdictLink(v int) string {
	kv := p.filterPairs()
	out := kv[:0]
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] != "verdict" {
			out = append(out, kv[i], kv[i+1])
		}
	}
	if p.Verdict != v {
		out = append(out, "verdict", strconv.Itoa(v))
	}
	return buildURL(p.FilterBase, out)
}

// RangeLink 切换日期区间。
func (p Page) RangeLink(from, to string) string {
	kv := p.filterPairs()
	out := kv[:0]
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] != "from" && kv[i] != "to" {
			out = append(out, kv[i], kv[i+1])
		}
	}
	if from != "" {
		out = append(out, "from", from)
	}
	if to != "" {
		out = append(out, "to", to)
	}
	return buildURL(p.FilterBase, out)
}

// MonthLink 把「2026-09」这样的月份键变成该月的日期区间链接，
// 用于点月度柱条直接把筛选切到那个月。
func (p Page) MonthLink(key string) string {
	t, err := time.Parse("2006-01", key)
	if err != nil {
		return p.FilterBase
	}
	return p.RangeLink(t.Format("2006-01-02"), t.AddDate(0, 1, 0).Format("2006-01-02"))
}

// HWLink 切换「包含家 / 工作」。站点默认隐藏这两个常住地，
// 站长登录后在页面上打开开关即可临时显示。
func (p Page) HWLink() string {
	kv := p.filterPairs()
	out := kv[:0]
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] != "hw" {
			out = append(out, kv[i], kv[i+1])
		}
	}
	if !p.ShowHomeWork {
		out = append(out, "hw", "1")
	}
	return buildURL(p.FilterBase, out)
}

// RemoveFilter 生成「摘掉某个筛选条件」的链接。
// key 传 "date" 时同时摘掉 from 与 to（日期是一组）；
// value 非空时只摘掉多值参数（act）里的这一项，其余保留。
func (p Page) RemoveFilter(key, value string) string {
	kv := p.filterPairs()
	out := kv[:0]
	for i := 0; i+1 < len(kv); i += 2 {
		k, v := kv[i], kv[i+1]
		match := k == key || (key == "date" && (k == "from" || k == "to"))
		if !match {
			out = append(out, k, v)
			continue
		}
		if value != "" && v != value {
			out = append(out, k, v) // 多值参数里只摘掉指定的那个
		}
	}
	return buildURL(p.FilterBase, out)
}

// ActiveChip 是「已选条件」行里的一项。
type ActiveChip struct {
	Label     string
	RemoveURL string
}

// ActiveChips 列出当前生效的筛选，让站长逐个摘掉。
// 以前只能靠「重置」全清——想撤掉其中一个，得手动改回「全部」再点应用。
func (p Page) ActiveChips() []ActiveChip {
	var out []ActiveChip
	if p.From != "" || p.To != "" {
		label := "截至 " + p.To
		switch {
		case p.From != "" && p.To != "":
			label = p.From + " ~ " + p.To
		case p.From != "":
			label = p.From + " 起"
		}
		out = append(out, ActiveChip{Label: label, RemoveURL: p.RemoveFilter("date", "")})
	}
	for _, id := range p.ActivityIDs {
		for _, a := range p.Activities {
			if a.ID == id {
				out = append(out, ActiveChip{Label: a.Name,
					RemoveURL: p.RemoveFilter("act", strconv.FormatInt(id, 10))})
				break
			}
		}
	}
	if p.City != "" {
		out = append(out, ActiveChip{Label: p.City, RemoveURL: p.RemoveFilter("city", "")})
	}
	if p.TagID > 0 {
		for _, t := range p.TagOptions {
			if t.ID == p.TagID {
				out = append(out, ActiveChip{Label: t.Name, RemoveURL: p.RemoveFilter("tag", "")})
				break
			}
		}
	}
	if p.MinDwell > 0 {
		out = append(out, ActiveChip{Label: "≥ " + strconv.Itoa(p.MinDwell) + " 分钟",
			RemoveURL: p.RemoveFilter("min", "")})
	}
	if p.Keyword != "" {
		out = append(out, ActiveChip{Label: "“" + p.Keyword + "”", RemoveURL: p.RemoveFilter("q", "")})
	}
	if p.Verdict != 0 {
		label := "👍 推荐"
		if p.Verdict < 0 {
			label = "👎 踩雷"
		}
		out = append(out, ActiveChip{Label: label, RemoveURL: p.RemoveFilter("verdict", "")})
	}
	if p.ShowHomeWork {
		out = append(out, ActiveChip{Label: "含家/工作", RemoveURL: p.RemoveFilter("hw", "")})
	}
	return out
}

func buildURL(base string, kv []string) string {
	if len(kv) == 0 {
		return base
	}
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			continue
		}
		v.Add(kv[i], kv[i+1])
	}
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

// ---------- 页面数据 ----------

type HomeData struct {
	Page
	Overview      stats.Overview
	ActivityStats []stats.ActivityStat
	CityStats     []stats.CityStat
	Monthly       []MonthBar
	Calendar      []CalWeek
	Transports    []stats.TransportStat
	Tags          []stats.TagStat
	Weather       []stats.WeatherStat
	Recent        []stats.VisitRow
	Places        []stats.Place
	Points        []MapPoint

	MaxActivity  int
	MaxCity      int
	MaxTransport int
	MaxTag       int
}

type MapData struct {
	Page
	Points []MapPoint
	Legend []LegendItem
}

// ModeCount 是轨迹图例的一项：交通方式、段数与颜色。
type ModeCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Color string `json:"color"`
}

// modeFallback 是没记录交通方式的位移段在轨迹上归到的类别（灰）。
const modeFallback = "步行 / 其他"

// LegendItem 是地图右下角图例的一项，按当前筛选结果统计。
type LegendItem struct {
	Name  string
	Color string
	Icon  string
	Count int
}

type PlacesData struct {
	Page
	Places []stats.Place
}

type PlaceData struct {
	Page
	Place  stats.Place
	Visits []stats.VisitRow
	Points []MapPoint
	// Verdict 是站长官方结论(1=推荐 / -1=踩雷 / 0=未标)；Votes 是访客票数；
	// MyVote 是当前访客自己对这一地点的投票。
	Verdict int
	Votes   votes.Tally
	MyVote  int
	// MyNote 是站长手写的备注（编辑框里编辑的就是它）；
	// RondNote 是 rond 备份里带出来的备注，只读，展示时拼在 MyNote 之前。
	MyNote   string
	RondNote string
	// Images 是站长给这个地点配的图片（按 position 排序）。
	Images []PlaceImage
	// ImageBackend 是当前生效的图片存储（local / s3），后台那块面板展示用；
	// ImageReady 为假表示选了对象存储但凭据没配齐，此时不允许上传。
	ImageBackend string
	ImageReady   bool
	// ImageMaxPer 是单个地点的图片张数上限，模板里用来提示
	ImageMaxPer int
}

type TimelineData struct {
	Page
	Days []TimelineDay
}

type StatsData struct {
	Page
	Overview      stats.Overview
	ActivityStats []stats.ActivityStat
	CityStats     []stats.CityStat
	ProvinceStats []stats.ProvinceStat
	Monthly       []MonthBar
	Hourly        []int
	Weekday       []stats.Bucket
	Transports    []stats.TransportStat
	Tags          []stats.TagStat
	Weather       []stats.WeatherStat
	Temps         []TempPoint
	Fog           *FogCoverage
	// 推荐 / 踩雷 汇总与榜单
	VoteRec     int
	VoteAvoid   int
	VoteUp      int
	VoteDown    int
	Recommended []stats.Place
	Avoided     []stats.Place
	MaxMonthly  int
	MaxHourly   int
	MaxActivity int
	MaxCity     int
	MaxWeekday  int
	MaxTag      int
}

// ReportData 是年度报告页的数据。
type ReportData struct {
	Page
	Year       int
	Years      []int
	Overview   stats.Overview
	TopCities  []stats.CityStat
	Transports []stats.TransportStat
	North      *stats.Place
	South      *stats.Place
	NewPlaces  int
	// Cities 是本年度「首次到达」的城市里程碑（按首次到达日期升序）。
	Cities []CityMilestone
	Fog    *FogCoverage
}

// CityMilestone 是年度报告的「首次到达某城」里程碑：该城市有史以来第一次到访落在这一年。
type CityMilestone struct {
	City     string
	Province string
	First    string // 首次到达日期 YYYY-MM-DD
}

type LoginData struct {
	Settings setting.Settings
	Err      string
	Next     string
	Flash    string
}

type AdminData struct {
	Page
	Datasets []Dataset
	MaxSize  int64
	// 迷雾当前状态（未导入时 Blocks 为 0，模板据此显示空态提示）
	FogBlocks    int
	FogCells     int
	FogArea      float64
	FogUpdatedAt *time.Time
	// FogMasks 是遮罩区数量，模板据此在按钮上显示计数
	FogMasks int
	// DefaultPW 表示管理员仍在使用初始口令，模板顶部给出警示条
	DefaultPW bool
}

type NotesData struct {
	Page
	Rows        []NoteRow
	Orphans     []NoteRow // 备注挂在已不在当前数据集的地点上的行
	CityOptions []string
	Query       string
	Status      string // all / has / none
	City        string
	Sort        string
	NotesPublic bool
	// BackURL 是带完整筛选条件的本页地址，用于行内表单保存后跳回原处
	BackURL string
}

// PageLink 生成备注管理页的分页链接，保留当前筛选条件。
func (d NotesData) PageLink(page int) string {
	v := url.Values{}
	if d.Query != "" {
		v.Set("q", d.Query)
	}
	if d.Status != "" && d.Status != "all" {
		v.Set("status", d.Status)
	}
	if d.City != "" {
		v.Set("city", d.City)
	}
	if d.Sort != "" {
		v.Set("sort", d.Sort)
	}
	v.Set("page", strconv.Itoa(page))
	return "/admin/notes?" + v.Encode()
}

// EditData 是数据编辑页的数据：地点 / 到访 / 行程三个可搜索分页列表 + 各类新增表单。
type EditData struct {
	Page
	Activities []EditOption
	Tags       []EditOption
	Transports []EditTransport
	// TransportColors 是 rond 认的那套系统色名，供交通方式选色（顺序固定，便于复用）
	TransportColors []string
	PlaceOptions    []EditOption
	VisitOptions    []EditOption // 最近到访（值=src_pk），供行程/天气表单选关联
	Categories      []EditOption // 已有地点的类别（去重），供新建时下拉直选
	Places          []EditPlace
	Visits          []EditVisit
	Movements       []EditMovement
	Query           string
	// VisitQuery 是到访记录列表的搜索词（与地点列表的 Query 独立）
	VisitQuery string
	// MovementFilter 是行程列表的筛选条件，都是选择框而不是让人手输关键词
	MovementFilter    MovementFilter
	PageNo            int
	PageCount         int
	Total             int
	VisitPageNo       int
	VisitPageCount    int
	VisitTotal        int
	MovementPageNo    int
	MovementPageCount int
	MovementTotal     int
	// SuspectCount 是数据集里「显示坐标疑似就是 WGS-84」的地点总数
	SuspectCount int
	// SuspectOnly 是地点列表「只看疑似 WGS-84」的开关
	SuspectOnly bool
	// SourceFilter 是地点列表按「坐标来源」筛选的当前值（空=全部，none=没有留档）
	SourceFilter string
	// SourceCounts 是各来源的地点数，给筛选下拉显示数量
	SourceCounts map[string]int
	CanExport    bool
	GeocodeReady bool
	// OpenSrcPK 是「从地点详情页跳进来要定位的地点」（?open=<src_pk>）：非 0 时自动翻到
	// 该行所在页、展开它的编辑框并高亮，省得在几百行地点里手动搜
	OpenSrcPK int
}

// PlaceSources 供模板渲染筛选下拉，顺序固定（与 placeSourceLabels 一致）。
func (d EditData) PlaceSources() []struct{ Value, Label string } { return placeSourceLabels }

// SourceCount 取某个来源的点数，没有就是 0（模板里做「（N）」提示用）。
func (d EditData) SourceCount(v string) int { return d.SourceCounts[v] }

// CoordSysLabel 把坐标系标识写成显示用的短名。
func CoordSysLabel(v string) string { return coordSysLabel(v) }

// SourceLabel 把 coord_source 写成显示名；空值表示历史行没有留档。
func SourceLabel(v string) string {
	for _, s := range placeSourceLabels {
		if s.Value == v {
			return s.Label
		}
	}
	return "未留档"
}

// MovementFilter 是行程列表的筛选条件：交通方式 / 一端的地点 / 时间范围。
// 三个都是可选项，空值表示不限。
type MovementFilter struct {
	// Transport 为空=全部、"none"=没有交通方式的那些、数字=transport_src
	Transport string
	// PlaceID 按起或止任一端匹配（movements 上的 from/to_place_id 是导入与新建时都写好的）
	PlaceID int64
	From    string // YYYY-MM-DD
	To      string // YYYY-MM-DD，含当天
}

func (f MovementFilter) set(v url.Values) {
	if f.Transport != "" {
		v.Set("mt", f.Transport)
	}
	if f.PlaceID > 0 {
		v.Set("mp", strconv.FormatInt(f.PlaceID, 10))
	}
	if f.From != "" {
		v.Set("mfrom", f.From)
	}
	if f.To != "" {
		v.Set("mto", f.To)
	}
}

// PageLink 是某个列表翻到第 n 页的地址，带上当前的全部筛选条件（三个列表共用，
// 翻页不会把别的列表的条件丢掉）。
//
// 返回 template.URL 是必须的：href 里再经 html/template 的 URL 转义会把 "=" 编成
// %3d，`mt%3d4` 在服务端解出来是「键为 mt=4」的参数，筛选条件会在翻页时静默失效。
// & 手工写成实体，交给浏览器的解析结果仍是 &。
func (d EditData) PageLink(param string, n int) template.URL {
	v := url.Values{}
	if d.Query != "" {
		v.Set("q", d.Query)
	}
	if d.SuspectOnly {
		v.Set("sus", "1")
	}
	if d.SourceFilter != "" {
		v.Set("src", d.SourceFilter)
	}
	if d.VisitQuery != "" {
		v.Set("vq", d.VisitQuery)
	}
	d.MovementFilter.set(v)
	v.Set(param, strconv.Itoa(n))
	return template.URL(strings.ReplaceAll("/admin/edit?"+v.Encode(), "&", "&amp;"))
}

// EditOption 是表单里的一项选项（活动 / 标签 / 交通 / 地点）。
type EditOption struct {
	ID    int64
	SrcPK int
	Name  string
	Label string
}

type EditPlace struct {
	ID           int64
	SrcPK        int
	Name         string
	Category     string
	Province     string
	City         string
	District     string
	Thoroughfare string
	Lat          float64
	Lon          float64
	VisitCount   int
	// SuspectWGS 为真表示导入留档里的原始 GPS 与显示坐标完全相同——真机里正常会写
	// 「显示坐标 = 原始 GPS 做过 GCJ-02 偏移」，一字不差的就是当初没偏移的那批，
	// 在 GCJ-02 底图上会整体偏 500 米左右。
	SuspectWGS bool
	// OrigLat/OrigLon 是导入留档里的显示坐标（改过之后用来做前后对照）
	OrigLat, OrigLon sql.NullFloat64
	// MovedM 是当前坐标相对导入时的位移（米），0 表示没动过
	MovedM int
	// CoordSource/CoordSys 是建点时留下的来源与坐标系；SrcLat/SrcLon 是换算前的原始坐标。
	// 三者都为空表示历史行没有留档（2026-09 之前的点都是这样）。
	CoordSource    string
	CoordSys       string
	SrcLat, SrcLon sql.NullFloat64
	// CategoryOptions 是这一行类别下拉的选项：库里的已有类别 + 当前值（不在其中时补上）
	CategoryOptions []EditOption
}

// SourceTag 是列表里那一枚来源标记的文案，没有留档时给「未留档」。
func (p EditPlace) SourceTag() string {
	if p.CoordSource == "" {
		return "未留档"
	}
	l := SourceLabel(p.CoordSource)
	if s := coordSysLabel(p.CoordSys); s != "" {
		l += " · " + s
	}
	return l
}

// HasSrcCoord 表示这行是否留下了「换算前的原始坐标」，模板据此决定要不要显示。
func (p EditPlace) HasSrcCoord() bool { return p.SrcLat.Valid && p.SrcLon.Valid }

// EditTransport 是交通方式。站点没有单独维护它——rond 的 ZTRANSPORT 是全局列表，
// 导出时由行程里用到的 transport_src 聚合而来，所以这里也从行程里读。
type EditTransport struct {
	SrcPK int
	Name  string
	Color string
	Icon  string
	Count int
}

// Label 供下拉/列表显示：没有名称时退回编号，免得出现一个空选项。
func (t EditTransport) Label() string {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Sprintf("方式 #%d", t.SrcPK)
	}
	return t.Name
}

// EditMovement 是行程列表的一行。
type EditMovement struct {
	SrcPK          int
	TransportSrc   int
	TransportName  string
	TransportColor string
	FromSrc        int
	ToSrc          int
	FromLabel      string
	ToLabel        string
	// 城市单独放一份：窄屏下这一列只有几十像素，城市名会把行高撑到别处的近两倍，
	// 由 CSS 在窄屏隐藏（编号 + 地点名已经够认了）
	FromCity string
	ToCity   string
	// StartStr / EndStr 是表单用的 datetime-local 值；Span 是列表里显示用的短区间
	StartStr    string
	EndStr      string
	Span        string
	DurationMin int64
	DistanceKm  float64
}

// movementSpan 把行程区间压成一行：同一天只写一次日期。
// 列表在窄屏下每列只有几十像素，三行时间会把行高撑到别处的两倍。
func movementSpan(start, end time.Time) string {
	if start.Format("2006-01-02") == end.Format("2006-01-02") {
		return start.Format("01-02 15:04") + " → " + end.Format("15:04")
	}
	return start.Format("01-02 15:04") + " → " + end.Format("01-02 15:04")
}

type EditVisit struct {
	SrcPK        int
	PlaceName    string
	City         string
	ActivityName string
	ActivityID   int64
	Arrival      time.Time
	ArrivalStr   string
	DepartureStr string
	Remark       string
	Emoji        string
	Bookmarked   bool
	UserAdded    bool
}

type SettingsData struct {
	Page
	Modules []setting.Module
	Pages   []setting.Module
	Saved   bool
	// MapInfo 是当前部署实际生效的底图说明，只读展示。
	MapInfo string
	// MapLayers 是当前可选的底图图层，供「默认底图」下拉渲染。
	MapLayers []MapLayerOption
	// S3Info 是对象存储配置状态的只读说明（配没配齐、桶名、公开域名）。
	S3Info string
}

// MapLayerOption 是后台「默认底图」下拉的一项。
type MapLayerOption struct {
	Key   string
	Label string
}

type PasswordData struct {
	Page
	// Username 是当前用户名，改名表单里回显用
	Username string
}

// FogMaskData 是迷雾遮罩区管理页的数据。
type FogMaskData struct {
	Page
	Masks   []fog.Mask
	MapInfo string
}

// TwoFAData 是两步验证管理页的数据。
type TwoFAData struct {
	Page
	// Enabled 表示两步验证已开启；Pending 表示已生成秘钥等待扫码确认
	Enabled bool
	Pending bool
	Secret  string
	// QR 是 otpauth 链接的 PNG 二维码 data URI
	QR    string
	Err   string
	Flash string
}

// TwoFALoginData 是登录第二步（输入验证码）页面的数据。
type TwoFALoginData struct {
	Settings setting.Settings
	Pending  string
	Next     string
	Err      string
}

// ---------- 视图模型 ----------

type MapPoint struct {
	ID     int64   `json:"id"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Name   string  `json:"n"`
	City   string  `json:"c,omitempty"`
	Act    string  `json:"a,omitempty"`
	Color  string  `json:"k,omitempty"`
	Icon   string  `json:"i,omitempty"`
	Count  int     `json:"v"`
	Dwell  int64   `json:"d"`
	Home   bool    `json:"h,omitempty"`
	Work   bool    `json:"w,omitempty"`
	LastAt string  `json:"t,omitempty"`
	// First 是首次到访时间（Unix 秒），供地图的「时间轴回放」按时间累加显示点位；
	// 没有到访记录的点为 0，时间轴一律把它们算作「一开始就在」。
	First int64 `json:"ft,omitempty"`
	// SrcPK / Verdict / Up / Down 供地图气泡展示「推荐 / 踩雷」标记与票数
	SrcPK   int `json:"sp,omitempty"`
	Verdict int `json:"vd,omitempty"`
	Up      int `json:"u,omitempty"`
	Down    int `json:"dn,omitempty"`
	// Note 是地点备注（访客是否可见由站点设置决定），气泡里直接展示
	Note string `json:"note,omitempty"`
}

type MonthBar struct {
	Key   string
	Label string
	Count int
	Pct   int
}

type CalWeek struct {
	Days  []CalDay
	Month string
}

type CalDay struct {
	Date  string
	Day   int
	Count int
	Level int
	Empty bool
	Title string
}

// BuildCalendar 生成活跃日历（按周分列，周一起始）。
func BuildCalendar(from, to time.Time, counts map[string]int) []CalWeek {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return nil
	}
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location())
	// 回退到周一
	off := (int(start.Weekday()) + 6) % 7
	start = start.AddDate(0, 0, -off)
	end := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, to.Location())

	max := 0
	for _, c := range counts {
		if c > max {
			max = c
		}
	}

	var weeks []CalWeek
	for cur := start; !cur.After(end); cur = cur.AddDate(0, 0, 7) {
		w := CalWeek{}
		for i := 0; i < 7; i++ {
			d := cur.AddDate(0, 0, i)
			key := d.Format("2006-01-02")
			if d.Before(from) || d.After(end) {
				w.Days = append(w.Days, CalDay{Empty: true})
				continue
			}
			n := counts[key]
			w.Days = append(w.Days, CalDay{
				Date:  key,
				Day:   d.Day(),
				Count: n,
				Level: levelOf(n, max),
				Title: fmt.Sprintf("%s：%d 条记录", key, n),
			})
		}
		// 每列顶部标注当月，便于阅读
		mid := cur.AddDate(0, 0, 3)
		w.Month = mid.Format("1月")
		weeks = append(weeks, w)
	}
	// 若跨度超过 60 周，按月聚合展示
	if len(weeks) > 60 {
		return weeks[:60]
	}
	return weeks
}

func levelOf(n, max int) int {
	if n <= 0 || max <= 0 {
		return 0
	}
	switch {
	case n*4 <= max:
		return 1
	case n*2 <= max:
		return 2
	case n*4 <= max*3:
		return 3
	default:
		return 4
	}
}

// BuildMonthly 把月度统计补齐成连续序列（无记录的月份补 0），
// 否则有空档的时间段会被压缩，看不出真实的节奏。
func BuildMonthly(buckets []stats.Bucket) []MonthBar {
	if len(buckets) == 0 {
		return nil
	}
	byKey := make(map[string]int, len(buckets))
	for _, b := range buckets {
		byKey[b.Key] = b.Count
	}
	start, err1 := time.Parse("2006-01", buckets[0].Key)
	end, err2 := time.Parse("2006-01", buckets[len(buckets)-1].Key)
	if err1 != nil || err2 != nil {
		return nil
	}

	// 跨度较大时按年标注，避免「8月」重复出现分不清年份
	multiYear := start.Year() != end.Year()

	max := 0
	for _, b := range buckets {
		if b.Count > max {
			max = b.Count
		}
	}

	var out []MonthBar
	for cur := start; !cur.After(end); cur = cur.AddDate(0, 1, 0) {
		key := cur.Format("2006-01")
		n := byKey[key]
		pct := 2
		if max > 0 {
			pct = int(float64(n)/float64(max)*100 + 0.5)
		}
		if pct < 2 {
			pct = 2
		}
		label := fmt.Sprintf("%d月", int(cur.Month()))
		if multiYear {
			label = fmt.Sprintf("%d/%d", cur.Year()%100, int(cur.Month()))
		}
		out = append(out, MonthBar{Key: key, Label: label, Count: n, Pct: pct})
	}
	return out
}

// ---------- 模板函数 ----------

// maxOf 求切片中的最大计数值，用于条形图归一化。
func maxOf[T any](in []T, get func(T) int) int {
	m := 0
	for _, v := range in {
		if n := get(v); n > m {
			m = n
		}
	}
	return m
}

// appleColors 是 rond 使用的系统色名到十六进制的映射。
var appleColors = map[string]string{
	"red": "#ff3b30", "orange": "#ff9500", "yellow": "#e8b700", "green": "#34c759",
	"mint": "#00c7be", "teal": "#30b0c7", "cyan": "#32ade6", "blue": "#007aff",
	"indigo": "#5856d6", "purple": "#af52de", "pink": "#ff2d55", "brown": "#a2845e",
	"gray": "#8e8e93",
}

// transportColorNames 返回 rond 的系统色名（固定顺序，不随机 map 遍历顺序）。
func transportColorNames() []string {
	out := make([]string, 0, len(appleColors))
	for n := range appleColors {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func colorOf(name string) string {
	if c, ok := appleColors[name]; ok {
		return c
	}
	if len(name) == 7 && name[0] == '#' {
		return name
	}
	return "#5b6169"
}

var poiNames = map[string]string{
	"MKPOICategoryStore": "商店", "MKPOICategoryRestaurant": "餐厅", "MKPOICategoryCafe": "咖啡厅",
	"MKPOICategoryPublicTransport": "公共交通", "MKPOICategoryHotel": "酒店", "MKPOICategoryLandmark": "地标",
	"MKPOICategoryFoodMarket": "食品市场", "MKPOICategoryMuseum": "博物馆", "MKPOICategoryZoo": "动物园",
	"MKPOICategoryBeach": "海滩", "MKPOICategoryCampground": "露营地", "MKPOICategoryStadium": "体育场",
	"MKPOICategoryPark": "公园", "MKPOICategoryNationalPark": "国家公园", "MKPOICategorySchool": "学校",
	"MKPOICategoryPharmacy": "药店", "MKPOICategoryHospital": "医院", "MKPOICategorySpa": "水疗",
	"MKPOICategoryAirport": "机场", "MKPOICategoryMarina": "码头", "MKPOICategoryPostOffice": "邮局",
	"MKPOICategoryMarina2": "码头",
}

func poiName(s string) string {
	if v, ok := poiNames[s]; ok {
		return v
	}
	return s
}

// iconOf 只保留 emoji 图标。rond 里的图标多为 SF Symbols 名称（如 house.fill），
// 在网页上无法渲染，直接返回空串让模板退回默认样式。
func iconOf(s string) string {
	for _, r := range s {
		if r > 0x2000 {
			return s
		}
	}
	return ""
}

func iconOr(s, fallback string) string {
	if v := iconOf(s); v != "" {
		return v
	}
	return fallback
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"num":  fmtNum,
		"size": fmtSize,
		// dur 接受任意整数类型，模板里 int / int64 都会传入
		"dur": func(v any) string {
			switch n := v.(type) {
			case int64:
				return fmtDur(n)
			case int:
				return fmtDur(int64(n))
			case int32:
				return fmtDur(int64(n))
			case float64:
				return fmtDur(int64(n))
			}
			return "—"
		},
		"km": func(f float64) string { return fmt.Sprintf("%.1f", f) },
		"f1": func(f float64) string { return fmt.Sprintf("%.1f", f) },
		"dt": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") },
		// dtt 接受 *time.Time，nil 返回空串，便于可选时间字段直接渲染
		"dtt": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"d":     func(t time.Time) string { return t.Local().Format("2006-01-02") },
		"hm":    func(t time.Time) string { return t.Local().Format("15:04") },
		"md":    func(t time.Time) string { return t.Local().Format("01-02") },
		"day":   dayLabel,
		"since": sinceLabel,
		"trunc": func(s string, n int) string {
			r := []rune(s)
			if len(r) <= n {
				return s
			}
			return string(r[:n]) + "…"
		},
		"add":  func(a, b int) int { return a + b },
		"sub":  func(a, b int) int { return a - b },
		"mul":  func(a, b float64) float64 { return a * b },
		"seqn": seqn,
		"pct": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return int(float64(a)/float64(b)*100 + .5)
		},
		"dict": dict,
		"cond": func(ok bool, a, b string) string {
			if ok {
				return a
			}
			return b
		},
		"safe":      func(s string) template.HTML { return template.HTML(s) },
		"colorof":   colorOf,
		"poiname":   poiName,
		"icon":      iconOf,
		"iconor":    iconOr,
		"condition": stats.ConditionName,
		"json": func(v any) template.JS {
			b, err := json.Marshal(v)
			if err != nil {
				return template.JS("[]")
			}
			return template.JS(b)
		},
	}
}

func fmtSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func fmtNum(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	out = append([]string{s}, out...)
	r := strings.Join(out, ",")
	if neg {
		return "-" + r
	}
	return r
}

func fmtDur(minutes int64) string {
	if minutes <= 0 {
		return "—"
	}
	d := minutes / (60 * 24)
	h := (minutes % (60 * 24)) / 60
	m := minutes % 60
	switch {
	case d > 0 && h > 0:
		return fmt.Sprintf("%d天%d小时", d, h)
	case d > 0:
		return fmt.Sprintf("%d天", d)
	case h > 0 && m > 0:
		return fmt.Sprintf("%d小时%d分", h, m)
	case h > 0:
		return fmt.Sprintf("%d小时", h)
	default:
		return fmt.Sprintf("%d分", m)
	}
}

var weekdayCN = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

func dayLabel(t time.Time) string {
	lt := t.Local()
	now := time.Now()
	prefix := ""
	switch lt.Format("2006-01-02") {
	case now.Format("2006-01-02"):
		prefix = "今天 · "
	case now.AddDate(0, 0, -1).Format("2006-01-02"):
		prefix = "昨天 · "
	}
	return fmt.Sprintf("%s%d年%d月%d日 %s", prefix, lt.Year(), lt.Month(), lt.Day(), weekdayCN[int(lt.Weekday())])
}

func sinceLabel(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	days := int(d.Hours() / 24)
	switch {
	case days == 0:
		return "今天"
	case days == 1:
		return "昨天"
	case days < 30:
		return fmt.Sprintf("%d 天前", days)
	case days < 365:
		return fmt.Sprintf("%d 个月前", days/30)
	default:
		return fmt.Sprintf("%d 年前", days/365)
	}
}

func seqn(n int) []int {
	out := make([]int, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, i)
	}
	return out
}

func dict(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			m[k] = kv[i+1]
		}
	}
	return m
}

// ---------- 专题 ----------

// Topic 是一个行程专题：某段时间、某个区域内的足迹打包展示。
type Topic struct {
	ID          int64
	Title       string
	Subtitle    string
	Description string
	StartAt     time.Time
	EndAt       time.Time
	// StartDate / EndDate 是表单用的 YYYY-MM-DD 形式
	StartDate string
	EndDate   string
	// RangeLabel 是展示用的时间范围文案
	RangeLabel string
	CenterLat  float64
	CenterLon  float64
	RadiusKm   float64
	ShowFog    bool
	Enabled    bool
	SortOrder  int
	// PlaceCount / VisitCount / DayCount / CityCount / DwellMin 由内容统计得出
	PlaceCount int
	VisitCount int
	DayCount   int
	CityCount  int
	DwellMin   int64
}

// HasArea 报告专题是否限定了区域（中心点 + 半径）。
func (t Topic) HasArea() bool { return t.RadiusKm > 0 && (t.CenterLat != 0 || t.CenterLon != 0) }

// TopicAdminData 是后台专题管理页的数据。
type TopicAdminData struct {
	Page
	Topics []Topic
	Form   Topic // 表单回填用
	Flash  string
	Err    string
	IsNew  bool
}

// TopicListData 是前台专题列表页的数据。
type TopicListData struct {
	Page
	Topics []Topic
}

// TopicData 是专题详情页的数据。
type TopicData struct {
	Page
	Topic   Topic
	Points  []MapPoint
	IsAdmin bool
	// PointsJSON / FitCircleJSON / FitBBoxJSON 内联给地图脚本
	// （template.JS 跳过转义，避免被二次转义搞坏）。两个 fit 只有一个非 null。
	PointsJSON    template.JS
	FitCircleJSON template.JS
	FitBBoxJSON   template.JS
}

// trackTag 把配置里的追踪脚本转成可输出的 HTML。
//
// 填 URL（http/https 开头，且不含尖括号）时包一层 <script defer src>；
// 其余情况原样输出——百度统计、Umami 这类要带属性或初始化代码，只能贴整段。
// 值来自服务端配置文件（站长自己写），所以这里不做转义，与 map_tile_url 同一信任级别。
func trackTag(v string) template.HTML {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		if strings.ContainsAny(v, "<>\"'") {
			return "" // 形如 URL 却夹了引号/标签，宁可不挂
		}
		return template.HTML(`<script defer src="` + v + `"></script>`)
	}
	return template.HTML(v)
}

// TempPoint 是逐月气温图上的一根柱子（含算好的百分比高度与配色），
// 具体数值与配色都在服务端算好，模板里只管摆。
type TempPoint struct {
	Key   string
	Label string
	AvgC  float64
	Hours int
	Pct   int
	Color string
}

// TempChart 把逐月平均气温整理成可直接渲染的柱子。
// 高度不按 0℃ 起算——那样 -5℃ 到 35℃ 之间只有 12% 的差别，看不出季节变化；
// 这里按「本区间的最低温 ~ 最高温」铺满高度。
func TempChart(bs []stats.TempBucket) []TempPoint {
	if len(bs) == 0 {
		return nil
	}
	lo, hi := bs[0].AvgC, bs[0].AvgC
	for _, b := range bs {
		if b.AvgC < lo {
			lo = b.AvgC
		}
		if b.AvgC > hi {
			hi = b.AvgC
		}
	}
	span := hi - lo
	out := make([]TempPoint, 0, len(bs))
	for _, b := range bs {
		pct := 60
		if span > 0.05 {
			pct = 12 + int((b.AvgC-lo)/span*88)
		}
		out = append(out, TempPoint{
			Key: b.Key, Label: b.Label, AvgC: b.AvgC, Hours: b.Hours,
			Pct: pct, Color: tempColor(b.AvgC),
		})
	}
	return out
}

// tempColor 把摄氏度映射到冷蓝（≤-5）→ 暖红（≥32）的色带。
func tempColor(c float64) string {
	switch {
	case c <= -5:
		return "#4b83c4"
	case c <= 5:
		return "#6fa8dc"
	case c <= 15:
		return "#69b8a8"
	case c <= 22:
		return "#8fbf5a"
	case c <= 28:
		return "#e2a33c"
	default:
		return "#d9614a"
	}
}
