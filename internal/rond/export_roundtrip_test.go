package rond

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestBaselineKeepsCoreData 守住导出的地基：基线库必须完整保留 Core Data 的
// 元数据表与索引、以及我们不落库的实体（ZRAWVISIT 等）。一旦有人重新改成
// 「纯建表重建」，这里会立刻失败——那正是当初导出包 85KB、rond 提示恢复失败的原因。
// 需要环境变量 ROND_SRC（原始 .rondbackup）；未设置时跳过。
func TestBaselineKeepsCoreData(t *testing.T) {
	src := os.Getenv("ROND_SRC")
	if src == "" {
		t.Skip("设置 ROND_SRC 后运行基线保真度检查")
	}
	base := filepath.Join(t.TempDir(), "baseline.sqlite")
	if err := WriteBaseline(src, base); err != nil {
		t.Fatalf("生成基线库失败: %v", err)
	}
	before := shapeOf(t, src)
	after := shapeOfFile(t, base)

	if after.meta == 0 || after.metaPlist == 0 {
		t.Errorf("基线缺少 Z_METADATA / Z_PLIST，Core Data 会拒绝打开")
	}
	if after.modelCache == 0 {
		t.Errorf("基线缺少 Z_MODELCACHE")
	}
	if !reflect.DeepEqual(before.tables, after.tables) {
		t.Errorf("表集合不一致\n原: %v\n基: %v", before.tables, after.tables)
	}
	if !reflect.DeepEqual(before.indexes, after.indexes) {
		t.Errorf("索引集合不一致：缺 %v", diffList(before.indexes, after.indexes))
	}
	for _, kv := range [][3]any{
		{"ZRAWVISIT", before.rawVisit, after.rawVisit},
		{"ZKEYWORD", before.keyword, after.keyword},
		{"ZVISIT", before.visit, after.visit},
		{"ZLOCATION", before.location, after.location},
	} {
		if kv[1] != kv[2] {
			t.Errorf("%s 行数不一致: 原 %v vs 基 %v", kv[0], kv[1], kv[2])
		}
	}
}

// sqliteShape 是「表/索引集合 + 关键表行数」，用于对比两个库的结构。
type sqliteShape struct {
	tables     []string
	indexes    []string
	meta       int
	metaPlist  int
	modelCache int
	rawVisit   int
	keyword    int
	visit      int
	location   int
}

func shapeOf(t *testing.T, archive string) sqliteShape {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(archive, dir)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", archive, err)
	}
	defer db.Close()
	return readShape(t, db)
}

// shapeOfFile 与 shapeOf 相同，但直接读一个已解包的 .sqlite 文件（基线库形态）。
func shapeOfFile(t *testing.T, path string) sqliteShape {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	defer db.Close()
	return readShape(t, db)
}

