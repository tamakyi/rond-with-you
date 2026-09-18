package web

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestNewRowNumberingSkipsArchivedPKs 盯住「站点新建行的编号」。
//
// 编号不能只看 `MAX(places.src_pk)`：`entity_raw` 里还躺着导入时缺必填字段的 skipped 行
// （导出要把它们原样补回去，所以一直留着），那些号没有对应实体行却占着号位。
// 只看 places 的话，新行会拿到一个已经被留档占用的号 —— 导出时被当成「已存在的行」走 UPDATE，
// 于是继承对方 rond 自己写的那些列（ZRADIUS / ZTYPE_ / ZNOTE_…）。
//
// 这里造一条「只有留档、没有实体行」的 ZLOCATION，再分别走两条新建路径，断言编号绕开它。
func TestNewRowNumberingSkipsArchivedPKs(t *testing.T) {
	f := newMovementFixture(t)

	// 与 nextSrcPK 同一套口径，先算出「当前有效上限」
	var cur int
	if err := f.pg.QueryRowContext(f.ctx, `SELECT GREATEST(
			(SELECT COALESCE(MAX(src_pk),0) FROM places WHERE dataset_id=$1),
			COALESCE((coredata_meta->'entities'->'Location'->>'max')::int,0),
			(SELECT COALESCE(MAX((raw->>'Z_PK')::numeric),0)::int FROM entity_raw
			  WHERE dataset_id=$1 AND entity='ZLOCATION' AND (raw->>'Z_PK') ~ '^[0-9]+$')
		) FROM datasets WHERE id=$1`, f.dsID).Scan(&cur); err != nil {
		t.Fatalf("算编号上限失败: %v", err)
	}
	stalePK := cur + 3
	// 留档存的是整行 Core Data 记录，Z_PK 就在里面——nextSrcPK 正是靠它算上限
	f.mustExec(t, `INSERT INTO entity_raw (dataset_id, entity, src_pk, raw, skipped)
		VALUES ($1,'ZLOCATION',$2,$3::jsonb,true)`, f.dsID, stalePK,
		fmt.Sprintf(`{"Z_PK":%d,"ZLATITUDE":30.0,"ZLONGITUDE":120.0}`, stalePK))

	post := func(path string, form url.Values) {
		t.Helper()
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(f.ctx)
		rec := httptest.NewRecorder()
		switch path {
		case "/admin/edit/place":
			f.srv.editPlaceCreate(rec, req)
		case "/admin/photos/import":
			f.srv.adminPhotosImport(rec, req)
		default:
			t.Fatalf("未接入的处理函数: %s", path)
		}
		if rec.Code >= 400 {
			t.Fatalf("%s 返回 %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	srcPKOf := func(name string) int {
		t.Helper()
		var pk int
		if err := f.pg.QueryRowContext(f.ctx, `SELECT src_pk FROM places
			WHERE dataset_id=$1 AND name=$2`, f.dsID, name).Scan(&pk); err != nil {
			t.Fatalf("地点 %q 没落库: %v", name, err)
		}
		return pk
	}

	// 路径一：后台「新增地点」
	post("/admin/edit/place", url.Values{
		"mode": {"new"}, "name": {"编号测试点-手工"}, "lat": {"30.11"}, "lon": {"120.11"},
	})
	if got := srcPKOf("编号测试点-手工"); got <= stalePK {
		t.Errorf("手工新增的编号 %d 没绕开留档占用的 %d（旧实现会拿到 %d）", got, stalePK, cur+1)
	}

	// 路径二：照片导入（同一套规则）
	post("/admin/photos/import", url.Values{
		"n": {"1"}, "coord": {"wgs84"}, "keep_0": {"on"},
		"lat_0": {"30.22"}, "lon_0": {"120.22"},
		"arrival_0": {"2026-04-18T18:14"}, "name_0": {"编号测试点-照片"},
		"country_0": {"CN"}, "timezone_0": {"Asia/Shanghai"},
	})
	if got := srcPKOf("编号测试点-照片"); got <= stalePK {
		t.Errorf("照片导入的编号 %d 没绕开留档占用的 %d", got, stalePK)
	}

	// 留档那条本身还在（newSrcPK 绕开了它，就不该被清掉）
	var n int
	if err := f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM entity_raw
		WHERE dataset_id=$1 AND entity='ZLOCATION' AND src_pk=$2`, f.dsID, stalePK).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("留档行被误清：期望 1 行，实际 %d", n)
	}
}
