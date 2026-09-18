// Package rond 负责解析 rond（iOS 应用 LifeEasy）导出的 .rondbackup 备份包。
//
// 备份包本质是 zip，内含 Core Data 的 SQLite 库 LifeEasy.sqlite 及同名 -wal / -shm。
// 数据主要在 WAL 里，因此必须三个文件一起解出来再打开，否则会丢失绝大部分记录。
package rond

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Core Data 时间戳基准：2001-01-01 UTC
var appleEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func appleTime(v sql.NullFloat64) *time.Time {
	if !v.Valid {
		return nil
	}
	// 秒的整数部分和小数部分分开算：8 亿量级的秒乘 1e6/1e9 已超出 float64 的
	// 精确整数范围（2^53），一次性相乘再取整会系统性吃掉最后 1 微秒，
	// 往返导出后时间戳就对不上了。
	sec := math.Floor(v.Float64)
	us := int64(sec)*1_000_000 + int64(math.Round((v.Float64-sec)*1e6))
	t := appleEpoch.Add(time.Duration(us) * time.Microsecond).Local()
	return &t
}

type Activity struct {
	SrcPK      int
	Name       string
	Color      string
	Icon       string
	IsHome     bool
	IsWork     bool
	IsExcluded bool
	IsArchived bool
}

type Tag struct {
	SrcPK     int
	Name      string
	Color     string
	GroupName string
}

type Location struct {
	SrcPK           int
	Name            string
	POICategory     string
	Lat             float64
	Lon             float64
	CountryCode     string
	Province        string
	City            string
	District        string
	Sublocality     string
	Thoroughfare    string
	SubThoroughfare string
	Timezone        string
}

type Visit struct {
	SrcPK         int
	LocationSrcPK *int
	ActivitySrcPK *int
	Arrival       *time.Time
	Departure     *time.Time
	Bookmarked    bool
	UserAdded     bool
	Remark        string
	Emoji         string
	WeatherSymbol string
	TagSrcPKs     []int
}

type Movement struct {
	SrcPK        int
	TransportSrc *int
	FromVisitSrc *int
	ToVisitSrc   *int
	Start        *time.Time
	End          *time.Time
}

type Weather struct {
	SrcPK      int
	VisitSrc   *int
	At         *time.Time
	IsDaylight bool
	TempC      *float64
	ApparentC  *float64
	Humidity   *float64
	PrecipAmt  *float64
	PrecipPct  *float64
	WindSpeed  *float64
	Visibility *float64
	UVIndex    *int
	Condition  string
	Symbol     string
	UVCategory string
	WindDir    string
}

type Transport struct {
	SrcPK int
	Name  string
	Color string
	Icon  string
}

// CoreDataMeta 记录备份包里 Core Data 库的结构信息（实体编号、建表 DDL、关联表），
// 作为数据集的结构快照留档；导出 rondbackup 已改为以「基线库」为底覆盖写入，不再依赖它。
type CoreDataMeta struct {
	// 实体名(ZPRIMARYKEY.Z_NAME) -> 实体编号(Z_ENT) 与当前最大主键(Z_MAX)
	Entities map[string]EntityInfo `json:"entities"`
	// 表名 -> CREATE TABLE 语句（含全部列与默认值，导出只 INSERT 其中部分列）
	DDL map[string]string `json:"ddl"`
	// 访问↔标签多对多关联表（Core Data 自动生成的 Z_<tagEnt>VISITS_）
	JoinTable JoinTableInfo `json:"join_visit_tag"`
}

type EntityInfo struct {
	Ent int `json:"ent"`
	Max int `json:"max"`
}

type JoinTableInfo struct {
	Name     string `json:"name"`
	VisitCol string `json:"visit_col"`
	TagCol   string `json:"tag_col"`
}

type Backup struct {
	Activities  []Activity
	Tags        []Tag
	Transports  []Transport
	Locations   []Location
	Visits      []Visit
	Movements   []Movement
	Weather     []Weather
	RawVisitNum int
	TagGroups   map[int]string
	Meta        *CoreDataMeta
	// RawRows 是各实体在 Core Data 里的完整原始行：「表名 -> Z_PK -> 列名 -> 值」。
	// 站点只映射了其中一部分字段，留档让导出能逐列还原，不必依赖基线库补剩下的列。
	RawRows map[string]map[int]map[string]any
}

