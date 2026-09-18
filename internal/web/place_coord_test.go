package web

import (
	"database/sql"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rond-with-you/internal/geo"
	"rond-with-you/internal/ingest"
	"rond-with-you/internal/rond"
)

// TestPlaceCoordSource 盯住「坐标来源留档」这条链路：建点时就记下来、编辑时不被误改、
// 换算时跟着坐标一起修正。
//
// 为什么要这么细：2026-09 那次照片导入把 WGS-84 的 EXIF 当成 GCJ-02 存了进去，
// 63 个点整体偏 500~670m，而库里没有任何记录能说明「当初选的是什么坐标系」，
// 最后只能逐张翻原图 EXIF 才裁出来。留档就是为了让这种事下一次能一眼看出。
//
//	ROND_TEST_CONF=conf/app-test.ini ROND_SRC=data/xxx.rondbackup go test ./internal/web -run CoordSource -v
func TestPlaceCoordSource(t *testing.T) {
	f := newMovementFixture(t)

	// ---- 1. rond 导入：留档必须自动补上，且原始坐标就是留档里的 ZRAWLATITUDE ----
	var n, bad int
	if err := f.pg.QueryRowContext(f.ctx, `SELECT count(*),
		count(*) FILTER (WHERE coord_source<>'rond' OR coord_sys<>'wgs84'
			OR src_lat IS DISTINCT FROM (SELECT (r.raw->>'ZRAWLATITUDE')::float8 FROM entity_raw r
				WHERE r.dataset_id=places.dataset_id AND r.entity='ZLOCATION' AND r.src_pk=places.src_pk))
		FROM places WHERE dataset_id=$1`, f.dsID).Scan(&n, &bad); err != nil {
		t.Fatalf("统计留档失败: %v", err)
	}
	if n == 0 {
		t.Fatal("数据集里没有地点，导入没成功")
	}
	if bad != 0 {
		t.Fatalf("%d/%d 个地点没补上 rond 留档（或原始坐标与留档对不上）", bad, n)
	}

	// 挑一个「留档里的原始坐标 != 显示坐标」的点，也就是 rond 正常做过偏移的那种
	var pk int
	if err := f.pg.QueryRowContext(f.ctx, `SELECT src_pk FROM places
		WHERE dataset_id=$1 AND src_lat IS NOT NULL AND abs(src_lat-lat)>1e-9
		ORDER BY src_pk LIMIT 1`, f.dsID).Scan(&pk); err != nil {
		t.Fatalf("找不到做过偏移的点: %v", err)
	}

	type snapshot struct {
		lat, lon               float64
		coordSource, coordSys  string
		srcLat, srcLon         float64
		hasSrc                 bool
		name, province, street string
	}
	load := func() snapshot {
		t.Helper()
		var s snapshot
		var sl, so *float64
		if err := f.pg.QueryRowContext(f.ctx, `SELECT lat, lon, COALESCE(coord_source,''), COALESCE(coord_sys,''),
			src_lat, src_lon, COALESCE(name,''), COALESCE(province,''), COALESCE(thoroughfare,'')
			FROM places WHERE dataset_id=$1 AND src_pk=$2`, f.dsID, pk).
			Scan(&s.lat, &s.lon, &s.coordSource, &s.coordSys, &sl, &so, &s.name, &s.province, &s.street); err != nil {
			t.Fatalf("读地点失败: %v", err)
		}
		if sl != nil && so != nil {
			s.srcLat, s.srcLon, s.hasSrc = *sl, *so, true
		}
		return s
	}
	before := load()
	if !before.hasSrc {
		t.Fatal("rond 导入的点应当留下原始坐标")
	}

	// ---- 2. 只改名称：留档一个字都不能动，街道也不能被没提交的字段清掉 ----
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(f.ctx)
		rec := httptest.NewRecorder()
		switch path {
		case "/admin/edit/place/update":
			f.srv.editPlaceUpdate(rec, req)
		case "/admin/edit/place":
			f.srv.editPlaceCreate(rec, req)
		case "/admin/photos/import":
			f.srv.adminPhotosImport(rec, req)
		default:
			t.Fatalf("未接入的处理函数: %s", path)
		}
		return rec
	}

	// 先给这一行塞一个街道值，验「表单没提交就不该被清空」
	f.mustExec(t, `UPDATE places SET thoroughfare='测试街道' WHERE dataset_id=$1 AND src_pk=$2`, f.dsID, pk)

	// 批量换算工具就是这么提交的：只有坐标与地址，没有留档字段
	post("/admin/edit/place/update", url.Values{
		"src_pk": {fmt.Sprint(pk)}, "name": {before.name}, "category": {""},
		"lat": {fmt.Sprintf("%.6f", before.lat)}, "lon": {fmt.Sprintf("%.6f", before.lon)},
		"province": {before.province}, "city": {""}, "district": {""},
	})
	after := load()
	if after.coordSource != before.coordSource || after.coordSys != before.coordSys ||
		after.srcLat != before.srcLat || after.srcLon != before.srcLon {
		t.Fatalf("只改名称却动了留档：%+v -> %+v", before, after)
	}
	if after.street != "测试街道" {
		t.Fatalf("表单没提交街道，却被清成了 %q", after.street)
	}

	// ---- 3. 走了「坐标系换算」：留档要记成来源坐标系 + 换算前的坐标 ----
	// 复刻页面的真实行为：JavaScript 先调 /api/coords 把坐标换算好再提交，
	// 同时把「换算前的值 + 它属于哪个坐标系」一起带上（见 edit.html 的 coord-sel）。
	// 这里反过来做：把这条 rond 点的显示坐标（GCJ）当 WGS 提交，看留档跟不跟着走。
	// 隐藏字段是原值全精度带出来的（不是 %.6f），所以这里也用全精度提交，
	// 免得把留档里那份原始坐标截成 6 位。
	rawLat, rawLon := before.srcLat, before.srcLon
	subLat, subLon := geo.WGS84ToGCJ02(rawLat, rawLon)
	post("/admin/edit/place/update", url.Values{
		"src_pk": {fmt.Sprint(pk)}, "name": {before.name},
		"lat": {fmt.Sprintf("%.6f", subLat)}, "lon": {fmt.Sprintf("%.6f", subLon)},
		"province": {""}, "city": {""}, "district": {""},
		"coord_source": {"manual"}, "coord_sys": {"wgs84"},
		"src_lat": {fmt.Sprintf("%v", rawLat)}, "src_lon": {fmt.Sprintf("%v", rawLon)},
	})
	after = load()
	if after.coordSys != "wgs84" || after.coordSource != "manual" {
		t.Fatalf("留档没跟着换算走：%+v", after)
	}
	if after.srcLat != rawLat || after.srcLon != rawLon {
		t.Fatalf("原始坐标没按表单给的记：想要 %v,%v 实际 %v,%v", rawLat, rawLon, after.srcLat, after.srcLon)
	}
	// 显示坐标 == 按留档换算原始坐标（表单按 6 位小数提交，所以留 1e-6 的余量）
	if wlat, wlon := geo.WGS84ToGCJ02(after.srcLat, after.srcLon); abs(wlat-after.lat) > 1e-6 || abs(wlon-after.lon) > 1e-6 {
		t.Fatalf("「显示坐标 == 按留档换算原始坐标」不成立：%v,%v vs %v,%v", wlat, wlon, after.lat, after.lon)
	}

	// ---- 4. 明确选「未记录」：整组留档一起清掉 ----
	post("/admin/edit/place/update", url.Values{
		"src_pk": {fmt.Sprint(pk)}, "name": {before.name},
		"lat": {fmt.Sprintf("%.6f", after.lat)}, "lon": {fmt.Sprintf("%.6f", after.lon)},
		"province": {""}, "city": {""}, "district": {""},
		"coord_source": {"none"}, "coord_sys": {"none"},
	})
	after = load()
	if after.coordSource != "" || after.coordSys != "" || after.hasSrc {
		t.Fatalf("选了「未记录」却没清干净：%+v", after)
	}

	// ---- 5. 后台新增地点：来源 manual / 坐标系 gcj02（地图点选与高德搜索都是 GCJ） ----
	post("/admin/edit/place", url.Values{
		"mode": {"new"}, "name": {"留档测试点"}, "lat": {"30.123456"}, "lon": {"120.123456"},
	})
	var newPK int
	if err := f.pg.QueryRowContext(f.ctx, `SELECT src_pk FROM places WHERE dataset_id=$1 AND name='留档测试点'`,
		f.dsID).Scan(&newPK); err != nil {
		t.Fatalf("新增地点没落库: %v", err)
	}
	var cs, cy string
	var sl, so *float64
	if err := f.pg.QueryRowContext(f.ctx, `SELECT COALESCE(coord_source,''), COALESCE(coord_sys,''),
		src_lat, src_lon FROM places WHERE dataset_id=$1 AND src_pk=$2`, f.dsID, newPK).
		Scan(&cs, &cy, &sl, &so); err != nil {
		t.Fatalf("读新增点失败: %v", err)
	}
	if cs != "manual" || cy != "gcj02" || sl == nil || *sl != 30.123456 || so == nil || *so != 120.123456 {
		t.Fatalf("新增地点的留档不对：source=%q sys=%q src=%v,%v", cs, cy, sl, so)
	}

	// ---- 6. 照片导入：来源 photo，原始坐标是换算前那一份 ----
	// 选「已经是 GCJ-02」正是当初出事的选项，这里把它固化下来：一定要记下来。
	post("/admin/photos/import", url.Values{
		"n": {"1"}, "coord": {"gcj02"}, "keep_0": {"on"},
		"lat_0": {"22.853216"}, "lon_0": {"113.253067"},
		"arrival_0": {"2026-04-18T18:14"}, "name_0": {"照片留档测试点"},
		"country_0": {"CN"}, "timezone_0": {"Asia/Shanghai"},
	})
	var pcs, pcy string
	var psl, pso float64
	var plat, plon float64
	if err := f.pg.QueryRowContext(f.ctx, `SELECT coord_source, coord_sys, src_lat, src_lon, lat, lon
		FROM places WHERE dataset_id=$1 AND name='照片留档测试点'`, f.dsID).
		Scan(&pcs, &pcy, &psl, &pso, &plat, &plon); err != nil {
		t.Fatalf("照片导入没落库: %v", err)
	}
	if pcs != "photo" || pcy != "gcj02" {
		t.Fatalf("照片导入的留档不对：source=%q sys=%q", pcs, pcy)
	}
	if psl != 22.853216 || pso != 113.253067 {
		t.Fatalf("照片导入没记下换算前的原始坐标：%v,%v", psl, pso)
	}
	if plat != 22.853216 || plon != 113.253067 {
		t.Fatalf("选了「已经是 GCJ-02」就不该换算，实际存成了 %v,%v", plat, plon)
	}

	// 再导一张按 WGS-84 处理的：显示坐标必须是换算后的值，原始坐标保持 EXIF 原样
	post("/admin/photos/import", url.Values{
		"n": {"1"}, "coord": {"wgs84"}, "keep_0": {"on"},
		"lat_0": {"22.853216"}, "lon_0": {"113.253067"},
		"arrival_0": {"2026-04-18T18:14"}, "name_0": {"照片留档测试点2"},
		"country_0": {"CN"}, "timezone_0": {"Asia/Shanghai"},
	})
	if err := f.pg.QueryRowContext(f.ctx, `SELECT coord_source, coord_sys, src_lat, src_lon, lat, lon
		FROM places WHERE dataset_id=$1 AND name='照片留档测试点2'`, f.dsID).
		Scan(&pcs, &pcy, &psl, &pso, &plat, &plon); err != nil {
		t.Fatalf("照片导入（WGS）没落库: %v", err)
	}
	if pcs != "photo" || pcy != "wgs84" || psl != 22.853216 || pso != 113.253067 {
		t.Fatalf("WGS-84 照片导入的留档不对：source=%q sys=%q src=%v,%v", pcs, pcy, psl, pso)
	}
	if wlat, wlon := geo.WGS84ToGCJ02(psl, pso); abs(wlat-plat) > 1e-9 || abs(wlon-plon) > 1e-9 {
		t.Fatalf("显示坐标应当是换算后的值：%v,%v", plat, plon)
	}
}

