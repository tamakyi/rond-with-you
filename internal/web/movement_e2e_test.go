package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rond-with-you/internal/auth"
	"rond-with-you/internal/config"
	"rond-with-you/internal/db"
	"rond-with-you/internal/geo"
	"rond-with-you/internal/ingest"
	"rond-with-you/internal/rond"
)

// movementFixture 是一份「刚导入、还没被改过」的数据集，外加可直接调用的后台服务。
//
// 需要本地才有的测试库配置与原始备份包，用环境变量开启（缺一个就跳过）：
//
//	ROND_TEST_CONF=conf/app-test.ini ROND_SRC=data/xxx.rondbackup go test ./internal/web -run Export -v
//
// 它会在测试库里建一个专用账号，跑完连同它的数据集一起删掉，不碰别的数据。
type movementFixture struct {
	cfg  *config.Config
	pg   *sql.DB
	srv  *Server
	dsID int64
	src  string          // 原始 .rondbackup 的路径
	ctx  context.Context // 已注入登录态，可直接喂给后台处理函数
}

func newMovementFixture(t *testing.T) *movementFixture {
	t.Helper()
	confPath, src := os.Getenv("ROND_TEST_CONF"), os.Getenv("ROND_SRC")
	if confPath == "" || src == "" {
		t.Skip("设置 ROND_TEST_CONF 与 ROND_SRC 后运行导入导出校验")
	}
	// go test 的工作目录是包目录（internal/web），而配置里的相对路径、以及调试模式下
	// 模板的磁盘路径（internal/web/templates/...）都是相对仓库根的，先切过去
	chdirModuleRoot(t)
	cfg, err := config.Load(confPath)
	if err != nil {
		t.Fatalf("加载配置 %s 失败: %v", confPath, err)
	}

	ctx := context.Background()
	pg, err := db.Open(cfg.DSN)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	// 注册得比后面几项早，t.Cleanup 是后进先出，所以连接会最后才关
	t.Cleanup(func() { pg.Close() })
	if err := db.Migrate(ctx, pg); err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}

	// 用一次性账号隔离：导入去重是按 (user_id, sha256) 判的，共用账号会命中
	// 上一次跑剩的数据集，测的就不是「刚导入的这份」了。
	// 顺带清掉上次跑到一半留下的账号，免得越攒越多。
	purgeE2EUsers(t, ctx, pg, cfg)
	user := fmt.Sprintf("e2e-movement-%d", time.Now().UnixNano())
	hash, err := auth.HashPassword("e2e-password")
	if err != nil {
		t.Fatalf("生成口令哈希失败: %v", err)
	}
	var uid int64
	if err := pg.QueryRowContext(ctx, `INSERT INTO users (username, password_hash, display_name)
		VALUES ($1,$2,'e2e') RETURNING id`, user, hash).Scan(&uid); err != nil {
		t.Fatalf("创建测试账号失败: %v", err)
	}
	var prevActive int64
	pg.QueryRowContext(ctx, `SELECT COALESCE(id,0) FROM datasets WHERE is_active LIMIT 1`).Scan(&prevActive)
	t.Cleanup(func() {
		dropE2EUser(t, ctx, pg, cfg, uid)
		if prevActive > 0 {
			pg.ExecContext(ctx, `UPDATE datasets SET is_active=(id=$1) WHERE id=$1`, prevActive)
		}
	})

	// 导入原始备份包
	rdb, err := rond.Open(src, t.TempDir())
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", src, err)
	}
	defer rdb.Close()
	backup, err := rond.Parse(rdb)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", src, err)
	}
	sha, size, err := ingest.File(src)
	if err != nil {
		t.Fatalf("计算摘要失败: %v", err)
	}
	dsID, _, err := ingest.Import(ctx, pg, uid, filepath.Base(src), sha, size, backup)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	// 导出取基线库时要按 sha 在 uploads 目录里找回原包，这里补一份
	if _, err := os.Stat(cfg.UploadDir); errors.Is(err, os.ErrNotExist) {
		os.MkdirAll(cfg.UploadDir, 0o755)
	}
	archived := filepath.Join(cfg.UploadDir, fmt.Sprintf("e2e-%d-%s", time.Now().Unix(), filepath.Base(src)))
	if err := copyTo(archived, src); err != nil {
		t.Fatalf("复制原始包到上传目录失败: %v", err)
	}
	t.Cleanup(func() { os.Remove(archived) })

	srv, err := New(cfg, pg)
	if err != nil {
		t.Fatalf("初始化服务失败: %v", err)
	}
	return &movementFixture{cfg: cfg, pg: pg, srv: srv, dsID: dsID, src: src,
		ctx: ctxWithUser(&auth.User{ID: uid, Username: user})}
}