// Open 解压备份包到 dir，返回可用的 SQLite 连接。
// 调用方负责 Close 与删除 dir。
func Open(archivePath, dir string) (*sql.DB, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("备份包不是有效的 zip: %w", err)
	}
	defer zr.Close()

	var dbPath string
	for _, f := range zr.File {
		base := filepath.Base(f.Name)
		if f.FileInfo().IsDir() || base == "" {
			continue
		}
		// 只取 Core Data 主库及其 WAL / SHM
		low := strings.ToLower(base)
		if !strings.HasSuffix(low, ".sqlite") && !strings.HasSuffix(low, ".sqlite-wal") && !strings.HasSuffix(low, ".sqlite-shm") {
			continue
		}
		dst := filepath.Join(dir, base)
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		out, err := os.Create(dst)
		if err != nil {
			rc.Close()
			return nil, err
		}
		_, cerr := io.Copy(out, rc)
		rc.Close()
		out.Close()
		if cerr != nil {
			return nil, cerr
		}
		if strings.HasSuffix(low, ".sqlite") {
			dbPath = dst
		}
	}
	if dbPath == "" {
		return nil, fmt.Errorf("备份包内未找到 .sqlite 数据库文件")
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// 把 WAL 合并回主库，避免后续读取时依赖 -shm 状态
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		db.Close()
		return nil, fmt.Errorf("合并 WAL 失败: %w", err)
	}
	return db, nil
}