// TestCoordSourceSkipsExportSynthetic 盯住回填判据里最容易踩的那个坑：站点导出的备份会
// 给新建地点**合成** ZLOCATION 行，把 ZRAWLATITUDE 写成 GCJ02ToWGS84(导出那一刻的显示坐标)。
// 这种包被回导之后，「有 ZLOCATION 留档」就不再等于「rond 导入」，早期回填据此把 62 个
// 照片导入的点全标成了 rond。判据必须落在「ZRAW 是否恰好等于 gcj2wgs(该行自己的 ZLAT)」上。
//
// 这里手工造两行留档：一行合成（ZRAW = gcj2wgs(ZLAT)），一行真机（ZRAW 是独立采样值），
// 然后跑回填，断言只有真机那行被认领、合成那行不碰。
func TestCoordSourceSkipsExportSynthetic(t *testing.T) {
	f := newMovementFixture(t)

	// 造两个站点新建的点：编号取在基线之外，模拟「导出时还不存在」的新建地点
	var maxPK int
	if err := f.pg.QueryRowContext(f.ctx, `SELECT COALESCE(max(src_pk),0) FROM places
		WHERE dataset_id=$1`, f.dsID).Scan(&maxPK); err != nil {
		t.Fatalf("读编号上限失败: %v", err)
	}
	synthPK, devicePK := maxPK+1, maxPK+2
	f.mustExec(t, `INSERT INTO places (dataset_id, src_pk, name, lat, lon) VALUES
		($1,$2,'合成行测试点',30.5,120.5), ($1,$3,'真机行测试点',30.6,120.6)`, f.dsID, synthPK, devicePK)

	// 合成行：ZRAW 严格等于 gcj2wgs(ZLAT)——正是 export.go 给新建地点的写法
	synLat, synLon := geo.GCJ02ToWGS84(30.5, 120.5)
	// 真机行：ZRAW 是独立采样值，显示坐标是 app 自己按 GCJ 偏移算的，两者不会位级吻合
	// （真机基线 321 行零命中就是这个原因）。这里按同样的关系构造，但不让往返精确相等。
	devRawLat, devRawLon := 30.7, 120.7
	devLat, devLon := geo.WGS84ToGCJ02(devRawLat, devRawLon)
	devLat, devLon = devLat+1e-9, devLon+1e-9

	raw := func(lat, lon, rlat, rlon float64) string {
		return fmt.Sprintf(`{"ZLATITUDE":%v,"ZLONGITUDE":%v,"ZRAWLATITUDE":%v,"ZRAWLONGITUDE":%v}`,
			lat, lon, rlat, rlon)
	}
	f.mustExec(t, `INSERT INTO entity_raw (dataset_id, entity, src_pk, raw, skipped) VALUES
		($1,'ZLOCATION',$2,$3::jsonb,false), ($1,'ZLOCATION',$4,$5::jsonb,false)`,
		f.dsID, synthPK, raw(30.5, 120.5, synLat, synLon), devicePK, raw(devLat, devLon, devRawLat, devRawLon))

	// 先故意把合成行写成早期回填的错误结果，验证回填会把它撤回
	f.mustExec(t, `UPDATE places SET coord_source='rond', coord_sys='wgs84', src_lat=$3, src_lon=$4
		WHERE dataset_id=$1 AND src_pk=$2`, f.dsID, synthPK, synLat, synLon)

	if _, err := ingest.BackfillCoordSource(f.ctx, f.pg, f.dsID); err != nil {
		t.Fatalf("回填失败: %v", err)
	}

	var source, sys string
	var srcLat, srcLon *float64
	load := func(pk int) {
		t.Helper()
		if err := f.pg.QueryRowContext(f.ctx, `SELECT COALESCE(coord_source,''), COALESCE(coord_sys,''),
			src_lat, src_lon FROM places WHERE dataset_id=$1 AND src_pk=$2`, f.dsID, pk).
			Scan(&source, &sys, &srcLat, &srcLon); err != nil {
			t.Fatalf("读留档失败: %v", err)
		}
	}

	// 合成行必须被撤回：它不是真机留档，来源应当是「未记录」
	load(synthPK)
	if source != "" || sys != "" || srcLat != nil || srcLon != nil {
		t.Fatalf("导出合成行没被撤回：source=%q sys=%q src=%v,%v", source, sys, srcLat, srcLon)
	}

	// 真机行必须被认领，且原始坐标就是留档里的 ZRAW
	load(devicePK)
	if source != "rond" || sys != "wgs84" || srcLat == nil || *srcLat != devRawLat || srcLon == nil || *srcLon != devRawLon {
		t.Fatalf("真机留档行没被正确认领：source=%q sys=%q src=%v,%v（想要 %v,%v）",
			source, sys, srcLat, srcLon, devRawLat, devRawLon)
	}

	// 再跑一次必须幂等（启动时每次都会跑）
	if _, err := ingest.BackfillCoordSource(f.ctx, f.pg, f.dsID); err != nil {
		t.Fatalf("二次回填失败: %v", err)
	}
	load(devicePK)
	if source != "rond" || srcLat == nil || *srcLat != devRawLat {
		t.Fatalf("回填不幂等：%q %v", source, srcLat)
	}
}

