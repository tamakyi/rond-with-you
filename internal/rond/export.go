package rond

import (
	"archive/zip"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rond-with-you/internal/geo"

	_ "modernc.org/sqlite"
)

// managedTables 是「由 PostgreSQL 全量重建」的实体表。
// 其余表一律保持基线原样——包括 Z_METADATA / Z_MODELCACHE / 持久化历史
// (ACHANGE/ATRANSACTION*)、全部索引、以及我们不落库的实体
// （ZRAWVISIT 原始 GPS 采样点、ZKEYWORD、ZPOICATEGORY、ZTRIP* 等）。
var managedTables = []string{"ZVISIT", "ZMOVEMENT", "ZHOURLYWEATHER", "ZTRANSPORT", "ZLOCATION", "ZACTIVITY", "ZTAG"}

// managedCols 是「由 PostgreSQL 负责」的列。这些列之外的字段只能从原始行留档还原
// （留档缺失时再退回基线里的原值），否则会像早期版本那样把 rond 自己写的字段洗成 NULL。
var managedCols = map[string][]string{
	"ZLOCATION": {"ZNAME_", "ZCATEGORY_", "ZLATITUDE", "ZLONGITUDE", "ZISOCOUNTRYCODE",
		"ZADMINISTRATIVEAREA", "ZLOCALITY", "ZSUBLOCALITY", "ZTHOROUGHFARE", "ZTIMEZONE",
		"ZLASTARRIVALDATE_", "ZRAWLATITUDE", "ZRAWLONGITUDE"},
	"ZACTIVITY": {"ZNAME_", "ZCOLOR_", "ZICON_", "ZISHOME", "ZISWORK", "ZISEXCLUDED", "ZISARCHIVED",
		"ZLASTARRIVALDATE_", "ZUID_"},
	"ZTAG":       {"ZNAME_", "ZCOLOR_", "ZGROUP", "ZLASTARRIVALDATE_"},
	"ZTRANSPORT": {"ZNAME_", "ZCOLOR_", "ZICON_"},
	"ZVISIT": {"ZLOCATION", "ZACTIVITY_", "ZARRIVALDATE_", "ZDEPARTUREDATE_",
		"ZBOOKMARKED", "ZUSERADDED", "ZREMARK_", "ZEMOJINAME_", "ZWEATHERSYMBOL_", "ZIDENTIFIER_"},
	"ZMOVEMENT": {"ZTRANSPORT_", "ZVISITFROM_", "ZVISITTO_", "ZSTART_", "ZEND_", "ZTYPE_"},
	"ZHOURLYWEATHER": {"ZVISIT", "ZDATE_", "ZISDAYLIGHT", "ZTEMPERATURE_", "ZAPPARENTTEMPERATURE_", "ZHUMIDITY_",
		"ZPRECIPITATIONAMOUNT_", "ZPRECIPITATIONCHANCE_", "ZWINDSPEED_", "ZVISIBILITY_", "ZUVINDEXVALUE_",
		"ZCONDITION_", "ZSYMBOLNAME_", "ZUVINDEXCATEGORY_", "ZWINDCOMPASSDIRECTION_"},
}

// newRowDefaults 是「编辑页新建、没有原始留档」的行要补的值，照真机包里的取值分布抄。
// 置 NULL 会让 rond 把地点显示成「固定」这类特殊状态。
var newRowDefaults = map[string]map[string]any{
	"ZLOCATION": {"ZBOOKMARKED": 0, "ZISPHOTOIMPORT": 0, "ZNEEDREFRESH": 0, "ZPINSTYLE_": 0,
		"ZTYPE_": 0, "ZUSERADDED": 0, "ZUSERIGNORED": 0, "ZCOUNTDOWNINSECONDS": 0.0, "ZRATING": 0.0,
		"ZRADIUS": 100.0},
	"ZACTIVITY": {"ZISCALENDAREXCLUDED": 0, "ZISDEFAULT": 0, "ZPINSTYLE_": 0,
		"ZCA_": 0.0, "ZCB_": 0.0, "ZCG_": 0.0, "ZCR_": 0.0, "ZG_": 0.0},
	"ZTAG":       {"ZISARCHIVED": 0, "ZISAUTOMARK": 0, "ZPINSTYLE_": 0},
	"ZTRANSPORT": {"ZISDEFAULT": 0},
	"ZVISIT":     {"ZASSOCIATIONBITMASK_": 0, "ZISACTIVITYTAGEXCLUDED": 0, "ZISPHOTOIMPORT": 0, "ZISPLACETAGEXCLUDED": 0, "ZUSERIGNORED": 0},
	"ZMOVEMENT":  {"ZISMODIFIED": 0},
}

// keepNilCols 是「PG 侧算不出值就保持基线原值」的管理列——它们要么是 rond 的派生缓存列，
// 要么是原生包里有、站点这边没有对应字段的列。都只在站点确实有值可写时才动，否则原样保留：
//
//   - ZLOCATION/ZACTIVITY/ZTAG 的 ZLASTARRIVALDATE_（各页面里的「最近活跃时间」）：值为该
//     地点/活动/标签最晚一次到访的到达时间。新建的行不写，rond 里就永远显示「无数据」；
//     但从没被用过的那几个，app 写的是 distantPast 哨兵（真机包里 ZLOCATION 有 38 个），
//     站点复现不出来，就别用 NULL 去覆盖。
//   - ZLOCATION.ZRAWLATITUDE/ZRAWLONGITUDE：真机存的是原始 GPS（WGS-84），而
//     ZLATITUDE/ZLONGITUDE 是它经 GCJ-02 偏移后的显示坐标（实测 293/323 精确满足）。
//     站点新建的地点没有原始 GPS 可写时，才用显示坐标反算一个补上。
//   - ZVISIT.ZIDENTIFIER_ / ZACTIVITY.ZUID_：真机给每条记录存一个 16 字节 UUID v4
//     （822/824、30/30 全有），站点这边没有对应字段，新建的行需要补一个确定性 UUID。
//   - ZMOVEMENT.ZTYPE_：真机 791/791 都有值，取值集中在 2（无交通方式，实测中位速度
//     5.0km/h ≈ 步行）与 5（有交通方式，中位 17km/h），另有 23 条 4、1 条 0。
//     站点复现不了 rond 自己的判定，只给新建的行补一个（见 patchSQLite），
//     已有行一律保持真机原值。
//   - ZTRANSPORT 的名称/颜色/图标：站点不单独维护交通方式表，导出时从位移里聚合。
//     聚合不到（用同一方式的所有行程这几列都为空）就保留基线原值，
//     否则会把真机的模式名洗成 NULL，rond 里变成一个没有名字的方式。
var keepNilCols = map[string][]string{
	"ZLOCATION":  {"ZLASTARRIVALDATE_", "ZRAWLATITUDE", "ZRAWLONGITUDE"},
	"ZACTIVITY":  {"ZLASTARRIVALDATE_", "ZUID_"},
	"ZTAG":       {"ZLASTARRIVALDATE_"},
	"ZVISIT":     {"ZIDENTIFIER_"},
	"ZMOVEMENT":  {"ZTYPE_"},
	"ZTRANSPORT": {"ZNAME_", "ZCOLOR_", "ZICON_"},
}