func readShape(t *testing.T, db *sql.DB) sqliteShape {
	t.Helper()
	return sqliteShape{
		tables:     queryNames(t, db, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`),
		indexes:    queryNames(t, db, `SELECT name FROM sqlite_master WHERE type='index' ORDER BY name`),
		meta:       countRows(t, db, `SELECT count(*) FROM Z_METADATA`),
		metaPlist:  countRows(t, db, `SELECT count(*) FROM Z_METADATA WHERE length(Z_PLIST)>0`),
		modelCache: countRows(t, db, `SELECT count(*) FROM Z_MODELCACHE`),
		rawVisit:   countRows(t, db, `SELECT count(*) FROM ZRAWVISIT`),
		keyword:    countRows(t, db, `SELECT count(*) FROM ZKEYWORD`),
		visit:      countRows(t, db, `SELECT count(*) FROM ZVISIT`),
		location:   countRows(t, db, `SELECT count(*) FROM ZLOCATION`),
	}
}

func queryNames(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func countRows(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("查询失败 %s: %v", q, err)
	}
	return n
}

func diffList(have, want []string) []string {
	set := map[string]bool{}
	for _, v := range want {
		set[v] = true
	}
	var out []string
	for _, v := range have {
		if !set[v] {
			out = append(out, v)
		}
	}
	return out
}

// TestExportRoundtrip 用 rond 自己的 Open+Parse 反向解析「导出包」，与原始备份逐项比对，
// 验证从 PostgreSQL 重建的 .rondbackup 与 rond 原始格式一致。
// 需要环境变量 ROND_SRC（原始 .rondbackup）与 ROND_EXPORT（导出包）；未设置时跳过。
func TestExportRoundtrip(t *testing.T) {
	src := os.Getenv("ROND_SRC")
	exp := os.Getenv("ROND_EXPORT")
	if src == "" || exp == "" {
		t.Skip("设置 ROND_SRC 与 ROND_EXPORT 后运行往返对比")
	}
	a := parseForTest(t, src)
	c := parseForTest(t, exp)

	na := countArrival(a)
	nc := countArrival(c)
	t.Logf("原始: visits(有到达)=%d places=%d tags=%d acts=%d moves=%d weather=%d rawvisits=%d",
		na, len(a.Locations), len(a.Tags), len(a.Activities), len(a.Movements), len(a.Weather), a.RawVisitNum)
	t.Logf("导出: visits(有到达)=%d places=%d tags=%d acts=%d moves=%d weather=%d rawvisits=%d",
		nc, len(c.Locations), len(c.Tags), len(c.Activities), len(c.Movements), len(c.Weather), c.RawVisitNum)

	if na != nc {
		t.Errorf("到访数不一致: 原 %d vs 导 %d", na, nc)
	}
	if len(a.Locations) != len(c.Locations) {
		t.Errorf("地点数不一致: 原 %d vs 导 %d", len(a.Locations), len(c.Locations))
	}
	if len(a.Activities) != len(c.Activities) {
		t.Errorf("活动类型数不一致: 原 %d vs 导 %d", len(a.Activities), len(c.Activities))
	}
	if len(a.Tags) != len(c.Tags) {
		t.Errorf("标签数不一致: 原 %d vs 导 %d", len(a.Tags), len(c.Tags))
	}
	if len(a.Movements) != len(c.Movements) {
		t.Errorf("位移数不一致: 原 %d vs 导 %d", len(a.Movements), len(c.Movements))
	}
	if len(a.Weather) != len(c.Weather) {
		t.Errorf("天气数不一致: 原 %d vs 导 %d", len(a.Weather), len(c.Weather))
	}

	// 逐条抽查时间与坐标是否一致（按 SrcPK 对齐）
	loc := map[int]Location{}
	for _, l := range a.Locations {
		loc[l.SrcPK] = l
	}
	maxDiff := 0.0
	for _, l := range c.Locations {
		o, ok := loc[l.SrcPK]
		if !ok {
			t.Errorf("导出多出地点 %d", l.SrcPK)
			continue
		}
		if d := absf(o.Lat - l.Lat); d > maxDiff {
			maxDiff = d
		}
		if d := absf(o.Lon - l.Lon); d > maxDiff {
			maxDiff = d
		}
	}
	t.Logf("坐标最大偏差: %.9f", maxDiff)
	if maxDiff > 1e-9 {
		t.Errorf("坐标出现偏差: %v", maxDiff)
	}

	vm := map[int]Visit{}
	for _, v := range a.Visits {
		vm[v.SrcPK] = v
	}
	var maxTimeDelta time.Duration
	for _, v := range c.Visits {
		o, ok := vm[v.SrcPK]
		if !ok {
			t.Errorf("导出多出到访 %d", v.SrcPK)
			continue
		}
		if (o.Arrival == nil) != (v.Arrival == nil) {
			t.Errorf("到访 %d 到达时间有无不一致", v.SrcPK)
			continue
		}
		if o.Arrival == nil {
			continue
		}
		// Core Data 用 float64 存秒数，往返存在亚微秒级舍入，毫秒容差内视为一致
		d := o.Arrival.Sub(*v.Arrival)
		if d < 0 {
			d = -d
		}
		if d > maxTimeDelta {
			maxTimeDelta = d
		}
		if d > time.Millisecond {
			t.Errorf("到访 %d 到达时间偏差过大: 原 %v vs 导 %v", v.SrcPK, o.Arrival, *v.Arrival)
		}
	}
	t.Logf("时间最大偏差: %v", maxTimeDelta)
}

func parseForTest(t *testing.T, path string) *Backup {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(path, dir)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	defer db.Close()
	b, err := Parse(db)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}
	return b
}

func countArrival(b *Backup) int {
	n := 0
	for _, v := range b.Visits {
		if v.Arrival != nil {
			n++
		}
	}
	return n
}

// TestLastArrivalDateMatchesVisits 守住地点详情的「最近活跃时间」。
// rond 直接显示 ZLOCATION.ZLASTARRIVALDATE_，它是该地点最晚一次到访到达时间的缓存：
// 有到访却留空，rond 里就永远显示「无数据」；时间对不上，显示的就是过期值。
// 需要环境变量 ROND_EXPORT（导出后的 .rondbackup）；未设置时跳过。
func TestLastArrivalDateMatchesVisits(t *testing.T) {
	exp := os.Getenv("ROND_EXPORT")
	if exp == "" {
		t.Skip("设置 ROND_EXPORT 后运行「最近活跃时间」一致性检查")
	}
	db, err := Open(exp, t.TempDir())
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", exp, err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT l.Z_PK, COALESCE(l.ZNAME_,''), l.ZLASTARRIVALDATE_, v.mx
		FROM ZLOCATION l
		LEFT JOIN (SELECT ZLOCATION AS loc, max(ZARRIVALDATE_) mx FROM ZVISIT GROUP BY ZLOCATION) v
			ON v.loc = l.Z_PK`)
	if err != nil {
		t.Fatalf("查询地点: %v", err)
	}
	defer rows.Close()

	checked, unvisited := 0, 0
	for rows.Next() {
		var pk int
		var name string
		var last, mx sql.NullFloat64
		if err := rows.Scan(&pk, &name, &last, &mx); err != nil {
			t.Fatalf("读取地点: %v", err)
		}
		switch {
		case !mx.Valid:
			// 从未到访：rond 自己写的是 distantPast 哨兵或干脆不写，两种都行
			unvisited++
		case !last.Valid:
			t.Errorf("地点 %d(%s) 有到访记录，但 ZLASTARRIVALDATE_ 为空——rond 会显示「无数据」", pk, name)
		default:
			checked++
			// Core Data 用 float64 存秒数，毫秒容差内视为一致
			if d := absf(last.Float64 - mx.Float64); d > 1e-3 {
				t.Errorf("地点 %d(%s) 最近活跃时间与最新到访不符: 差 %.3f 秒", pk, name, d)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历地点: %v", err)
	}
	t.Logf("已核对 %d 个有到访的地点（另有 %d 个从未到访）", checked, unvisited)
	if checked == 0 {
		t.Error("没有核对到任何有到访的地点，样本可能不对")
	}
}

// TestUUIDv5 守住补 UUID 的确定性：同一条记录每次导出必须拿到同一个 UUID。
// 若退化成随机，每次导出的包都会让 rond 把同一批记录当成新的。
func TestUUIDv5(t *testing.T) {
	a, b := uuidV5("ZVISIT/42"), uuidV5("ZVISIT/42")
	if !bytes.Equal(a, b) {
		t.Errorf("同名两次生成不一致: %x vs %x", a, b)
	}
	if len(a) != 16 {
		t.Fatalf("长度应为 16 字节，实际 %d", len(a))
	}
	if a[6]>>4 != 5 {
		t.Errorf("版本位应为 5，实际 %d", a[6]>>4)
	}
	if a[8]>>6 != 2 {
		t.Errorf("变体位应为 RFC 4122（10xx），实际 %d", a[8]>>6)
	}
	if bytes.Equal(a, uuidV5("ZVISIT/43")) {
		t.Error("不同记录不应拿到同一个 UUID")
	}
	if bytes.Equal(uuidV5("ZVISIT/42"), uuidV5("ZACTIVITY/42")) {
		t.Error("不同表不应拿到同一个 UUID")
	}
}

// TestProgramRowsFillNativeColumns 守住「站点新建的行也要带上真机必有的列」。
// ZLOCATION 的原始 GPS 与 ZVISIT/ZACTIVITY 的 UUID 在真机包里几乎 100% 有值
// （实测 323/323、822/824、30/30），而站点这边没有对应字段，导出必须替新行补上，
// 否则 rond 里就会缺东西——「最近活跃时间」显示「无数据」就是同一个坑。
// 需要环境变量 ROND_SRC（原始包）与 ROND_EXPORT（导出包）；未设置时跳过。
func TestProgramRowsFillNativeColumns(t *testing.T) {
	src, exp := os.Getenv("ROND_SRC"), os.Getenv("ROND_EXPORT")
	if src == "" || exp == "" {
		t.Skip("设置 ROND_SRC 与 ROND_EXPORT 后运行原生列补全检查")
	}
	old, cur := openBackup(t, src), openBackup(t, exp)
	defer old.Close()
	defer cur.Close()

	for _, c := range []struct{ table, col string }{
		{"ZLOCATION", "ZRAWLATITUDE"},
		{"ZLOCATION", "ZRAWLONGITUDE"},
		{"ZVISIT", "ZIDENTIFIER_"},
		{"ZACTIVITY", "ZUID_"},
		{"ZMOVEMENT", "ZTYPE_"},
	} {
		var base int
		if err := old.QueryRow(`SELECT COALESCE(max(Z_PK),0) FROM ` + c.table).Scan(&base); err != nil {
			t.Fatalf("读取 %s 基线最大主键: %v", c.table, err)
		}
		var total, missing int
		if err := cur.QueryRow(`SELECT count(*), COALESCE(sum(CASE WHEN `+c.col+` IS NULL THEN 1 ELSE 0 END),0)
			FROM `+c.table+` WHERE Z_PK > ?`, base).Scan(&total, &missing); err != nil {
			t.Fatalf("统计 %s.%s: %v", c.table, c.col, err)
		}
		if total == 0 {
			continue // 这一版没有新增行
		}
		if missing > 0 {
			t.Errorf("%s.%s：%d 个站点新建的行里有 %d 个为空（真机几乎全部有值）", c.table, c.col, total, missing)
		}
		t.Logf("%s.%s：新建 %d 行，全部有值", c.table, c.col, total)
	}
}

// TestMovementTypeNeverNull 守住 ZMOVEMENT.ZTYPE_ 全表非空。
// 真机 791/791 都有值（2=无交通方式/步行、5=有交通方式，另有少量 0/4），
// 说明这个属性在 Core Data 里是非可选的——留 NULL 轻则显示异常，重则整包恢复失败。
// 需要环境变量 ROND_EXPORT（导出包）；未设置时跳过。
func TestMovementTypeNeverNull(t *testing.T) {
	exp := os.Getenv("ROND_EXPORT")
	if exp == "" {
		t.Skip("设置 ROND_EXPORT 后运行位移类型非空检查")
	}
	db := openBackup(t, exp)
	defer db.Close()

	var total, missing int
	if err := db.QueryRow(`SELECT count(*),
		COALESCE(sum(CASE WHEN ZTYPE_ IS NULL THEN 1 ELSE 0 END),0) FROM ZMOVEMENT`).
		Scan(&total, &missing); err != nil {
		t.Fatalf("统计 ZMOVEMENT.ZTYPE_: %v", err)
	}
	if total == 0 {
		t.Skip("导出包里没有位移记录")
	}
	if missing > 0 {
		t.Errorf("ZMOVEMENT 共 %d 条，其中 %d 条 ZTYPE_ 为空", total, missing)
	}
	t.Logf("ZMOVEMENT 共 %d 条，ZTYPE_ 全部有值", total)
}

func openBackup(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := Open(path, t.TempDir())
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	return db
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