// TestExportPhotoImportFlag 盯住导出侧补的 ZISPHOTOIMPORT：rond 靠这一位区分
// 「照片导入」与「手工新增」，站点没有对应字段，只能从坐标来源留档推。
// 真机行（coord_source=rond）必须原样保留，不能被我们改写。
func TestExportPhotoImportFlag(t *testing.T) {
	f := newMovementFixture(t)

	// 照片导入一个点 → photo；手工新增一个点 → manual
	post := func(path string, form url.Values) {
		t.Helper()
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(f.ctx)
		rec := httptest.NewRecorder()
		switch path {
		case "/admin/photos/import":
			f.srv.adminPhotosImport(rec, req)
		case "/admin/edit/place":
			f.srv.editPlaceCreate(rec, req)
		default:
			t.Fatalf("未接入的处理函数: %s", path)
		}
	}
	post("/admin/photos/import", url.Values{
		"n": {"1"}, "coord": {"wgs84"}, "keep_0": {"on"},
		"lat_0": {"22.853216"}, "lon_0": {"113.253067"},
		"arrival_0": {"2026-04-18T18:14"}, "name_0": {"导出照片标志位测试点"},
		"country_0": {"CN"}, "timezone_0": {"Asia/Shanghai"},
	})
	post("/admin/edit/place", url.Values{
		"mode": {"new"}, "name": {"导出手工点测试点"}, "lat": {"30.123456"}, "lon": {"120.123456"},
	})

	var photoPK, manualPK int
	if err := f.pg.QueryRowContext(f.ctx, `SELECT src_pk FROM places
		WHERE dataset_id=$1 AND name='导出照片标志位测试点'`, f.dsID).Scan(&photoPK); err != nil {
		t.Fatalf("照片导入点没落库: %v", err)
	}
	if err := f.pg.QueryRowContext(f.ctx, `SELECT src_pk FROM places
		WHERE dataset_id=$1 AND name='导出手工点测试点'`, f.dsID).Scan(&manualPK); err != nil {
		t.Fatalf("手工点没落库: %v", err)
	}
	// 基线里的真机行：留档里记着它自己那份 ZISPHOTOIMPORT，导出后必须一字不改
	var devPK int
	if err := f.pg.QueryRowContext(f.ctx, `SELECT src_pk FROM places
		WHERE dataset_id=$1 AND coord_source='rond' ORDER BY src_pk LIMIT 1`, f.dsID).Scan(&devPK); err != nil {
		t.Fatalf("找不到真机点: %v", err)
	}
	srcDB := f.openSource(t)
	var devWant int
	if err := srcDB.QueryRow(`SELECT ZISPHOTOIMPORT FROM ZLOCATION WHERE Z_PK=?`, devPK).Scan(&devWant); err != nil {
		t.Fatalf("读原包 ZISPHOTOIMPORT 失败: %v", err)
	}

	out := filepath.Join(t.TempDir(), "export.rondbackup")
	if err := os.WriteFile(out, f.export(t), 0o644); err != nil {
		t.Fatal(err)
	}
	edb, err := rond.Open(out, t.TempDir())
	if err != nil {
		t.Fatalf("打开导出包失败: %v", err)
	}
	defer edb.Close()

	flagOf := func(pk int) int {
		t.Helper()
		var v sql.NullInt64
		if err := edb.QueryRow(`SELECT ZISPHOTOIMPORT FROM ZLOCATION WHERE Z_PK=?`, pk).Scan(&v); err != nil {
			t.Fatalf("读导出包 Z_PK=%d 失败: %v", pk, err)
		}
		if !v.Valid {
			return -1
		}
		return int(v.Int64)
	}
	if got := flagOf(photoPK); got != 1 {
		t.Errorf("照片导入的点 ZISPHOTOIMPORT 应为 1，实际 %d", got)
	}
	if got := flagOf(manualPK); got != 0 {
		t.Errorf("手工新增的点 ZISPHOTOIMPORT 应为 0，实际 %d", got)
	}
	if got := flagOf(devPK); got != devWant {
		t.Errorf("真机点 #%d 的 ZISPHOTOIMPORT 被改写：原有 %d，导出成 %d", devPK, devWant, got)
	}
}