// openSource 再开一次原始包，用来跟导出结果做对照。
func (f *movementFixture) openSource(t *testing.T) *sql.DB {
	t.Helper()
	db, err := rond.Open(f.src, t.TempDir())
	if err != nil {
		t.Fatalf("打开原始包失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// export 走后台真实的导出处理函数，返回 .rondbackup 的字节。
func (f *movementFixture) export(t *testing.T) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/export/rondbackup", nil).WithContext(f.ctx)
	f.srv.exportRondbackup(rec, req)
	if rec.Code != 200 {
		t.Fatalf("导出失败: code=%d body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// TestMovementCRUDExportE2E 端到端验证「后台增改删行程 → 导出 rondbackup」这条链路。
//
// 真的导入、真的调用后台处理函数、真的导出，然后直接读导出的 SQLite 逐项核对：
// 新建/修改的位移有没有带上 rond 必需的列（ZTYPE_ 等）、派生字段是否落库、
// 删掉的是否真没了、以及没被碰过的数据是不是一字未动。
func TestMovementCRUDExportE2E(t *testing.T) {
	f := newMovementFixture(t)
	ctx, pg, dsID := f.ctx, f.pg, f.dsID
	srv, base := f.srv, f.ctx

	// 选一对合法的起止到访：起点有离开时间，终点到访在其之后
	var fromVisit, toVisit int
	if err := pg.QueryRowContext(ctx, `SELECT vf.src_pk, vt.src_pk
		FROM visits vf JOIN visits vt ON vt.dataset_id=vf.dataset_id AND vt.arrival > vf.departure + interval '30 min'
		WHERE vf.dataset_id=$1 AND vf.departure IS NOT NULL
		ORDER BY vf.arrival DESC LIMIT 1`, dsID).Scan(&fromVisit, &toVisit); err != nil {
		t.Fatalf("挑不出合法的起止到访: %v", err)
	}
	var wantStart, wantEnd time.Time
	if err := pg.QueryRowContext(ctx, `SELECT vf.departure, vt.arrival FROM visits vf JOIN visits vt
		ON vt.dataset_id=vf.dataset_id AND vt.src_pk=$3
		WHERE vf.dataset_id=$1 AND vf.src_pk=$2`, dsID, fromVisit, toVisit).Scan(&wantStart, &wantEnd); err != nil {
		t.Fatalf("读取两端到访时间失败: %v", err)
	}
	t.Logf("起止到访 #%d → #%d，合法区间 %v ~ %v", fromVisit, toVisit, wantStart.Local(), wantEnd.Local())

	// 3) 编号复用防护：先删掉基线里编号最大的那条位移，再新建。
	// 若按「库内 MAX(src_pk)+1」分配，这里会原样拿回 791——那个号在基线里已存在，
	// 导出时新行会被当成旧行去 UPDATE，ZTYPE_ 之类就继承成别人的值。
	var baselineMax int
	if err := pg.QueryRowContext(ctx, `SELECT COALESCE(MAX(src_pk),0) FROM movements WHERE dataset_id=$1`,
		dsID).Scan(&baselineMax); err != nil {
		t.Fatal(err)
	}
	post(t, srv.editMovementDelete, base, "/admin/edit/movement/delete", url.Values{
		"src_pk": {fmt.Sprint(baselineMax)},
	})
	post(t, srv.editMovementCreate, base, "/admin/edit/movement", url.Values{
		"from_visit": {fmt.Sprint(fromVisit)},
		"to_visit":   {fmt.Sprint(toVisit)},
	})
	plainMove, _ := lastMovement(t, ctx, pg, dsID)
	if plainMove <= baselineMax {
		t.Errorf("删掉 #%d 后新建的行程拿到了 #%d：复用了基线里的编号", baselineMax, plainMove)
	}
	t.Logf("删掉基线里最大的位移 #%d 后新建得到 #%d", baselineMax, plainMove)

	// 4) 新建：时间留空（应自动推断）、顺带新建一个交通方式
	post(t, srv.editMovementCreate, base, "/admin/edit/movement", url.Values{
		"from_visit":      {fmt.Sprint(fromVisit)},
		"to_visit":        {fmt.Sprint(toVisit)},
		"transport_src":   {transportNewValue},
		"transport_name":  {"共享单车"},
		"transport_color": {"green"},
		"transport_icon":  {"bicycle"},
	})
	newMove, newTrans := lastMovement(t, ctx, pg, dsID)
	if newTrans <= 0 {
		t.Fatal("新建交通方式没有分配编号")
	}
	t.Logf("新建行程 src_pk=%d，交通方式 #%d", newMove, newTrans)

	// 5) 改：把新建那条的时间改成区间内的一小段，并把方式改名。
	// 表单是分钟精度，断言也要按分钟对齐，否则差的就是被截掉的秒数
	mid := wantStart.Add(wantEnd.Sub(wantStart) / 3).Truncate(time.Minute)
	post(t, srv.editMovementUpdate, base, "/admin/edit/movement/update", url.Values{
		"src_pk":          {fmt.Sprint(newMove)},
		"from_visit":      {fmt.Sprint(fromVisit)},
		"to_visit":        {fmt.Sprint(toVisit)},
		"start":           {mid.Format("2006-01-02T15:04")},
		"end":             {mid.Add(10 * time.Minute).Format("2006-01-02T15:04")},
		"transport_src":   {fmt.Sprint(newTrans)},
		"transport_name":  {"单车"},
		"transport_color": {"green"},
		"transport_icon":  {"bicycle"},
	})

	// 5b) 只改交通方式：时间必须原样不动。表单是分钟精度，回填后原样提交若被当成
	// 「改了时间」写回，真机的小数秒就被抹掉了，而且截断后还早于起点到访的离开时间，
	// 会被校验拒掉（线上就是「改方式提示开始时间不能早于离开时间」）。
	var pickPK, pickTrans, otherTrans int
	var pickFrom, pickTo int
	var pickSt, pickEn time.Time
	// 优先挑「把开始时间截到分钟就会早于起点到访的离开时间」的那类行——线上报的就是它
	const pickCols = `SELECT m.src_pk, m.transport_src, m.started_at, m.ended_at,
		m.from_visit_src, m.to_visit_src,
		(SELECT min(transport_src) FROM movements WHERE dataset_id=m.dataset_id
		 AND transport_src IS NOT NULL AND transport_src <> m.transport_src)`
	const pickWhere = ` FROM movements m
		JOIN visits vf ON vf.dataset_id=m.dataset_id AND vf.src_pk=m.from_visit_src
		WHERE m.dataset_id=$1 AND m.to_visit_src IS NOT NULL
		AND m.transport_src IS NOT NULL AND m.started_at IS NOT NULL AND m.ended_at IS NOT NULL`
	const pickTail = ` ORDER BY m.src_pk LIMIT 1`
	err := pg.QueryRowContext(ctx, pickCols+pickWhere+` AND date_trunc('minute', m.started_at) < vf.departure`+pickTail, dsID).
		Scan(&pickPK, &pickTrans, &pickSt, &pickEn, &pickFrom, &pickTo, &otherTrans)
	if errors.Is(err, sql.ErrNoRows) {
		err = pg.QueryRowContext(ctx, pickCols+pickWhere+pickTail, dsID).
			Scan(&pickPK, &pickTrans, &pickSt, &pickEn, &pickFrom, &pickTo, &otherTrans)
	}
	if err != nil {
		t.Fatalf("挑不出可改方式的已有行程: %v", err)
	}
	var otherName, otherColor string
	pg.QueryRowContext(ctx, `SELECT COALESCE(max(transport_name),''), COALESCE(max(transport_color),'')
		FROM movements WHERE dataset_id=$1 AND transport_src=$2`, dsID, otherTrans).Scan(&otherName, &otherColor)
	post(t, srv.editMovementUpdate, base, "/admin/edit/movement/update", url.Values{
		"src_pk":          {fmt.Sprint(pickPK)},
		"from_visit":      {fmt.Sprint(pickFrom)},
		"to_visit":        {fmt.Sprint(pickTo)},
		"start":           {pickSt.Local().Format("2006-01-02T15:04")},
		"end":             {pickEn.Local().Format("2006-01-02T15:04")},
		"transport_src":   {fmt.Sprint(otherTrans)},
		"transport_name":  {otherName},
		"transport_color": {otherColor},
	})
	var gotSt, gotEn time.Time
	var gotTrans int
	if err := pg.QueryRowContext(ctx, `SELECT started_at, ended_at, transport_src FROM movements
		WHERE dataset_id=$1 AND src_pk=$2`, dsID, pickPK).Scan(&gotSt, &gotEn, &gotTrans); err != nil {
		t.Fatal(err)
	}
	if !gotSt.Equal(pickSt) || !gotEn.Equal(pickEn) {
		t.Errorf("只改方式却动了时间: %v~%v → %v~%v", pickSt, pickEn, gotSt, gotEn)
	}
	if gotTrans != otherTrans {
		t.Errorf("交通方式没改成: 期望 %d，实际 %d", otherTrans, gotTrans)
	}
	t.Logf("只改方式 #%d：%d→%d，时间保持 %v ~ %v", pickPK, pickTrans, gotTrans, gotSt, gotEn)

	// 5c) 坐标换算：真机里有一批点的显示坐标其实没做过 GCJ-02 偏移（留档里的原始
	// GPS 与显示坐标一字不差）。把它们转成 GCJ-02 后，导出包里
	// 「gcj(原始GPS) == 显示坐标」这条不变量必须重新成立，否则 rond 上还是偏的。
	var susPK int
	var susLat, susLon float64
	var susName, susCat, susProv, susCity, susDist string
	err = pg.QueryRowContext(ctx, `SELECT p.src_pk, p.lat, p.lon, COALESCE(p.name,''),
		COALESCE(p.poi_category,''), COALESCE(p.province,''), COALESCE(p.city,''), COALESCE(p.district,'')
		FROM places p`+placeRawJoin+` WHERE p.dataset_id=$1 AND `+suspectWGS+` ORDER BY p.src_pk LIMIT 1`,
		dsID).Scan(&susPK, &susLat, &susLon, &susName, &susCat, &susProv, &susCity, &susDist)
	if errors.Is(err, sql.ErrNoRows) {
		t.Error("这份数据集里应当有「显示坐标其实是 WGS-84」的点，一个都没判出来")
	} else if err != nil {
		t.Fatalf("挑选待换算的地点失败: %v", err)
	} else {
		nlat, nlon := geo.WGS84ToGCJ02(susLat, susLon)
		loc := post(t, srv.editPlaceUpdate, base, "/admin/edit/place/update", url.Values{
			"src_pk":   {fmt.Sprint(susPK)},
			"name":     {susName},
			"category": {susCat},
			"lat":      {fmt.Sprintf("%.6f", nlat)},
			"lon":      {fmt.Sprintf("%.6f", nlon)},
			"province": {susProv},
			"city":     {susCity},
			"district": {susDist},
		})
		if !strings.Contains(loc, "err=") && !strings.Contains(loc, url.QueryEscape("坐标移动")) {
			t.Errorf("保存后应提示坐标移动了多少，实际跳转: %s", loc)
		}
		var gotLat, gotLon float64
		pg.QueryRowContext(ctx, `SELECT lat, lon FROM places WHERE dataset_id=$1 AND src_pk=$2`,
			dsID, susPK).Scan(&gotLat, &gotLon)
		if absFloat(gotLat-nlat) > 1e-6 || absFloat(gotLon-nlon) > 1e-6 {
			t.Errorf("坐标没写进去: %.6f,%.6f（期望 %.6f,%.6f）", gotLat, gotLon, nlat, nlon)
		}
		var still int
		pg.QueryRowContext(ctx, `SELECT count(*) FROM places p`+placeRawJoin+
			` WHERE p.dataset_id=$1 AND p.src_pk=$2 AND `+suspectWGS, dsID, susPK).Scan(&still)
		if still != 0 {
			t.Error("换算过的点仍被判为「疑似 WGS-84」")
		}
		t.Logf("坐标换算 #%d %s：%.6f,%.6f → %.6f,%.6f", susPK, susName, susLat, susLon, nlat, nlon)
	}

	// 5d) 站点新建的地点（照片导入就是这一类，没有 rond 留档）：换算后导出时要把
	// 「换算前用户填的那个值」当成原始 GPS 写进 ZRAWLATITUDE/ZRAWLONGITUDE。
	// 新行的 ZRAW 是导出时按显示坐标反算的，所以换算一次正好把它还原成真值——
	// rond 里位置对了，原始 GPS 也留着。
	const siteLat, siteLon = 22.853216, 113.253067
	post(t, srv.editPlaceCreate, base, "/admin/edit/place", url.Values{
		"mode": {"new"}, "name": {"e2e-坐标换算"}, "lat": {fmt.Sprint(siteLat)}, "lon": {fmt.Sprint(siteLon)},
	})
	var sitePK int
	if err := pg.QueryRowContext(ctx, `SELECT src_pk FROM places WHERE dataset_id=$1 ORDER BY id DESC LIMIT 1`,
		dsID).Scan(&sitePK); err != nil {
		t.Fatalf("读取新建地点失败: %v", err)
	}
	siteConvLat, siteConvLon := geo.WGS84ToGCJ02(siteLat, siteLon)
	post(t, srv.editPlaceUpdate, base, "/admin/edit/place/update", url.Values{
		"src_pk": {fmt.Sprint(sitePK)},
		"name":   {"e2e-坐标换算"},
		"lat":    {fmt.Sprintf("%.6f", siteConvLat)},
		"lon":    {fmt.Sprintf("%.6f", siteConvLon)},
	})
	t.Logf("站点新建地点 #%d 换算：%.6f,%.6f → %.6f,%.6f", sitePK, siteLat, siteLon, siteConvLat, siteConvLon)

	// 6) 派生字段必须落库：网页地图靠 from/to_place_id 画线，访客侧的区域过滤也按它们判定，
	// 留空的话这段行程会绕开黑名单区域直接露出来
	var dist sql.NullFloat64
	var fromID, toID, dur sql.NullInt64
	if err := pg.QueryRowContext(ctx, `SELECT from_place_id, to_place_id, distance_km,
		COALESCE(duration_min,0) FROM movements WHERE dataset_id=$1 AND src_pk=$2`,
		dsID, newMove).Scan(&fromID, &toID, &dist, &dur); err != nil {
		t.Fatalf("读取新建行程失败: %v", err)
	}
	if !dur.Valid || dur.Int64 != 10 {
		t.Errorf("改后的时长应为 10 分钟，实际 %v", dur)
	}
	if !fromID.Valid || !toID.Valid {
		t.Error("新建行程的 from_place_id / to_place_id 为空：地图轨迹与区域过滤都会失灵")
	}
	if !dist.Valid || dist.Float64 <= 0 {
		t.Errorf("新建行程的里程没有算出来: %v", dist)
	}

	// 7) 编辑页要能渲染出来：行程列表的 SQL、交通方式聚合、模板执行都在这条路径上，
	// 光测处理函数看不见它们
	for _, q := range []string{"/admin/edit",
		fmt.Sprintf("/admin/edit?mt=%d", newTrans),
		fmt.Sprintf("/admin/edit?mp=%d", fromID.Int64),
		"/admin/edit?mfrom=2026-09-01&mto=2026-09-30",
		"/admin/edit?mt=none",
	} {
		rec := httptest.NewRecorder()
		srv.editPage(rec, httptest.NewRequest("GET", q, nil).WithContext(base))
		if rec.Code != 200 {
			t.Fatalf("渲染 %s 失败: code=%d", q, rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{"行程记录", "新增行程", "新建方式…"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 的页面里没有 %q", q, want)
			}
		}
	}

	// 8) 导出
	blob := f.export(t)
	out := filepath.Join(t.TempDir(), "export.rondbackup")
	if err := os.WriteFile(out, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if p := os.Getenv("ROND_EXPORT_OUT"); p != "" {
		os.WriteFile(p, blob, 0o644)
		t.Logf("导出包已保存到 %s", p)
	}
	edb, err := rond.Open(out, t.TempDir())
	if err != nil {
		t.Fatalf("打开导出包失败: %v", err)
	}
	defer edb.Close()

	// 9) 核对导出包
	var mvType sql.NullInt64
	var mvTrans sql.NullInt64
	var mvStart, mvEnd sql.NullFloat64
	var mvFrom, mvTo sql.NullInt64
	if err := edb.QueryRow(`SELECT ZTYPE_, ZTRANSPORT_, ZSTART_, ZEND_, ZVISITFROM_, ZVISITTO_
		FROM ZMOVEMENT WHERE Z_PK=?`, newMove).Scan(&mvType, &mvTrans, &mvStart, &mvEnd, &mvFrom, &mvTo); err != nil {
		t.Fatalf("导出包里找不到新建的行程 #%d: %v", newMove, err)
	}
	if !mvType.Valid {
		t.Error("新建行程的 ZTYPE_ 为空：真机 791/791 都有值，Core Data 里它是非可选属性")
	} else if mvType.Int64 != 5 {
		t.Errorf("有交通方式的新建行程 ZTYPE_ 应为 5，实际 %d", mvType.Int64)
	}
	if !mvTrans.Valid || int(mvTrans.Int64) != newTrans {
		t.Errorf("新建行程的 ZTRANSPORT_ 应为 %d，实际 %v", newTrans, mvTrans)
	}
	if int(mvFrom.Int64) != fromVisit || int(mvTo.Int64) != toVisit {
		t.Errorf("两端到访写回不对: %d/%d，应为 %d/%d", mvFrom.Int64, mvTo.Int64, fromVisit, toVisit)
	}
	if d := absFloat(mvStart.Float64 - appleSec(mid)); d > 1 {
		t.Errorf("开始时间偏差 %.3f 秒（应为改后的 %v）", d, mid.Local())
	}
	if d := absFloat(mvEnd.Float64 - appleSec(mid.Add(10*time.Minute))); d > 1 {
		t.Errorf("结束时间偏差 %.3f 秒", d)
	}

	var n int
	edb.QueryRow(`SELECT count(*) FROM ZMOVEMENT WHERE Z_PK=?`, baselineMax).Scan(&n)
	if n != 0 {
		t.Errorf("删掉的位移 #%d 仍在导出包里", baselineMax)
	}
	// 无交通方式的新建行程 → ZTYPE_ 应为 2；这条同时验证了编号没有复用
	// （复用了就会继承基线里那条记录的取值）
	var plainType sql.NullInt64
	if err := edb.QueryRow(`SELECT ZTYPE_ FROM ZMOVEMENT WHERE Z_PK=?`, plainMove).Scan(&plainType); err != nil {
		t.Fatalf("导出包里找不到 #%d: %v", plainMove, err)
	}
	if !plainType.Valid || plainType.Int64 != 2 {
		t.Errorf("无交通方式的新建行程 ZTYPE_ 应为 2，实际 %v", plainType)
	}

	// 交通方式：改名要作用到该方式的全部行程，导出后 ZTRANSPORT 只有一行这个名字
	var tName, tColor string
	if err := edb.QueryRow(`SELECT COALESCE(ZNAME_,''), COALESCE(ZCOLOR_,'') FROM ZTRANSPORT WHERE Z_PK=?`,
		newTrans).Scan(&tName, &tColor); err != nil {
		t.Fatalf("导出包里找不到新建的交通方式 #%d: %v", newTrans, err)
	}
	if tName != "单车" || tColor != "green" {
		t.Errorf("交通方式应为「单车/green」，实际「%s/%s」", tName, tColor)
	}
	// 原有交通方式一个都不能少（删掉某方式的最后一条行程 ≠ 删除这个方式）
	var transCount, unnamed int
	edb.QueryRow(`SELECT count(*), COALESCE(sum(CASE WHEN COALESCE(ZNAME_,'')='' THEN 1 ELSE 0 END),0)
		FROM ZTRANSPORT`).Scan(&transCount, &unnamed)
	if transCount != 6 {
		t.Errorf("ZTRANSPORT 应有 6 个（原 5 个 + 新建 1 个），实际 %d", transCount)
	}
	if unnamed != 0 {
		t.Errorf("有 %d 个交通方式没有名字", unnamed)
	}
	var dup int
	edb.QueryRow(`SELECT count(*) FROM (SELECT Z_PK FROM ZTRANSPORT GROUP BY Z_PK HAVING count(*)>1) x`).Scan(&dup)
	if dup != 0 {
		t.Error("ZTRANSPORT 出现重复主键")
	}
	// 全表 ZTYPE_ 非空
	var total, missing int
	edb.QueryRow(`SELECT count(*), COALESCE(sum(CASE WHEN ZTYPE_ IS NULL THEN 1 ELSE 0 END),0) FROM ZMOVEMENT`).
		Scan(&total, &missing)
	if missing > 0 {
		t.Errorf("ZMOVEMENT 共 %d 条，其中 %d 条 ZTYPE_ 为空", total, missing)
	}
	// 只改方式的那条，导出后的 ZSTART_/ZEND_ 必须与原始包逐位一致（没被分钟精度截断）
	srcDB := f.openSource(t)
	var origSt, origEn float64
	if err := srcDB.QueryRow(`SELECT ZSTART_, ZEND_ FROM ZMOVEMENT WHERE Z_PK=?`, pickPK).Scan(&origSt, &origEn); err != nil {
		t.Fatalf("原始包里找不到 #%d: %v", pickPK, err)
	}
	var expSt, expEn sql.NullFloat64
	if err := edb.QueryRow(`SELECT ZSTART_, ZEND_ FROM ZMOVEMENT WHERE Z_PK=?`, pickPK).Scan(&expSt, &expEn); err != nil {
		t.Fatalf("导出包里找不到 #%d: %v", pickPK, err)
	}
	if d := absFloat(expSt.Float64 - origSt); d > 1e-6 {
		t.Errorf("只改方式却把 ZSTART_ 改了 %.6f 秒（%.6f → %.6f）", d, origSt, expSt.Float64)
	}
	if d := absFloat(expEn.Float64 - origEn); d > 1e-6 {
		t.Errorf("只改方式却把 ZEND_ 改了 %.6f 秒", d)
	}

	// 换算过的那个点：导出包里 gcj(原始GPS) 必须等于显示坐标（rond 的坐标模型），
	// 且原始 GPS 本身不能被动过——它才是这条记录真正的 WGS-84。
	if susPK > 0 {
		var rawLat, rawLon, disLat, disLon float64
		if err := edb.QueryRow(`SELECT ZRAWLATITUDE, ZRAWLONGITUDE, ZLATITUDE, ZLONGITUDE
			FROM ZLOCATION WHERE Z_PK=?`, susPK).Scan(&rawLat, &rawLon, &disLat, &disLon); err != nil {
			t.Fatalf("导出包里找不到换算过的地点 #%d: %v", susPK, err)
		}
		expLat, expLon := geo.WGS84ToGCJ02(rawLat, rawLon)
		if absFloat(expLat-disLat) > 1e-6 || absFloat(expLon-disLon) > 1e-6 {
			t.Errorf("导出包里 gcj(原始GPS) 与显示坐标对不上：gcj(%.6f,%.6f)=%.6f,%.6f vs %.6f,%.6f",
				rawLat, rawLon, expLat, expLon, disLat, disLon)
		}
		if absFloat(rawLat-susLat) > 1e-9 {
			t.Errorf("原始 GPS 被改动了: %.6f → %.6f", susLat, rawLat)
		}
		t.Logf("导出核对：%s 的 gcj(原始GPS)=%.6f,%.6f == 显示坐标 ✓", susName, expLat, expLon)
	}

	// 站点新建的那个地点：显示坐标是换算后的 GCJ-02，原始 GPS 应写回换算前用户填的值
	{
		var zrawLat, zrawLon, zdisLat, zdisLon float64
		if err := edb.QueryRow(`SELECT ZRAWLATITUDE, ZRAWLONGITUDE, ZLATITUDE, ZLONGITUDE FROM ZLOCATION
			WHERE Z_PK=?`, sitePK).Scan(&zrawLat, &zrawLon, &zdisLat, &zdisLon); err != nil {
			t.Fatalf("导出包里找不到新建地点 #%d: %v", sitePK, err)
		}
		if absFloat(zdisLat-siteConvLat) > 1e-6 || absFloat(zdisLon-siteConvLon) > 1e-6 {
			t.Errorf("新建地点的显示坐标应是换算后的值: %.6f,%.6f", zdisLat, zdisLon)
		}
		if absFloat(zrawLat-siteLat) > 1e-6 || absFloat(zrawLon-siteLon) > 1e-6 {
			t.Errorf("新建地点的原始 GPS 应写回换算前输入的值 %.6f,%.6f，实际 %.6f,%.6f",
				siteLat, siteLon, zrawLat, zrawLon)
		}
		if gLat, gLon := geo.WGS84ToGCJ02(zrawLat, zrawLon); absFloat(gLat-zdisLat) > 1e-6 || absFloat(gLon-zdisLon) > 1e-6 {
			t.Errorf("gcj(原始GPS) 与显示坐标对不上: %.6f,%.6f", gLat, gLon)
		}
		t.Logf("新建地点导出核对：原始 GPS %.6f,%.6f / 显示坐标 %.6f,%.6f ✓", zrawLat, zrawLon, zdisLat, zdisLon)
	}

	// 未落库的实体必须原样带回来
	var raw, rawAfter int
	srcDB.QueryRow(`SELECT count(*) FROM ZRAWVISIT`).Scan(&raw)
	edb.QueryRow(`SELECT count(*) FROM ZRAWVISIT`).Scan(&rawAfter)
	if raw != rawAfter {
		t.Errorf("ZRAWVISIT 行数变了: %d → %d", raw, rawAfter)
	}
	t.Logf("导出核对通过：位移 %d 条、交通方式 %d 个、ZRAWVISIT %d 条", total, transCount, rawAfter)
}

// TestMovementCleanExport 只做「导入后原样导出」，把包写到 ROND_CLEAN_OUT。
// 它是上面那个用例的对照组：一个没被改过的数据集导出来必须是纯净往返，
// 交给 tools/verify_rondbackup.py 逐字段比对，用来证明这轮改动没有波及别处。
func TestMovementCleanExport(t *testing.T) {
	out := os.Getenv("ROND_CLEAN_OUT")
	if out == "" {
		t.Skip("设置 ROND_CLEAN_OUT 后导出一份纯净往返包")
	}
	f := newMovementFixture(t)
	if err := os.WriteFile(out, f.export(t), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("纯净往返包已保存到 %s", out)
}

// dropE2EUser 删掉测试账号：数据集表上的外键会级联清掉各事实表与整行留档，
// 留在磁盘上的基线库文件要自己删。
func dropE2EUser(t *testing.T, ctx context.Context, pg *sql.DB, cfg *config.Config, uid int64) {
	t.Helper()
	var ids []int64
	rows, err := pg.QueryContext(ctx, `SELECT id FROM datasets WHERE user_id=$1`, uid)
	if err != nil {
		t.Logf("清理测试账号失败: %v", err)
		return
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if _, err := pg.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, uid); err != nil {
		t.Logf("清理测试账号失败: %v", err)
	}
	for _, id := range ids {
		os.Remove(filepath.Join(cfg.DataDir, "baselines", fmt.Sprintf("%d.sqlite", id)))
	}
}

// purgeE2EUsers 清掉上次跑到一半留下的测试账号（它的 username 前缀是本测试专用的）。
func purgeE2EUsers(t *testing.T, ctx context.Context, pg *sql.DB, cfg *config.Config) {
	t.Helper()
	var ids []int64
	rows, err := pg.QueryContext(ctx, `SELECT id FROM users WHERE username LIKE 'e2e-movement-%'`)
	if err != nil {
		return
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		t.Logf("清掉上次遗留的测试账号 #%d", id)
		dropE2EUser(t, ctx, pg, cfg, id)
	}
}

// chdirModuleRoot 把工作目录切到仓库根（含 go.mod 的那一层）。
// 调试模式下模板是从磁盘按「internal/web/templates/...」读的，配置里的
// storage 路径也是相对仓库根，只有站在仓库根上才和真跑起来时一致。
func chdirModuleRoot(t *testing.T) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if err := os.Chdir(dir); err != nil {
				t.Fatal(err)
			}
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到 go.mod，无法定位仓库根目录")
		}
		dir = parent
	}
}

// post 直接调用后台处理函数，返回重定向地址（带 ok=/err= 提示）。
// 绕过 requireAdmin 是刻意的：这里要测的是处理逻辑本身，登录态由 ctxWithUser 直接注入。
func post(t *testing.T, h func(http.ResponseWriter, *http.Request), ctx context.Context, path string, form url.Values) string {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	h(w, r)
	// 处理函数出错时会 500；正常路径是 303 重定向 + ?err= 提示
	if w.Code >= 500 {
		t.Fatalf("%s 处理失败: code=%d", path, w.Code)
	}
	loc := w.Header().Get("Location")
	if strings.Contains(loc, "err=") {
		t.Fatalf("%s 被拒绝: %s", path, loc)
	}
	return loc
}

func ctxWithUser(u *auth.User) context.Context {
	return context.WithValue(context.Background(), userKey{}, u)
}

func lastMovement(t *testing.T, ctx context.Context, pg *sql.DB, datasetID int64) (int, int) {
	t.Helper()
	var pk int
	var tr sql.NullInt64
	if err := pg.QueryRowContext(ctx, `SELECT src_pk, transport_src FROM movements
		WHERE dataset_id=$1 ORDER BY id DESC LIMIT 1`, datasetID).Scan(&pk, &tr); err != nil {
		t.Fatalf("读取刚写入的行程失败: %v", err)
	}
	return pk, int(tr.Int64)
}

func copyTo(dst, src string) error {
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

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func appleSec(t time.Time) float64 {
	return float64(t.Unix()-978307200) + float64(t.Nanosecond())/1e9
}