// Parse 读取 Core Data 库，转换为领域模型。
func Parse(db *sql.DB) (*Backup, error) {
	b := &Backup{TagGroups: map[int]string{}}
	var err error

	// 标签分组
	rows, err := db.Query(`SELECT Z_PK, COALESCE(ZNAME_,'') FROM ZTAGGROUP`)
	if err != nil {
		return nil, fmt.Errorf("读取标签分组: %w", err)
	}
	for rows.Next() {
		var pk int
		var name string
		if err = rows.Scan(&pk, &name); err != nil {
			rows.Close()
			return nil, err
		}
		b.TagGroups[pk] = name
	}
	rows.Close()

	// 标签（含所属分组关系 Z_11TAGS_ 的逆向：ZTAG.ZGROUP -> ZTAGGROUP）
	tagGroup := map[int]int{}
	grows, err := db.Query(`SELECT Z_PK, COALESCE(ZGROUP,0) FROM ZTAG`)
	if err != nil {
		return nil, fmt.Errorf("读取标签: %w", err)
	}
	for grows.Next() {
		var pk, g int
		if err = grows.Scan(&pk, &g); err != nil {
			grows.Close()
			return nil, err
		}
		tagGroup[pk] = g
	}
	grows.Close()

	trows, err := db.Query(`SELECT Z_PK, COALESCE(ZNAME_,''), COALESCE(ZCOLOR_,'') FROM ZTAG`)
	if err != nil {
		return nil, fmt.Errorf("读取标签: %w", err)
	}
	for trows.Next() {
		var t Tag
		if err = trows.Scan(&t.SrcPK, &t.Name, &t.Color); err != nil {
			trows.Close()
			return nil, err
		}
		t.GroupName = b.TagGroups[tagGroup[t.SrcPK]]
		b.Tags = append(b.Tags, t)
	}
	trows.Close()

	// 类型 / 活动
	arows, err := db.Query(`SELECT Z_PK, COALESCE(ZNAME_,''), COALESCE(ZCOLOR_,''), COALESCE(ZICON_,''),
		COALESCE(ZISHOME,0), COALESCE(ZISWORK,0), COALESCE(ZISEXCLUDED,0), COALESCE(ZISARCHIVED,0) FROM ZACTIVITY`)
	if err != nil {
		return nil, fmt.Errorf("读取活动类型: %w", err)
	}
	for arows.Next() {
		var a Activity
		var h, w, e, ar int
		if err = arows.Scan(&a.SrcPK, &a.Name, &a.Color, &a.Icon, &h, &w, &e, &ar); err != nil {
			arows.Close()
			return nil, err
		}
		a.IsHome, a.IsWork, a.IsExcluded, a.IsArchived = h == 1, w == 1, e == 1, ar == 1
		b.Activities = append(b.Activities, a)
	}
	arows.Close()

	// 交通方式
	trrows, err := db.Query(`SELECT Z_PK, COALESCE(ZNAME_,''), COALESCE(ZCOLOR_,''), COALESCE(ZICON_,'') FROM ZTRANSPORT`)
	if err != nil {
		return nil, fmt.Errorf("读取交通方式: %w", err)
	}
	for trrows.Next() {
		var t Transport
		if err = trrows.Scan(&t.SrcPK, &t.Name, &t.Color, &t.Icon); err != nil {
			trrows.Close()
			return nil, err
		}
		b.Transports = append(b.Transports, t)
	}
	trrows.Close()

	// 地点
	lrows, err := db.Query(`SELECT Z_PK, COALESCE(ZNAME_,''), COALESCE(ZCATEGORY_,''),
		COALESCE(ZLATITUDE,0), COALESCE(ZLONGITUDE,0),
		COALESCE(ZISOCOUNTRYCODE,''), COALESCE(ZADMINISTRATIVEAREA,''), COALESCE(ZLOCALITY,''),
		COALESCE(ZSUBLOCALITY,''), COALESCE(ZTHOROUGHFARE,''), COALESCE(ZSUBTHOROUGHFARE,''),
		COALESCE(ZTIMEZONE,'') FROM ZLOCATION`)
	if err != nil {
		return nil, fmt.Errorf("读取地点: %w", err)
	}
	for lrows.Next() {
		var l Location
		if err = lrows.Scan(&l.SrcPK, &l.Name, &l.POICategory, &l.Lat, &l.Lon,
			&l.CountryCode, &l.Province, &l.City, &l.Sublocality, &l.Thoroughfare,
			&l.SubThoroughfare, &l.Timezone); err != nil {
			lrows.Close()
			return nil, err
		}
		b.Locations = append(b.Locations, l)
	}
	lrows.Close()

	// 访问记录
	vrows, err := db.Query(`SELECT Z_PK, ZLOCATION, ZACTIVITY_, ZARRIVALDATE_, ZDEPARTUREDATE_,
		COALESCE(ZBOOKMARKED,0), COALESCE(ZUSERADDED,0), COALESCE(ZREMARK_,''),
		COALESCE(ZEMOJINAME_,''), COALESCE(ZWEATHERSYMBOL_,'') FROM ZVISIT`)
	if err != nil {
		return nil, fmt.Errorf("读取访问记录: %w", err)
	}
	visitIdx := map[int]int{}
	for vrows.Next() {
		var v Visit
		var loc, act sql.NullInt64
		var arr, dep sql.NullFloat64
		var bm, ua int
		if err = vrows.Scan(&v.SrcPK, &loc, &act, &arr, &dep, &bm, &ua,
			&v.Remark, &v.Emoji, &v.WeatherSymbol); err != nil {
			vrows.Close()
			return nil, err
		}
		if loc.Valid {
			pk := int(loc.Int64)
			v.LocationSrcPK = &pk
		}
		if act.Valid {
			pk := int(act.Int64)
			v.ActivitySrcPK = &pk
		}
		v.Arrival = appleTime(arr)
		v.Departure = appleTime(dep)
		v.Bookmarked, v.UserAdded = bm == 1, ua == 1
		visitIdx[v.SrcPK] = len(b.Visits)
		b.Visits = append(b.Visits, v)
	}
	vrows.Close()

	// 访问 → 标签
	vtrows, err := db.Query(`SELECT Z_18VISITS_, Z_11TAGS_5 FROM Z_11VISITS_`)
	if err != nil {
		return nil, fmt.Errorf("读取访问标签: %w", err)
	}
	for vtrows.Next() {
		var vid, tid int
		if err = vtrows.Scan(&vid, &tid); err != nil {
			vtrows.Close()
			return nil, err
		}
		if i, ok := visitIdx[vid]; ok {
			b.Visits[i].TagSrcPKs = append(b.Visits[i].TagSrcPKs, tid)
		}
	}
	vtrows.Close()

	// 位移
	mrows, err := db.Query(`SELECT Z_PK, ZTRANSPORT_, ZVISITFROM_, ZVISITTO_, ZSTART_, ZEND_ FROM ZMOVEMENT`)
	if err != nil {
		return nil, fmt.Errorf("读取位移记录: %w", err)
	}
	for mrows.Next() {
		var m Movement
		var tr, from, to sql.NullInt64
		var st, en sql.NullFloat64
		if err = mrows.Scan(&m.SrcPK, &tr, &from, &to, &st, &en); err != nil {
			mrows.Close()
			return nil, err
		}
		if tr.Valid {
			pk := int(tr.Int64)
			m.TransportSrc = &pk
		}
		if from.Valid {
			pk := int(from.Int64)
			m.FromVisitSrc = &pk
		}
		if to.Valid {
			pk := int(to.Int64)
			m.ToVisitSrc = &pk
		}
		m.Start = appleTime(st)
		m.End = appleTime(en)
		b.Movements = append(b.Movements, m)
	}
	mrows.Close()

	// 小时级天气
	wrows, err := db.Query(`SELECT Z_PK, ZVISIT, ZDATE_, COALESCE(ZISDAYLIGHT,0),
		ZTEMPERATURE_, ZAPPARENTTEMPERATURE_, ZHUMIDITY_, ZPRECIPITATIONAMOUNT_,
		ZPRECIPITATIONCHANCE_, ZWINDSPEED_, ZVISIBILITY_, ZUVINDEXVALUE_,
		COALESCE(ZCONDITION_,''), COALESCE(ZSYMBOLNAME_,''), COALESCE(ZUVINDEXCATEGORY_,''),
		COALESCE(ZWINDCOMPASSDIRECTION_,'') FROM ZHOURLYWEATHER`)
	if err != nil {
		return nil, fmt.Errorf("读取天气记录: %w", err)
	}
	for wrows.Next() {
		var w Weather
		var vis sql.NullInt64
		var at sql.NullFloat64
		var dl int
		var temp, app, hum, pa, pc, ws, vv sql.NullFloat64
		var uv sql.NullInt64
		if err = wrows.Scan(&w.SrcPK, &vis, &at, &dl, &temp, &app, &hum, &pa, &pc, &ws, &vv, &uv,
			&w.Condition, &w.Symbol, &w.UVCategory, &w.WindDir); err != nil {
			wrows.Close()
			return nil, err
		}
		if vis.Valid {
			pk := int(vis.Int64)
			w.VisitSrc = &pk
		}
		w.At = appleTime(at)
		w.IsDaylight = dl == 1
		w.TempC = fptr(temp)
		w.ApparentC = fptr(app)
		w.Humidity = fptr(hum)
		w.PrecipAmt = fptr(pa)
		w.PrecipPct = fptr(pc)
		w.WindSpeed = fptr(ws)
		w.Visibility = fptr(vv)
		if uv.Valid {
			n := int(uv.Int64)
			w.UVIndex = &n
		}
		b.Weather = append(b.Weather, w)
	}
	wrows.Close()

	if err := db.QueryRow(`SELECT count(*) FROM ZRAWVISIT`).Scan(&b.RawVisitNum); err != nil {
		b.RawVisitNum = 0
	}

	// 抓取 Core Data 结构元信息（实体编号 / 建表 DDL / 关联表），作为结构快照留档。
	// 失败不致命：旧备份可能没有这些表。
	if meta, merr := captureMeta(db); merr == nil {
		b.Meta = meta
	} else {
		log.Printf("抓取 Core Data 元信息失败: %v", merr)
	}
	// 整行留档：导出时逐列还原，不丢任何 rond 自己写的字段。
	if raw, rerr := captureRawRows(db); rerr == nil {
		b.RawRows = raw
	} else {
		log.Printf("抓取原始行留档失败（导出将退回基线补列）: %v", rerr)
	}
	return b, nil
}