// movementTypeDefault 给站点新建的位移补 ZTYPE_：真机里这条规律覆盖 757/791 行
// （有交通方式→5、没有→2），剩下的是用户在 app 里手改过的不规则取值。
func movementTypeDefault(transportSet bool) int {
	if transportSet {
		return 5
	}
	return 2
}

// ExportRondbackup 以导入时留下的基线库为底，把 PostgreSQL 里的当前数据覆盖写回，
// 再打包成与 rond 原始格式一致的 .rondbackup（LifeEasy.sqlite / -wal / -shm）。
//
// 坐标是 GCJ-02，与 rond 的 ZLOCATION 同系，原样写回、无需任何坐标变换。
func ExportRondbackup(ctx context.Context, db *sql.DB, datasetID int64, baselinePath string, dst io.Writer) error {
	if _, err := os.Stat(baselinePath); err != nil {
		return fmt.Errorf("缺少基线库（%s）：请重新上传一次原始 .rondbackup，以便重建基线后导出", filepath.Base(baselinePath))
	}
	work, err := os.MkdirTemp("", "rond-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	sqlitePath := filepath.Join(work, "LifeEasy.sqlite")
	if err := copyFile(baselinePath, sqlitePath); err != nil {
		return fmt.Errorf("复制基线库失败: %w", err)
	}
	if err := patchSQLite(ctx, db, datasetID, sqlitePath); err != nil {
		return err
	}
	return zipBackup(sqlitePath, dst)
}

// patchSQLite 在基线库副本上覆盖写入 PostgreSQL 的当前数据。
//
// 对基线里已存在的行走 UPDATE、只动我们管理的列——rond 自己写的其余列
// （ZLOCATION.ZPINSTYLE_/ZRADIUS、ZVISIT.ZRAW/ZIDENTIFIER_、ZMOVEMENT.ZTYPE_ 等）
// 原样保留；此前用 DELETE+INSERT 全量重写会把它们统统置 NULL，rond 里新建的地点
// 会因此显示成「固定」这类特殊状态。真正新增的行才 INSERT，并补上 rond 自己
// 会写的标志位默认值。
func patchSQLite(ctx context.Context, db *sql.DB, datasetID int64, sqlitePath string) error {
	sdb, err := sql.Open("sqlite", "file:"+filepath.ToSlash(sqlitePath))
	if err != nil {
		return fmt.Errorf("打开基线库失败: %w", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1)
	if _, err := sdb.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("切换 WAL 模式失败: %w", err)
	}
	if _, err := sdb.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("关闭外键检查失败: %w", err)
	}

	// 1) 实体编号与关联表都从基线里读，导出不再依赖 coredata_meta
	ents, err := loadEntities(ctx, sdb)
	if err != nil {
		return err
	}
	ent := func(name string) any {
		if e, ok := ents[name]; ok {
			return e
		}
		return nil
	}
	join, err := discoverVisitTagJoin(ctx, sdb, ents)
	if err != nil {
		return err
	}
	has, err := tableExists(ctx, sdb)
	if err != nil {
		return err
	}
	// 基线必须齐全，否则后半程的写入会以「no such table/实体编号为空」这种难懂的方式失败。
	for _, t := range managedTables {
		if !has[t] {
			return fmt.Errorf("基线库缺少表 %s，无法导出：请重新上传一次原始 .rondbackup 以重建基线", t)
		}
	}
	for _, name := range []string{"Location", "Activity", "Tag", "TagGroup", "Transport", "Visit", "Movement", "HourlyWeather"} {
		if _, ok := ents[name]; !ok {
			return fmt.Errorf("基线库的 Z_PRIMARYKEY 缺少实体 %s，无法导出：请重新上传一次原始 .rondbackup", name)
		}
	}

	// 原始行留档：有留档就逐列还原整行，不依赖基线去补那些站点没映射的字段。
	// 取不到（老数据集、表还没建）时 raws[表] 为空，写入器会自动退回原有行为。
	raws := map[string]map[int]map[string]any{}
	skippedRaw := map[string]map[int]bool{}
	for _, t := range managedTables {
		m, sk, err := queryEntityRaw(ctx, db, datasetID, t)
		if err != nil {
			return err
		}
		raws[t], skippedRaw[t] = m, sk
	}

	// 基线表里除主键与管理列之外的列——这些列的值只能从原始行留档还原。
	// 必须在 BeginTx 之前读：事务会独占 sdb 仅有的那条连接，再用 sdb 查询会死锁。
	others := map[string][]pgCol{}
	alls := map[string][]pgCol{}
	for _, t := range managedTables {
		all, other, err := otherColumns(ctx, sdb, t, managedCols[t])
		if err != nil {
			return err
		}
		alls[t], others[t] = all, other
	}

	tx, err := sdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 基线里各表已存在的主键，用于区分 UPDATE 与 INSERT。必须在写之前读，
	// 且只能走 tx——sdb 仅有的连接已被事务占用，再查会死锁。
	known := map[string]map[int]bool{}
	for _, t := range append(append([]string{}, managedTables...), "ZTAGGROUP") {
		if !has[t] {
			continue
		}
		k, err := knownPKs(ctx, tx, t)
		if err != nil {
			return err
		}
		known[t] = k
	}

	// w 是各表写入器的便捷构造；Z_ENT 来自基线的 Z_PRIMARYKEY，
	// 列清单与新增行默认值查 managedCols / newRowDefaults，留档从 PostgreSQL 读。
	w := func(table, entity string) *tableWriter {
		return &tableWriter{ctx: ctx, tx: tx, table: table, entID: ent(entity),
			cols: managedCols[table], defs: newRowDefaults[table], other: others[table],
			all: alls[table], skipped: skippedRaw[table], raws: raws[table],
			known: known[table], seen: map[int]bool{}, keep: keepNilSet(keepNilCols[table])}
	}

	// 2) 地点 ZLOCATION
	placeSrcByID := map[int64]int{}
	places, err := queryPlaces(ctx, db, datasetID)
	if err != nil {
		return err
	}
	loc := w("ZLOCATION", "Location")
	for _, p := range places {
		placeSrcByID[int64(p.ID)] = p.SrcPK
		raw := raws["ZLOCATION"][p.SrcPK]
		// 地点详情的「最近活跃时间」= 该地点最晚一次到访的到达时间，没有到访就不写
		var lastArrival any
		if p.LastVisitAt.Valid {
			lastArrival = appleSeconds(p.LastVisitAt.Time)
		}
		// 原始 GPS 列：真机存的是 WGS-84，站点存的是 GCJ-02 显示坐标。
		// 有留档就用真机那份（它才是真正的原始定位，哪怕用户后来挪过点）；
		// 站点新建的地点没有留档，用显示坐标反算一个，否则这列会空着。
		var rawLat, rawLon any
		if p.Lat.Valid && p.Lon.Valid && !known["ZLOCATION"][p.SrcPK] {
			rawLat, rawLon = geo.GCJ02ToWGS84(p.Lat.Float64, p.Lon.Float64)
		}
		if err := loc.write(p.SrcPK, []any{ns(p.Name), ns(p.Category), nf(p.Lat), nf(p.Lon),
			ns(p.Country), ns(p.Province), ns(p.City), ns(p.Sublocality), ns(p.Thoroughfare), ns(p.Timezone),
			lastArrival, rawLat, rawLon},
			raw); err != nil {
			return fmt.Errorf("写入地点失败: %w", err)
		}
	}
	if err := loc.finish(); err != nil {
		return err
	}

	// ZISPHOTOIMPORT：rond 靠这一位区分「照片导入」与「手工新增」，站点这边没有对应字段，
	// 但来源留档说得清清楚楚，比一律写默认值 0 准。统一在这里补一次：新建行与
	// 「有合成留档、走整行还原」的行都覆盖得到，而真机行（coord_source=rond）不碰。
	if loc.typeOf("ZISPHOTOIMPORT") != "" {
		var pks []any
		for _, p := range places {
			if p.CoordSource.Valid && p.CoordSource.String == "photo" {
				pks = append(pks, p.SrcPK)
			}
		}
		for len(pks) > 0 {
			n := len(pks)
			if n > 500 {
				n = 500
			}
			chunk := pks[:n]
			pks = pks[n:]
			ph := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
			if _, err := tx.ExecContext(ctx,
				"UPDATE ZLOCATION SET ZISPHOTOIMPORT=1 WHERE Z_PK IN ("+ph+")", chunk...); err != nil {
				return fmt.Errorf("写 ZISPHOTOIMPORT 失败: %w", err)
			}
		}
	}

	// 3) 活动类型 ZACTIVITY
	actSrcByID := map[int64]int{}
	activities, err := queryActivities(ctx, db, datasetID)
	if err != nil {
		return err
	}
	actLast, err := queryPlaceLikeMaxArrival(ctx, db, datasetID, "activity_id")
	if err != nil {
		return err
	}
	act := w("ZACTIVITY", "Activity")
	for _, a := range activities {
		actSrcByID[int64(a.ID)] = a.SrcPK
		raw := raws["ZACTIVITY"][a.SrcPK]
		// 站点没有图标入口，新建的活动在 rond 里会是空图标；站点自己渲染时缺省用 📍
		icon := ns(a.Icon)
		if icon == nil {
			icon = "📍"
		}
		// 活动列表里的「最近活跃时间」：同 ZLOCATION，取该活动最晚一次到访
		var last any
		if t, ok := actLast[int64(a.ID)]; ok {
			last = appleSeconds(t)
		}
		if err := act.write(a.SrcPK, []any{ns(a.Name), ns(a.Color), icon,
			bint(a.Home), bint(a.Work), bint(a.Excluded), bint(a.Archived), last,
			coreDataUUID(!known["ZACTIVITY"][a.SrcPK], "ZACTIVITY", a.SrcPK)},
			raw); err != nil {
			return fmt.Errorf("写入活动类型失败: %w", err)
		}
	}
	if err := act.finish(); err != nil {
		return err
	}

	// 4) 标签分组 ZTAGGROUP：沿用基线上已有的主键（按名字匹配），只补新增的分组，
	// 这样 ZTAG.ZGROUP 仍指向原有分组，分组本身也不会被换掉主键。
	groupPK := map[string]int{}
	{
		grows, err := tx.QueryContext(ctx, `SELECT Z_PK, COALESCE(ZNAME_,'') FROM ZTAGGROUP`)
		if err != nil {
			return fmt.Errorf("读取标签分组: %w", err)
		}
		for grows.Next() {
			var pk int
			var name string
			if err := grows.Scan(&pk, &name); err != nil {
				grows.Close()
				return err
			}
			groupPK[name] = pk
		}
		grows.Close()
	}
	needGroups, err := queryStrings(ctx, db, `SELECT DISTINCT COALESCE(group_name,'') FROM tags WHERE dataset_id=$1 AND group_name<>''`, datasetID)
	if err != nil {
		return fmt.Errorf("读取标签分组: %w", err)
	}
	maxGroupPK := 0
	for pk := range known["ZTAGGROUP"] {
		if pk > maxGroupPK {
			maxGroupPK = pk
		}
	}
	for _, g := range needGroups {
		if _, ok := groupPK[g]; ok {
			continue
		}
		maxGroupPK++
		groupPK[g] = maxGroupPK
		if _, err := tx.ExecContext(ctx, `INSERT INTO ZTAGGROUP (Z_PK, Z_ENT, Z_OPT, ZNAME_) VALUES (?,?,?,?)`,
			maxGroupPK, ent("TagGroup"), 1, g); err != nil {
			return fmt.Errorf("写入标签分组失败: %w", err)
		}
	}

	// 5) 标签 ZTAG
	tagSrcByID := map[int64]int{}
	tags, err := queryTags(ctx, db, datasetID)
	if err != nil {
		return err
	}
	tag := w("ZTAG", "Tag")
	tagLast, err := queryTagMaxArrival(ctx, db, datasetID)
	if err != nil {
		return err
	}
	for _, t := range tags {
		tagSrcByID[int64(t.SrcPK)] = t.SrcPK
		var grp any
		if t.Group.Valid && t.Group.String != "" {
			if gpk, ok := groupPK[t.Group.String]; ok {
				grp = gpk
			}
		}
		// 标签的「最近活跃时间」：该标签下最晚一次到访（同 ZLOCATION / ZACTIVITY）
		var last any
		if tm, ok := tagLast[int64(t.SrcPK)]; ok {
			last = appleSeconds(tm)
		}
		if err := tag.write(t.SrcPK, []any{ns(t.Name), ns(t.Color), grp, last},
			raws["ZTAG"][t.SrcPK]); err != nil {
			return fmt.Errorf("写入标签失败: %w", err)
		}
	}
	if err := tag.finish(); err != nil {
		return err
	}

	// 6) 交通方式 ZTRANSPORT（从位移里聚合而来）
	transports, err := queryTransports(ctx, db, datasetID)
	if err != nil {
		return err
	}
	tr := w("ZTRANSPORT", "Transport")
	// 没有任何行程引用的方式保持基线原样，不当成「已删除」
	tr.keepMissing = true
	for _, t := range transports {
		if err := tr.write(t.SrcPK, []any{ns(t.Name), ns(t.Color), ns(t.Icon)}, raws["ZTRANSPORT"][t.SrcPK]); err != nil {
			return fmt.Errorf("写入交通方式失败: %w", err)
		}
	}
	if err := tr.finish(); err != nil {
		return err
	}

	// 7) 访问记录 ZVISIT
	visits, err := queryVisits(ctx, db, datasetID)
	if err != nil {
		return err
	}
	visit := w("ZVISIT", "Visit")
	for _, v := range visits {
		var locPK, actPK any
		if v.PlaceID.Valid {
			if sp, ok := placeSrcByID[v.PlaceID.Int64]; ok {
				locPK = sp
			}
		}
		if v.ActivityID.Valid {
			if sp, ok := actSrcByID[v.ActivityID.Int64]; ok {
				actPK = sp
			}
		}
		if err := visit.write(v.SrcPK, []any{locPK, actPK, appleSeconds(v.Arrival), nt(v.Departure),
			bint(v.Bookmarked), bint(v.UserAdded), ns(v.Remark), ns(v.Emoji), ns(v.WeatherSymbol),
			coreDataUUID(!known["ZVISIT"][v.SrcPK], "ZVISIT", v.SrcPK)},
			raws["ZVISIT"][v.SrcPK]); err != nil {
			return fmt.Errorf("写入访问记录失败: %w", err)
		}
	}
	if err := visit.finish(); err != nil {
		return err
	}

	// 8) 访问↔标签关联（列名从基线探测得到；纯两列关系，全量重写即可）
	if join.Name != "" && join.VisitCol != "" && join.TagCol != "" {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+join.Name); err != nil {
			return fmt.Errorf("清空 %s 失败: %w", join.Name, err)
		}
		vt, err := db.QueryContext(ctx, `SELECT v.src_pk, t.src_pk FROM visit_tags vt
			JOIN visits v ON v.id=vt.visit_id JOIN tags t ON t.id=vt.tag_id
			WHERE v.dataset_id=$1`, datasetID)
		if err != nil {
			return fmt.Errorf("读取访问标签: %w", err)
		}
		for vt.Next() {
			var vsrc, tsrc int
			if err := vt.Scan(&vsrc, &tsrc); err != nil {
				vt.Close()
				return err
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (%s, %s) VALUES (?,?)",
				join.Name, join.VisitCol, join.TagCol), vsrc, tsrc); err != nil {
				vt.Close()
				return fmt.Errorf("写入访问标签关联失败: %w", err)
			}
		}
		vt.Close()
		if err := vt.Err(); err != nil {
			return err
		}
	}

	// 9) 位移 ZMOVEMENT
	movements, err := queryMovements(ctx, db, datasetID)
	if err != nil {
		return err
	}
	mov := w("ZMOVEMENT", "Movement")
	for _, m := range movements {
		var trv, fromv, tov any
		if m.Transport.Valid {
			trv = m.Transport.Int64
		}
		if m.FromVisit.Valid {
			fromv = m.FromVisit.Int64
		}
		if m.ToVisit.Valid {
			tov = m.ToVisit.Int64
		}
		// ZTYPE_ 只有站点新建的行才补（已在基线里的行保持真机原值，见 keepNilCols）
		var mvType any
		if !known["ZMOVEMENT"][m.SrcPK] {
			mvType = movementTypeDefault(m.Transport.Valid)
		}
		if err := mov.write(m.SrcPK, []any{trv, fromv, tov, nt(m.Start), nt(m.End), mvType},
			raws["ZMOVEMENT"][m.SrcPK]); err != nil {
			return fmt.Errorf("写入位移失败: %w", err)
		}
	}
	if err := mov.finish(); err != nil {
		return err
	}

	// 10) 小时级天气 ZHOURLYWEATHER
	weather, err := queryWeather(ctx, db, datasetID)
	if err != nil {
		return err
	}
	wx := w("ZHOURLYWEATHER", "HourlyWeather")
	for _, row := range weather {
		var vsrc any
		if row.VisitSrc.Valid {
			vsrc = row.VisitSrc.Int64
		}
		if err := wx.write(row.SrcPK, []any{vsrc, nt(row.At), bint(row.Daylight),
			nf(row.Temp), nf(row.App), nf(row.Hum), nf(row.Pa), nf(row.Pc), nf(row.Ws), nf(row.Vis), ni(row.UV),
			ns(row.Cond), ns(row.Symbol), ns(row.UVCat), ns(row.WindDir)},
			raws["ZHOURLYWEATHER"][row.SrcPK]); err != nil {
			return fmt.Errorf("写入天气失败: %w", err)
		}
	}
	if err := wx.finish(); err != nil {
		return err
	}

	// 11) 回写 Z_PRIMARYKEY.Z_MAX：只上调，避免 rond 下次插入撞键
	bumps := map[string]int{
		"Location":      maxPKOf(known["ZLOCATION"], placesPKs(places)),
		"Activity":      maxPKOf(known["ZACTIVITY"], actsPKs(activities)),
		"Tag":           maxPKOf(known["ZTAG"], tagsPKs(tags)),
		"Transport":     maxPKOf(known["ZTRANSPORT"], transPKs(transports)),
		"Visit":         maxPKOf(known["ZVISIT"], visitsPKs(visits)),
		"Movement":      maxPKOf(known["ZMOVEMENT"], movsPKs(movements)),
		"HourlyWeather": maxPKOf(known["ZHOURLYWEATHER"], wxPKs(weather)),
		"TagGroup":      maxGroupPK,
	}
	for name, mx := range bumps {
		if _, err := tx.ExecContext(ctx, `UPDATE Z_PRIMARYKEY SET Z_MAX=? WHERE Z_NAME=? AND Z_MAX<?`, mx, name, mx); err != nil {
			return fmt.Errorf("更新 Z_PRIMARYKEY %s 失败: %w", name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := sdb.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("合并 WAL 失败: %w", err)
	}
	return nil
}

// tableWriter 把一行数据写进基线库：主键已存在走 UPDATE（只动 cols，rond 写的其余列不动），
// 不存在走 INSERT（补 defs 里的 rond 默认值）。finish() 负责清掉 PG 里已删除的行。
type tableWriter struct {
	ctx   context.Context
	tx    *sql.Tx
	table string
	entID any // 该表在 Z_PRIMARYKEY 里的实体编号（INSERT 时写 Z_ENT）
	cols  []string
	defs  map[string]any
	other []pgCol // 非管理列，值只能从留档还原
	all   []pgCol // 除 Z_PK 外的全部列，仅供整行还原时取类型
	// 导入时因缺必填字段而没进 PostgreSQL 的行，导出要按留档原样补回去。
	skipped map[int]bool
	raws    map[int]map[string]any
	known   map[int]bool
	seen    map[int]bool
	// keep 里的列在值为空时保持基线/留档原值（见 keepNilCols）。
	keep map[string]bool
	// keepMissing 为真时，基线里存在但 PostgreSQL 没写到的行不删除。
	// 只有 ZTRANSPORT 用：交通方式在站点侧没有独立的表，导出时从行程里聚合，
	// 「删掉某方式的最后一条行程」不等于用户想删掉这个方式——留着才是保真。
	keepMissing bool
}

// keepNilSet 把 keepNilCols 的列名列表转成集合（空列表返回 nil，读 nil map 恒为 false）。
func keepNilSet(cols []string) map[string]bool {
	if len(cols) == 0 {
		return nil
	}
	m := make(map[string]bool, len(cols))
	for _, c := range cols {
		m[c] = true
	}
	return m
}

// typeOf 取某列在基线里的声明类型，用于把留档值还原成驱动认识的形式。
func (w *tableWriter) typeOf(col string) string {
	for _, c := range w.all {
		if c.Name == col {
			return c.Type
		}
	}
	return ""
}

// write 写一行。raw 非空（导入时留过整行档案）时用 INSERT OR REPLACE 写全列：
// 管理列取站点当前值，其余列取留档原值——rond 自己写的字段因此一个都不会丢。
func (w *tableWriter) write(pk int, vals []any, raw map[string]any) error {
	if len(vals) != len(w.cols) {
		return fmt.Errorf("%s: 值个数 %d 与列数 %d 不一致", w.table, len(vals), len(w.cols))
	}
	w.seen[pk] = true
	if len(raw) > 0 {
		return w.writeFull(pk, vals, raw)
	}
	if w.known[pk] {
		sets := make([]string, 0, len(w.cols))
		args := make([]any, 0, len(w.cols)+1)
		for i, c := range w.cols {
			// 派生缓存列没有新值就整列不动，交给基线里的原值
			if w.keep[c] && vals[i] == nil {
				continue
			}
			sets = append(sets, c+"=?")
			args = append(args, vals[i])
		}
		if len(sets) == 0 {
			return nil
		}
		args = append(args, pk)
		_, err := w.tx.ExecContext(w.ctx, "UPDATE "+w.table+" SET "+strings.Join(sets, ",")+" WHERE Z_PK=?", args...)
		return err
	}
	names := []string{"Z_PK", "Z_ENT", "Z_OPT"}
	args := []any{pk, w.entID, 1}
	for i, c := range w.cols {
		names = append(names, c)
		args = append(args, vals[i])
	}
	for c, v := range w.defs {
		if containsStr(w.cols, c) {
			continue
		}
		names = append(names, c)
		args = append(args, v)
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	_, err := w.tx.ExecContext(w.ctx,
		"INSERT INTO "+w.table+" ("+strings.Join(names, ",")+") VALUES ("+ph+")", args...)
	return err
}

// keepEmptyString 保住「原本是空串」的文本列。
// 站点把空串当未填写存成 NULL，导出时就会把这些列写成 NULL；
// 对 rond 来说空串和 NULL 不是一回事，往返一趟就对不上了。
func keepEmptyString(v any, raw any) any {
	if v != nil {
		return v
	}
	if s, ok := raw.(string); ok && s == "" {
		return ""
	}
	return v
}

// entID 是该表在 Z_PRIMARYKEY 里的实体编号（INSERT 时写 Z_ENT）。
// writeFull 按留档还原整行：管理列用站点当前值，其余列用原始值。
func (w *tableWriter) writeFull(pk int, vals []any, raw map[string]any) error {
	names := []string{"Z_PK"}
	args := []any{pk}
	// 1) 管理列：站点当前值（PG 是这些字段的真值来源）
	for i, c := range w.cols {
		if w.keep[c] && vals[i] == nil {
			// 派生缓存列算不出新值 → 写留档原值，别把 app 写的哨兵洗成 NULL
			rv, ok := raw[c]
			if !ok {
				continue
			}
			names = append(names, c)
			args = append(args, sqliteValue(w.typeOf(c), rv))
			continue
		}
		names = append(names, c)
		args = append(args, keepEmptyString(vals[i], raw[c]))
	}
	// 2) 其余列：留档里的原始值
	seen := make(map[string]bool, len(w.cols))
	for _, c := range w.cols {
		seen[c] = true
	}
	for _, c := range w.other {
		if seen[c.Name] {
			continue
		}
		if c.Name == "Z_ENT" {
			names, args = append(names, c.Name), append(args, w.entID)
			continue
		}
		rv, ok := raw[c.Name]
		if !ok {
			continue
		}
		names, args = append(names, c.Name), append(args, sqliteValue(c.Type, rv))
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	_, err := w.tx.ExecContext(w.ctx,
		"INSERT OR REPLACE INTO "+w.table+" ("+strings.Join(names, ",")+") VALUES ("+ph+")", args...)
	return err
}

// finish 删除基线里 PostgreSQL 已不存在的行，保证导出包与站点数据一致；
// 再把导入时因缺必填字段没能进 PostgreSQL 的行按留档原样补回，
// 否则「导入—导出」一趟就会凭空少数据。
func (w *tableWriter) finish() error {
	for pk := range w.known {
		if w.seen[pk] || w.keepMissing {
			continue
		}
		if _, err := w.tx.ExecContext(w.ctx, "DELETE FROM "+w.table+" WHERE Z_PK=?", pk); err != nil {
			return fmt.Errorf("清理 %s 中已删除的行 %d 失败: %w", w.table, pk, err)
		}
	}
	for pk := range w.skipped {
		if w.seen[pk] {
			continue
		}
		raw, ok := w.raws[pk]
		if !ok || len(raw) == 0 {
			continue
		}
		if err := w.writeRaw(pk, raw); err != nil {
			return fmt.Errorf("还原 %s 中未导入的行 %d 失败: %w", w.table, pk, err)
		}
	}
	return nil
}

// writeRaw 完全按留档写回一整行，所有列（含站点管理列）都取原始值。
func (w *tableWriter) writeRaw(pk int, raw map[string]any) error {
	names := []string{"Z_PK"}
	args := []any{pk}
	for _, c := range w.all {
		if c.Name == "Z_ENT" {
			names = append(names, c.Name)
			args = append(args, w.entID)
			continue
		}
		v, ok := raw[c.Name]
		if !ok {
			continue
		}
		names = append(names, c.Name)
		args = append(args, sqliteValue(c.Type, v))
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	_, err := w.tx.ExecContext(w.ctx,
		"INSERT OR REPLACE INTO "+w.table+" ("+strings.Join(names, ",")+") VALUES ("+ph+")", args...)
	return err
}

// pgCol 是基线表的一列（名字 + 声明类型）。
type pgCol struct {
	Name string
	Type string
}

// otherColumns 取基线表的列定义：all 是除 Z_PK 外的全部列，other 再排除掉管理列。
func otherColumns(ctx context.Context, sdb *sql.DB, table string, managed []string) (all, other []pgCol, err error) {
	rows, err := sdb.QueryContext(ctx,
		fmt.Sprintf("SELECT name, COALESCE(type,'') FROM pragma_table_info('%s')", strings.ReplaceAll(table, "'", "''")))
	if err != nil {
		return nil, nil, fmt.Errorf("读取 %s 列定义: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var c pgCol
		if err := rows.Scan(&c.Name, &c.Type); err != nil {
			return nil, nil, err
		}
		if c.Name == "Z_PK" {
			continue
		}
		all = append(all, c)
		if containsStr(managed, c.Name) {
			continue
		}
		other = append(other, c)
	}
	return all, other, rows.Err()
}

// queryEntityRaw 读出某张表在导入时留下的整行档案。
func queryEntityRaw(ctx context.Context, db *sql.DB, datasetID int64, table string) (map[int]map[string]any, map[int]bool, error) {
	out := map[int]map[string]any{}
	skip := map[int]bool{}
	rows, err := db.QueryContext(ctx,
		`SELECT src_pk, raw::text, skipped FROM entity_raw WHERE dataset_id=$1 AND entity=$2`, datasetID, table)
	if err != nil {
		// 表还没建（迁移未跑）时不能算致命，退回无留档模式
		return out, skip, nil
	}
	defer rows.Close()
	for rows.Next() {
		var pk int
		var blob string
		var sk bool
		if err := rows.Scan(&pk, &blob, &sk); err != nil {
			return nil, nil, err
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(blob), &raw); err != nil {
			continue
		}
		out[pk] = raw
		if sk {
			skip[pk] = true
		}
	}
	return out, skip, rows.Err()
}

// sqliteValue 把 JSON 里取回的值还原成适合写进 SQLite 的类型：
// JSON 只有数字一种数值类型，整形列要还原成 int64；BLOB 解码 base64。
func sqliteValue(colType string, v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case bool:
		return bint(t)
	case float64:
		if strings.Contains(strings.ToUpper(colType), "INT") && t == math.Trunc(t) {
			return int64(t)
		}
		return t
	case string:
		return t
	case map[string]any:
		if b, ok := t["$blob"].(string); ok {
			if data, err := base64.StdEncoding.DecodeString(b); err == nil {
				return data
			}
		}
		return nil
	default:
		return v
	}
}

// knownPKs 读出基线表里现存的全部主键。
func knownPKs(ctx context.Context, tx *sql.Tx, table string) (map[int]bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT Z_PK FROM "+table)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 主键: %w", table, err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var pk sql.NullInt64
		if err := rows.Scan(&pk); err != nil {
			return nil, err
		}
		if pk.Valid {
			out[int(pk.Int64)] = true
		}
	}
	return out, rows.Err()
}

// ---------- PostgreSQL 侧的行读取（先取回内存，再统一写 SQLite，避免两库交错） ----------

type pgPlace struct {
	ID, SrcPK                                                                              int
	Name, Category, Country, Province, City, District, Sublocality, Thoroughfare, Timezone sql.NullString
	Lat, Lon                                                                               sql.NullFloat64
	LastVisitAt                                                                            sql.NullTime
	// CoordSource 记录这个点是哪个入口建的（photo/manual/rond）。只有 ZISPHOTOIMPORT
	// 用得上：rond 靠这个位区分「照片导入」与「手工新增」，而站点这边没有对应字段。
	CoordSource sql.NullString
}

func queryPlaces(ctx context.Context, db *sql.DB, datasetID int64) ([]pgPlace, error) {
	// 区县取 sublocality，缺了再退到 district：站点表单只暴露「区县」，
	// 手工补录的地点这两列里只有 district 有值，直接用 sublocality 会把区县丢掉。
	rows, err := db.QueryContext(ctx, `SELECT id, src_pk, name, poi_category, lat, lon, country_code, province, city,
		COALESCE(NULLIF(sublocality,''), NULLIF(district,'')), thoroughfare, timezone, last_visit_at,
		coord_source
		FROM places WHERE dataset_id=$1 ORDER BY id`, datasetID)
	if err != nil {
		return nil, fmt.Errorf("读取地点: %w", err)
	}
	defer rows.Close()
	var out []pgPlace
	for rows.Next() {
		var p pgPlace
		if err := rows.Scan(&p.ID, &p.SrcPK, &p.Name, &p.Category, &p.Lat, &p.Lon, &p.Country,
			&p.Province, &p.City, &p.Sublocality, &p.Thoroughfare, &p.Timezone, &p.LastVisitAt,
			&p.CoordSource); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type pgActivity struct {
	ID, SrcPK                      int
	Name, Color, Icon              sql.NullString
	Home, Work, Excluded, Archived bool
}

// queryPlaceLikeMaxArrival 返回「按某个到访外键分组的最晚到达时间」，
// 用来填 ZACTIVITY.ZLASTARRIVALDATE_ 这类缓存列（col 只接受 activity_id）。
func queryPlaceLikeMaxArrival(ctx context.Context, db *sql.DB, datasetID int64, col string) (map[int64]time.Time, error) {
	if col != "activity_id" {
		return nil, fmt.Errorf("不支持的聚合列 %q", col)
	}
	rows, err := db.QueryContext(ctx, `SELECT activity_id, max(arrival) FROM visits
		WHERE dataset_id=$1 AND activity_id IS NOT NULL GROUP BY activity_id`, datasetID)
	if err != nil {
		return nil, fmt.Errorf("读取活动最近到访: %w", err)
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var id int64
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// queryTagMaxArrival 返回每个标签下最晚一次到访的到达时间（标签挂在到访上，走关联表）。
func queryTagMaxArrival(ctx context.Context, db *sql.DB, datasetID int64) (map[int64]time.Time, error) {
	rows, err := db.QueryContext(ctx, `SELECT vt.tag_id, max(v.arrival)
		FROM visits v JOIN visit_tags vt ON vt.visit_id = v.id
		WHERE v.dataset_id=$1 GROUP BY vt.tag_id`, datasetID)
	if err != nil {
		return nil, fmt.Errorf("读取标签最近到访: %w", err)
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var id int64
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

func queryActivities(ctx context.Context, db *sql.DB, datasetID int64) ([]pgActivity, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, src_pk, name, color, icon, is_home, is_work, is_excluded, is_archived
		FROM activities WHERE dataset_id=$1 ORDER BY id`, datasetID)
	if err != nil {
		return nil, fmt.Errorf("读取活动类型: %w", err)
	}
	defer rows.Close()
	var out []pgActivity
	for rows.Next() {
		var a pgActivity
		var h, w, e, ar bool
		if err := rows.Scan(&a.ID, &a.SrcPK, &a.Name, &a.Color, &a.Icon, &h, &w, &e, &ar); err != nil {
			return nil, err
		}
		a.Home, a.Work, a.Excluded, a.Archived = h, w, e, ar
		out = append(out, a)
	}
	return out, rows.Err()
}

type pgTag struct {
	SrcPK              int
	Name, Color, Group sql.NullString
}

func queryTags(ctx context.Context, db *sql.DB, datasetID int64) ([]pgTag, error) {
	rows, err := db.QueryContext(ctx, `SELECT src_pk, name, color, COALESCE(group_name,'') FROM tags WHERE dataset_id=$1 ORDER BY id`, datasetID)
	if err != nil {
		return nil, fmt.Errorf("读取标签: %w", err)
	}
	defer rows.Close()
	var out []pgTag
	for rows.Next() {
		var t pgTag
		if err := rows.Scan(&t.SrcPK, &t.Name, &t.Color, &t.Group); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type pgTransport struct {
	SrcPK             int
	Name, Color, Icon sql.NullString
}

// queryTransports 把位移里用到的交通方式聚合成 ZTRANSPORT 的行。
//
// 必须按 transport_src 唯一：DISTINCT 在「同一方式的不同行程取名不一致」时会返回
// 两行同一个 Z_PK（编辑页允许改方式名，历史数据里也已经有这种脏数据），
// 导出时会往 ZTRANSPORT 插两次同一个主键——新建的方式直接撞唯一约束让整次导出失败。
// 取名取最近编辑过的那条（id 最大）的非空值。
func queryTransports(ctx context.Context, db *sql.DB, datasetID int64) ([]pgTransport, error) {
	rows, err := db.QueryContext(ctx, `SELECT transport_src,
		(array_agg(transport_name  ORDER BY id DESC) FILTER (WHERE transport_name  IS NOT NULL))[1],
		(array_agg(transport_color ORDER BY id DESC) FILTER (WHERE transport_color IS NOT NULL))[1],
		(array_agg(transport_icon  ORDER BY id DESC) FILTER (WHERE transport_icon  IS NOT NULL))[1]
		FROM movements WHERE dataset_id=$1 AND transport_src IS NOT NULL
		GROUP BY transport_src ORDER BY transport_src`, datasetID)
	if err != nil {
		return nil, fmt.Errorf("读取交通方式: %w", err)
	}
	defer rows.Close()
	var out []pgTransport
	for rows.Next() {
		var t pgTransport
		var ts sql.NullInt64
		if err := rows.Scan(&ts, &t.Name, &t.Color, &t.Icon); err != nil {
			return nil, err
		}
		if !ts.Valid {
			continue
		}
		t.SrcPK = int(ts.Int64)
		out = append(out, t)
	}
	return out, rows.Err()
}

type pgVisit struct {
	SrcPK                        int
	PlaceID, ActivityID          sql.NullInt64
	Arrival                      time.Time
	Departure                    sql.NullTime
	Bookmarked, UserAdded        bool
	Remark, Emoji, WeatherSymbol sql.NullString
}

func queryVisits(ctx context.Context, db *sql.DB, datasetID int64) ([]pgVisit, error) {
	rows, err := db.QueryContext(ctx, `SELECT src_pk, place_id, activity_id, arrival, departure, bookmarked, is_user_added,
		COALESCE(remark,''), COALESCE(emoji,''), COALESCE(weather_symbol,'')
		FROM visits WHERE dataset_id=$1 ORDER BY id`, datasetID)
	if err != nil {
		return nil, fmt.Errorf("读取访问记录: %w", err)
	}
	defer rows.Close()
	var out []pgVisit
	for rows.Next() {
		var v pgVisit
		var bm, ua bool
		if err := rows.Scan(&v.SrcPK, &v.PlaceID, &v.ActivityID, &v.Arrival, &v.Departure, &bm, &ua,
			&v.Remark, &v.Emoji, &v.WeatherSymbol); err != nil {
			return nil, err
		}
		v.Bookmarked, v.UserAdded = bm, ua
		out = append(out, v)
	}
	return out, rows.Err()
}

type pgMovement struct {
	SrcPK                         int
	Transport, FromVisit, ToVisit sql.NullInt64
	Start, End                    sql.NullTime
}

func queryMovements(ctx context.Context, db *sql.DB, datasetID int64) ([]pgMovement, error) {
	rows, err := db.QueryContext(ctx, `SELECT src_pk, transport_src, from_visit_src, to_visit_src, started_at, ended_at
		FROM movements WHERE dataset_id=$1 ORDER BY id`, datasetID)
	if err != nil {
		return nil, fmt.Errorf("读取位移: %w", err)
	}
	defer rows.Close()
	var out []pgMovement
	for rows.Next() {
		var m pgMovement
		if err := rows.Scan(&m.SrcPK, &m.Transport, &m.FromVisit, &m.ToVisit, &m.Start, &m.End); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type pgWeather struct {
	SrcPK                           int
	VisitSrc                        sql.NullInt64
	At                              sql.NullTime
	Daylight                        bool
	Temp, App, Hum, Pa, Pc, Ws, Vis sql.NullFloat64
	UV                              sql.NullInt64
	Cond, Symbol, UVCat, WindDir    sql.NullString
}

func queryWeather(ctx context.Context, db *sql.DB, datasetID int64) ([]pgWeather, error) {
	rows, err := db.QueryContext(ctx, `SELECT src_pk, visit_src, at, is_daylight, temperature_c, apparent_c, humidity,
		precipitation_amount, precipitation_chance, wind_speed, visibility, uv_index,
		COALESCE(condition,''), COALESCE(symbol,''), COALESCE(uv_category,''), COALESCE(wind_direction,'')
		FROM weather WHERE dataset_id=$1 ORDER BY id`, datasetID)
	if err != nil {
		return nil, fmt.Errorf("读取天气: %w", err)
	}
	defer rows.Close()
	var out []pgWeather
	for rows.Next() {
		var wRow pgWeather
		var dl bool
		if err := rows.Scan(&wRow.SrcPK, &wRow.VisitSrc, &wRow.At, &dl, &wRow.Temp, &wRow.App, &wRow.Hum, &wRow.Pa, &wRow.Pc,
			&wRow.Ws, &wRow.Vis, &wRow.UV, &wRow.Cond, &wRow.Symbol, &wRow.UVCat, &wRow.WindDir); err != nil {
			return nil, err
		}
		wRow.Daylight = dl
		out = append(out, wRow)
	}
	return out, rows.Err()
}

// ---------- Z_PRIMARYKEY.Z_MAX ----------

func maxPKOf(known map[int]bool, written []int) int {
	mx := 0
	for pk := range known {
		if pk > mx {
			mx = pk
		}
	}
	for _, pk := range written {
		if pk > mx {
			mx = pk
		}
	}
	return mx
}

func placesPKs(v []pgPlace) []int {
	out := make([]int, len(v))
	for i, r := range v {
		out[i] = r.SrcPK
	}
	return out
}
func actsPKs(v []pgActivity) []int {
	out := make([]int, len(v))
	for i, r := range v {
		out[i] = r.SrcPK
	}
	return out
}
func tagsPKs(v []pgTag) []int {
	out := make([]int, len(v))
	for i, r := range v {
		out[i] = r.SrcPK
	}
	return out
}
func transPKs(v []pgTransport) []int {
	out := make([]int, len(v))
	for i, r := range v {
		out[i] = r.SrcPK
	}
	return out
}
func visitsPKs(v []pgVisit) []int {
	out := make([]int, len(v))
	for i, r := range v {
		out[i] = r.SrcPK
	}
	return out
}
func movsPKs(v []pgMovement) []int {
	out := make([]int, len(v))
	for i, r := range v {
		out[i] = r.SrcPK
	}
	return out
}
func wxPKs(v []pgWeather) []int {
	out := make([]int, len(v))
	for i, r := range v {
		out[i] = r.SrcPK
	}
	return out
}

// loadEntities 从基线的 Z_PRIMARYKEY 读「实体名 → 编号」。
func loadEntities(ctx context.Context, sdb *sql.DB) (map[string]int, error) {
	ents := map[string]int{}
	rows, err := sdb.QueryContext(ctx, `SELECT COALESCE(Z_ENT,0), COALESCE(Z_NAME,'') FROM Z_PRIMARYKEY`)
	if err != nil {
		return nil, fmt.Errorf("读取 Z_PRIMARYKEY: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ent int
		var name string
		if err := rows.Scan(&ent, &name); err != nil {
			return nil, err
		}
		if name != "" {
			ents[name] = ent
		}
	}
	return ents, rows.Err()
}

// discoverVisitTagJoin 在基线里定位「访问↔标签」多对多表及其两列。
func discoverVisitTagJoin(ctx context.Context, sdb *sql.DB, ents map[string]int) (JoinTableInfo, error) {
	visitEnt, okV := ents["Visit"]
	tagEnt, okT := ents["Tag"]
	if !okV || !okT {
		return JoinTableInfo{}, nil
	}
	names, err := queryStrings(ctx, sdb, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Z\_%' ESCAPE '\'`)
	if err != nil {
		return JoinTableInfo{}, err
	}
	for _, n := range names {
		if !strings.HasSuffix(n, "VISITS_") {
			continue
		}
		cols, err := queryStrings(ctx, sdb, fmt.Sprintf("SELECT name FROM pragma_table_info('%s')", strings.ReplaceAll(n, "'", "''")))
		if err != nil {
			continue
		}
		var vcol, tcol string
		for _, c := range cols {
			if strings.Contains(c, fmt.Sprintf("Z_%dVISITS", visitEnt)) {
				vcol = c
			}
			if strings.Contains(c, fmt.Sprintf("Z_%dTAGS", tagEnt)) {
				tcol = c
			}
		}
		if vcol != "" && tcol != "" {
			return JoinTableInfo{Name: n, VisitCol: vcol, TagCol: tcol}, nil
		}
	}
	return JoinTableInfo{}, nil
}

func tableExists(ctx context.Context, sdb *sql.DB) (map[string]bool, error) {
	names, err := queryStrings(ctx, sdb, `SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

func queryStrings(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// zipBackup 打包三个文件。压缩方式与 rond 一致（Store 不压缩），
// 并且即使 -wal/-shm 为空也一并写入——避免设备上残留的旧 WAL 与新库不匹配。
func zipBackup(sqlitePath string, dst io.Writer) error {
	zw := zip.NewWriter(dst)
	defer zw.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := sqlitePath + suffix
		if suffix != "" {
			if _, err := os.Stat(p); err != nil {
				f, cerr := os.Create(p)
				if cerr != nil {
					return cerr
				}
				f.Close()
			}
		}
		name := "LifeEasy.sqlite" + suffix
		if err := func() error {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
			if err != nil {
				return err
			}
			_, err = io.Copy(w, f)
			return err
		}(); err != nil {
			return fmt.Errorf("打包 %s 失败: %w", name, err)
		}
	}
	return nil
}

// appleSeconds 把时间转成 Core Data 时间戳（自 2001-01-01 UTC 起的秒数，绝对时刻，与时区无关）。
func appleSeconds(t time.Time) float64 {
	// 先取整数微秒再除，避免 Duration.Seconds() 的浮点表示误差
	// 让时间戳在往返一趟后差出 1 微秒。
	return float64(t.UTC().Sub(appleEpoch).Microseconds()) / 1e6
}

// coreDataUUID 给「基线里没有、站点又没这个字段」的新行补一个确定性 UUID（见 uuidV5）。
// 基线里已有的行一律返回 nil（交给 keepNilCols 沿用原值）：站点本就没有这个字段，
// 不该反过来改写真机的身份标识——真机里也确实有 2 条到访这一列是空的。
func coreDataUUID(isNew bool, table string, pk int) any {
	if !isNew {
		return nil
	}
	return uuidV5(fmt.Sprintf("%s/%d", table, pk))
}

// uuidNamespace 是本站补 UUID 时用的固定命名空间，取值本身无意义，只要不变即可。
var uuidNamespace = [16]byte{0x6b, 0x1c, 0x9a, 0x2e, 0x42, 0x7d, 0x4f, 0x16,
	0x8b, 0x33, 0x5a, 0x0c, 0xd9, 0x71, 0xe4, 0x08}

// uuidV5 由命名空间与名字生成 RFC 4122 版本 5 的 UUID（16 字节原始形式）。
//
// rond 给每条到访、每个活动都存一个 16 字节 UUID（ZIDENTIFIER_ / ZUID_，真机里全是 v4），
// 站点这边没有对应字段。这里用确定性算法补：名字只取表名 + src_pk（src_pk 跨快照稳定），
// 于是同一条记录每次导出拿到的都是同一个 UUID——若改成随机，每次导出的包都会让 rond
// 把同一批记录当成新的。
func uuidV5(name string) []byte {
	h := sha1.New()
	h.Write(uuidNamespace[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)[:16]
	sum[6] = (sum[6] & 0x0f) | 0x50 // 版本 5
	sum[8] = (sum[8] & 0x3f) | 0x80 // RFC 4122 变体位
	return sum
}

func ns(v sql.NullString) any {
	if v.Valid && v.String != "" {
		return v.String
	}
	return nil
}

func nf(v sql.NullFloat64) any {
	if v.Valid {
		return v.Float64
	}
	return nil
}

func ni(v sql.NullInt64) any {
	if v.Valid {
		return v.Int64
	}
	return nil
}

func nt(v sql.NullTime) any {
	if v.Valid {
		return appleSeconds(v.Time)
	}
	return nil
}

func bint(b bool) int {
	if b {
		return 1
	}
	return 0
}