// TestInlineSaveKeepsFullPrecisionCoord 盯住「行内保存会把坐标截到 6 位小数」。
// 表单的经纬度框是 %.6f，而真机基线的点带长小数（26.46800422211455 这种）。
// 只改备注/名称时原样回贴，旧实现会把坐标写短——导出与原始包逐位对照就对不上。
// 与时间字段同一套规矩：落在同一个 6 位小数上就沿用库里那份。
func TestInlineSaveKeepsFullPrecisionCoord(t *testing.T) {
	f := newMovementFixture(t)

	var pk int
	var lat0, lon0, srcLat0 float64
	if err := f.pg.QueryRowContext(f.ctx, `SELECT src_pk, lat, lon, src_lat FROM places
		WHERE dataset_id=$1 AND coord_source='rond' AND src_lat IS NOT NULL
		  AND lat <> round(lat::numeric, 6)::float8 ORDER BY src_pk LIMIT 1`, f.dsID).Scan(
		&pk, &lat0, &lon0, &srcLat0); err != nil {
		t.Skipf("本地基线的坐标都已经是 6 位，测不出截断: %v", err)
	}
	// 只改名称：经纬度按表单的 6 位小数原样回贴
	req := httptest.NewRequest("POST", "/admin/edit/place/update", strings.NewReader(url.Values{
		"src_pk": {fmt.Sprint(pk)}, "name": {"改了名字"},
		"lat": {fmt.Sprintf("%.6f", lat0)}, "lon": {fmt.Sprintf("%.6f", lon0)},
		"province": {""}, "city": {""}, "district": {""},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(f.ctx)
	f.srv.editPlaceUpdate(httptest.NewRecorder(), req)

	var lat1, lon1, srcLat1 float64
	var name string
	if err := f.pg.QueryRowContext(f.ctx, `SELECT lat, lon, src_lat, name FROM places
		WHERE dataset_id=$1 AND src_pk=$2`, f.dsID, pk).Scan(&lat1, &lon1, &srcLat1, &name); err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if name != "改了名字" {
		t.Fatalf("名称没改成功: %q", name)
	}
	if lat1 != lat0 || lon1 != lon0 {
		t.Errorf("只改名称却把坐标截短了：%v,%v → %v,%v", lat0, lon0, lat1, lon1)
	}
	if srcLat1 != srcLat0 {
		t.Errorf("原始坐标被动过：%v → %v", srcLat0, srcLat1)
	}
}

func (f *movementFixture) mustExec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.pg.ExecContext(f.ctx, q, args...); err != nil {
		t.Fatalf("执行 %s 失败: %v", q, err)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