// rawTables 是需要整行留档的 Core Data 表（站点管理的实体 + 标签分组）。
var rawTables = []string{"ZLOCATION", "ZVISIT", "ZMOVEMENT", "ZHOURLYWEATHER", "ZACTIVITY", "ZTAG", "ZTAGGROUP", "ZTRANSPORT"}

// captureRawRows 读出上表每一行的全部列，供导出逐列还原。
// BLOB 编码成 {"$blob": base64}（JSON 没有字节串类型），时间戳维持 Core Data 浮点秒原值。
func captureRawRows(db *sql.DB) (map[string]map[int]map[string]any, error) {
	out := map[string]map[int]map[string]any{}
	for _, t := range rawTables {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, t).Scan(&n); err != nil || n == 0 {
			continue // 旧备份可能没这张表
		}
		cols, err := queryStrings(context.Background(), db, fmt.Sprintf("SELECT name FROM pragma_table_info('%s')", t))
		if err != nil || len(cols) == 0 {
			continue
		}
		quoted := make([]string, len(cols))
		for i, c := range cols {
			quoted[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
		}
		rows, err := db.Query(fmt.Sprintf("SELECT %s FROM %s", strings.Join(quoted, ","), t))
		if err != nil {
			return nil, fmt.Errorf("读取 %s 原始行: %w", t, err)
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		m := map[int]map[string]any{}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, err
			}
			var pk int
			row := make(map[string]any, len(cols))
			for i, c := range cols {
				if c == "Z_PK" {
					if v, ok := vals[i].(int64); ok {
						pk = int(v)
					}
				}
				row[c] = jsonSafe(vals[i])
			}
			if pk > 0 {
				m[pk] = row
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out[t] = m
	}
	return out, nil
}

// jsonSafe 把驱动值换成能被 JSON 无损表达的形式。
func jsonSafe(v any) any {
	if b, ok := v.([]byte); ok {
		return map[string]any{"$blob": base64.StdEncoding.EncodeToString(b)}
	}
	return v
}

// WriteBaseline 把备份包内的 Core Data 库另存为一份「单文件基线」，供导出时作为底稿。
//
// 为什么要有基线：rond 是 Core Data 应用，库里有 Z_METADATA（模型版本哈希）、
// Z_MODELCACHE、持久化历史表(ACHANGE/ATRANSACTION*)、大量索引，以及我们并不落库的实体
// （ZRAWVISIT 原始 GPS 采样点、ZKEYWORD、ZTRIP* 等）。仅凭抓到的建表语句重建只能得到
// 一个「空壳」，Core Data 会判定与模型不兼容而拒绝打开。以基线为底、只覆盖我们管理的
// 那几张实体表，上述内容就都能原样保留。
//
// 备份包里数据主要在 WAL 中，所以这里先按 Open 的方式解包并合并 WAL，再用
// VACUUM INTO 导出一份自洽的单文件副本（不依赖 -wal/-shm）。
func WriteBaseline(archivePath, dst string) error {
	dir, err := os.MkdirTemp("", "rond-base-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	db, err := Open(archivePath, dir) // Open 内部已做 wal_checkpoint(TRUNCATE)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	_ = os.Remove(dst)
	// VACUUM INTO 需要字面量路径（不用参数绑定），单引号按 SQL 规则转义
	lit := strings.ReplaceAll(filepath.ToSlash(dst), "'", "''")
	if _, err := db.Exec("VACUUM INTO '" + lit + "'"); err != nil {
		// 退路：主库在 Open 里已合并过 WAL，直接复制也自洽
		src := filepath.Join(dir, "LifeEasy.sqlite")
		if cerr := copyFile(src, dst); cerr != nil {
			return fmt.Errorf("生成基线库失败: %v（VACUUM INTO 亦失败: %w）", cerr, err)
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// captureMeta 读取 ZPRIMARYKEY（实体编号与最大主键）与所需的 Z* 建表 DDL，
// 并定位「访问↔标签」关联表及其两列。作为数据集上的 Core Data 结构快照留档。
func captureMeta(db *sql.DB) (*CoreDataMeta, error) {
	m := &CoreDataMeta{Entities: map[string]EntityInfo{}, DDL: map[string]string{}}
	rows, err := db.Query(`SELECT COALESCE(Z_ENT,0), COALESCE(Z_NAME,''), COALESCE(Z_MAX,0) FROM Z_PRIMARYKEY`)
	if err != nil {
		return nil, fmt.Errorf("读取 Z_PRIMARYKEY: %w", err)
	}
	for rows.Next() {
		var ent, max int
		var name string
		if err := rows.Scan(&ent, &name, &max); err != nil {
			rows.Close()
			return nil, err
		}
		if name != "" {
			m.Entities[name] = EntityInfo{Ent: ent, Max: max}
		}
	}
	rows.Close()

	drows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type='table' AND name LIKE 'Z%' AND sql IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("读取建表 DDL: %w", err)
	}
	for drows.Next() {
		var name, ddl string
		if err := drows.Scan(&name, &ddl); err != nil {
			drows.Close()
			return nil, err
		}
		m.DDL[name] = ddl
	}
	drows.Close()

	// 访问↔标签关联表：表名形如 Z_<tagEnt>VISITS_，两列分别指向 Visit / Tag 实体。
	visitEnt, okV := m.Entities["Visit"]
	tagEnt, okT := m.Entities["Tag"]
	if okV && okT {
		for tname, ddl := range m.DDL {
			if !strings.HasPrefix(tname, "Z_") || !strings.HasSuffix(tname, "VISITS_") {
				continue
			}
			_ = ddl
			crows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", tname))
			if err != nil {
				continue
			}
			var vcol, tcol string
			for crows.Next() {
				var cid int
				var cname, ctype string
				var notnull int
				var dflt sql.NullString
				var pk int
				if err := crows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk); err != nil {
					crows.Close()
					continue
				}
				if strings.Contains(cname, fmt.Sprintf("Z_%dVISITS", visitEnt.Ent)) {
					vcol = cname
				}
				if strings.Contains(cname, fmt.Sprintf("Z_%dTAGS", tagEnt.Ent)) {
					tcol = cname
				}
			}
			crows.Close()
			if vcol != "" && tcol != "" {
				m.JoinTable = JoinTableInfo{Name: tname, VisitCol: vcol, TagCol: tcol}
				break
			}
		}
	}
	return m, nil
}

func fptr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}
