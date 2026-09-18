package setting

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Settings 站点级配置，管理员可在后台调整。
type Settings struct {
	SiteTitle    string   `json:"site_title"`
	SiteSubtitle string   `json:"site_subtitle"`
	OwnerName    string   `json:"owner_name"`
	OwnerAvatar  string   `json:"owner_avatar"`
	OwnerBio     string   `json:"owner_bio"`
	Modules      []string `json:"modules"`
	// Pages 是对访客开放的页面；管理员始终可以访问全部页面
	Pages []string `json:"pages"`
	// Regions 是访客的区域限制列表（省 / 市 / 区任意层级）；
	// 空表示不限制，管理员始终看到全部数据。方向由 RegionsMode 决定。
	Regions []string `json:"regions"`
	// RegionsMode 决定 Regions 是白名单还是黑名单：
	// whitelist = 访客只看得到命中这些区域的记录；blacklist = 命中这些区域的记录对访客隐藏。
	RegionsMode string `json:"regions_mode"`
	HideHome    bool   `json:"hide_home"`
	HideWork    bool   `json:"hide_work"`
	// NotesPublic 决定地点备注是否对访客展示；默认只站长自己可见
	NotesPublic bool `json:"notes_public"`
	// MarksPublic 决定「推荐 / 踩雷」标记与票数是否对访客展示；默认公开
	MarksPublic bool   `json:"marks_public"`
	MinDwellMin int    `json:"min_dwell_min"`
	AccentColor string `json:"accent_color"`
	// VisitorHome 是访客打开站点根路径时的落地页；VisitorHomeMain 表示就停在主页。
	// 站长自己访问根路径始终看主页，不受这里影响。
	VisitorHome string `json:"visitor_home"`
	// MapDefaultBase 是地图默认铺哪张底图，取值是底图图层的 Key（如 amap-roadnet）。
	// 空 = 用第一张。点位密集时换成「纯路网」可以去掉底图自带的 POI 与路名标注，
	// 自己的点看得更清楚；访客在图上手动切换过的话，浏览器会记住他自己的选择。
	MapDefaultBase string `json:"map_default_base"`
	// MediaBackend 是地点图片存哪：local（服务器磁盘）或 s3（S3 兼容对象存储，如 R2）。
	// 对象存储的凭据在 conf/app.ini 里（s3_*），不进数据库。
	MediaBackend string `json:"media_backend"`
}

// 地点图片的两种存储后端。
const (
	MediaBackendLocal = "local"
	MediaBackendS3    = "s3"
)

// VisitorHomeMain 表示访客落地到主页本身。
const VisitorHomeMain = "home"

// Regions 的两种方向。
const (
	RegionsWhitelist = "whitelist"
	RegionsBlacklist = "blacklist"
)

// AllModules 主页可开关的区块。
var AllModules = []Module{
	{Key: "overview", Name: "数据总览", Desc: "去过的地方、城市、天数、总里程等核心数字"},
	{Key: "map", Name: "足迹地图", Desc: "所有去过的地点在地图上的分布"},
	{Key: "types", Name: "类型分布", Desc: "按「餐厅 / 商场 / 景点」等类型统计"},
	{Key: "cities", Name: "城市排行", Desc: "按城市汇总到访次数与停留时长"},
	{Key: "timeline", Name: "最近足迹", Desc: "最新若干条到访记录"},
	{Key: "heatmap", Name: "活跃日历", Desc: "记录热度按天分布"},
	{Key: "transport", Name: "出行方式", Desc: "各类交通方式的次数与里程"},
	{Key: "hourly", Name: "作息分布", Desc: "到访时间在一天中的分布"},
	{Key: "tags", Name: "我的标签", Desc: "自定义标签的统计"},
	{Key: "weather", Name: "天气足迹", Desc: "记录期间的气温与天气"},
}

// AllPages 可控制访客可见性的页面。
var AllPages = []Module{
	{Key: "map", Name: "足迹地图", Desc: "交互式地图与行程轨迹"},
	{Key: "places", Name: "地点库", Desc: "全部地点的列表与详情"},
	{Key: "timeline", Name: "时间线", Desc: "按天浏览到访记录"},
	{Key: "stats", Name: "数据统计", Desc: "各维度统计图表与迷雾覆盖"},
	{Key: "report", Name: "年度报告", Desc: "按年份汇总的足迹报告"},
}

type Module struct {
	Key  string
	Name string
	Desc string
}

func Default() Settings {
	mods := make([]string, 0, len(AllModules))
	for _, m := range AllModules {
		mods = append(mods, m.Key)
	}
	pages := make([]string, 0, len(AllPages))
	for _, p := range AllPages {
		pages = append(pages, p.Key)
	}
	return Settings{
		SiteTitle:    "我的足迹地图",
		SiteSubtitle: "记录我去过的每一个地方",
		OwnerName:    "站长",
		OwnerAvatar:  "🐾",
		OwnerBio:     "",
		Modules:      mods,
		Pages:        pages,
		// 「家」和「工作」暴露的是常住地，默认不对访客展示
		HideHome: true,
		HideWork: true,
		// 「推荐 / 踩雷」属于评价类信息，默认对访客公开
		MarksPublic: true,
		// 区域限制默认按黑名单：列出要藏起来的区域，其余照常对访客开放
		RegionsMode: RegionsBlacklist,
		MinDwellMin: 0,
		AccentColor: "#2f7d6e",
		// 默认所有人都从主页进；想让人一进来就看到地图时再改
		VisitorHome: VisitorHomeMain,
		// 图片默认存服务器本地磁盘；配好 R2 之后再在后台切过去
		MediaBackend: MediaBackendLocal,
	}
}

func (s Settings) Has(module string) bool {
	for _, m := range s.Modules {
		if m == module {
			return true
		}
	}
	return false
}

// HasPage 报告某页面是否对访客开放。
func (s Settings) HasPage(page string) bool {
	for _, p := range s.Pages {
		if p == page {
			return true
		}
	}
	return false
}

// VisitorHomePath 返回访客访问根路径时应跳转到的路径，空串表示不跳。
//
// 只在目标页面已对访客开放时才返回路径：否则相当于把访客送到一个他自己看不到的
// 页面（那里会 404），比留在主页更糟。
func (s Settings) VisitorHomePath() string {
	if s.VisitorHome == "" || s.VisitorHome == VisitorHomeMain {
		return ""
	}
	for _, p := range AllPages {
		if p.Key == s.VisitorHome && s.HasPage(p.Key) {
			return "/" + p.Key
		}
	}
	return ""
}

// NormalizeVisitorHome 把表单值收敛到合法取值（主页或 AllPages 里的页）。
// 表单被改坏时回落到主页——这是个「跳错地方」比不跳更糟的开关。
func NormalizeVisitorHome(v string) string {
	for _, p := range AllPages {
		if p.Key == v {
			return v
		}
	}
	return VisitorHomeMain
}

// RegionBlacklist 报告 Regions 是黑名单（命中即对访客隐藏）还是白名单（命中才放行）。
func (s Settings) RegionBlacklist() bool { return s.RegionsMode == RegionsBlacklist }

type Store struct{ DB *sql.DB }

func (st *Store) Load(ctx context.Context) (Settings, error) {
	cur := Default()
	var raw []byte
	err := st.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='site'`).Scan(&raw)
	if err == sql.ErrNoRows {
		return cur, nil
	}
	if err != nil {
		return cur, err
	}
	// 先看原始 JSON 里出现过哪些键：布尔项的默认值是 true，
	// 直接用零值覆盖会让「历史配置里没这一项」被误判成「显式关掉了」。
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		return cur, fmt.Errorf("站点设置解析失败: %w", err)
	}
	var saved Settings
	if err := json.Unmarshal(raw, &saved); err != nil {
		return cur, fmt.Errorf("站点设置解析失败: %w", err)
	}
	// 逐项覆盖，保证新增字段拿到默认值
	if saved.SiteTitle != "" {
		cur.SiteTitle = saved.SiteTitle
	}
	if saved.SiteSubtitle != "" {
		cur.SiteSubtitle = saved.SiteSubtitle
	}
	if saved.OwnerName != "" {
		cur.OwnerName = saved.OwnerName
	}
	if saved.OwnerAvatar != "" {
		cur.OwnerAvatar = saved.OwnerAvatar
	}
	cur.OwnerBio = saved.OwnerBio
	if saved.Modules != nil {
		cur.Modules = saved.Modules
	}
	// pages 键出现即视为显式配置：空数组表示站长明确关闭了所有页面
	if _, ok := present["pages"]; ok {
		cur.Pages = saved.Pages
	}
	if _, ok := present["regions"]; ok {
		cur.Regions = saved.Regions
	}
	// regions_mode 是后加的键，而它出现之前 regions 一直是白名单。
	// 缺键时若直接套用新默认值（黑名单），历史配置里填过的白名单会静默反转成黑名单
	// —— 本该放行的区域全被藏起来，所以缺键一律按白名单解释。
	if _, ok := present["regions_mode"]; ok && saved.RegionsMode == RegionsBlacklist {
		cur.RegionsMode = RegionsBlacklist
	} else {
		cur.RegionsMode = RegionsWhitelist
	}
	if _, ok := present["hide_home"]; ok {
		cur.HideHome = saved.HideHome
	}
	if _, ok := present["hide_work"]; ok {
		cur.HideWork = saved.HideWork
	}
	if _, ok := present["notes_public"]; ok {
		cur.NotesPublic = saved.NotesPublic
	}
	if _, ok := present["marks_public"]; ok {
		cur.MarksPublic = saved.MarksPublic
	}
	if saved.VisitorHome != "" {
		cur.VisitorHome = NormalizeVisitorHome(saved.VisitorHome)
	}
	// 空串是合法值（= 跟随第一张），所以直接赋值，不做非空判断
	cur.MapDefaultBase = saved.MapDefaultBase
	if saved.MediaBackend == MediaBackendS3 {
		cur.MediaBackend = MediaBackendS3
	} else {
		cur.MediaBackend = MediaBackendLocal
	}
	cur.MinDwellMin = saved.MinDwellMin
	if saved.AccentColor != "" {
		cur.AccentColor = saved.AccentColor
	}
	return cur, nil
}

func (st *Store) Save(ctx context.Context, s Settings) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = st.DB.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('site', $1)
		ON CONFLICT (key) DO UPDATE SET value=$1, updated_at=now()`, raw)
	return err
}
