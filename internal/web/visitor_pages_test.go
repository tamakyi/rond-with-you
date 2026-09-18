package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// withVisitorRegions 临时给「访客可见区域」配上一项，跑完还原。
//
// 这类 bug 只在访客视角 + 配了区域时才犯（`visitorRegions` 对站长返回空），
// 站长自己点一遍是好的，所以必须专门造这个组合。
func withVisitorRegions(t *testing.T, f *movementFixture) {
	t.Helper()
	var prev []byte
	if err := f.pg.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key='site'`).Scan(&prev); err != nil {
		t.Fatalf("读站点设置失败: %v", err)
	}
	write := func(v []byte) {
		t.Helper()
		if _, err := f.pg.ExecContext(f.ctx,
			`UPDATE settings SET value=$1::jsonb WHERE key='site'`, string(v)); err != nil {
			t.Fatalf("写站点设置失败: %v", err)
		}
	}
	write([]byte(`{"regions":["河池市"],"regions_mode":"blacklist"}`))
	t.Cleanup(func() { write(prev) })
}

// TestVisitorPagesWithRegionFilter 用**访客身份**把公开页面走一遍，只要求不 500。
//
// 专为「某个查询漏了区域条件、或者参数与占位符对不上」准备：这类错误犯过两次——
// 一次是 movements 的里程没跟着区域走，一次是专题的「天数」查询把上一条 SQL 的参数
// 整个传进来（多了区域那几个），pgx 报 expected N arguments, got M；
// 而专题列表会把错误吞掉，表现成「专题内容全变 0、点进去才 500」。
// 两次都只在访客视角复现，站长自己试是好的——所以这个组合必须专门测。
func TestVisitorPagesWithRegionFilter(t *testing.T) {
	f := newMovementFixture(t)
	withVisitorRegions(t, f)

	var placeID, topicID int64
	if err := f.pg.QueryRowContext(f.ctx, `SELECT id FROM places WHERE dataset_id=$1 ORDER BY id LIMIT 1`,
		f.dsID).Scan(&placeID); err != nil {
		t.Fatalf("取地点失败: %v", err)
	}
	// 专题页正是这次踩坑的地方，没有现成的就建一个覆盖几天到访的
	if err := f.pg.QueryRowContext(f.ctx, `SELECT id FROM topics WHERE dataset_id=$1 ORDER BY id LIMIT 1`,
		f.dsID).Scan(&topicID); err != nil {
		if err := f.pg.QueryRowContext(f.ctx, `INSERT INTO topics
			(dataset_id, user_id, title, start_at, end_at, enabled)
			VALUES ($1,$2,'访客冒烟','2020-01-01T00:00:00+08:00','2030-01-01T00:00:00+08:00',true)
			RETURNING id`, f.dsID, userFrom(f.ctx).ID).Scan(&topicID); err != nil {
			t.Fatalf("建测试专题失败: %v", err)
		}
		t.Cleanup(func() {
			f.pg.ExecContext(f.ctx, `DELETE FROM topics WHERE id=$1`, topicID)
		})
	}

	pages := []struct {
		name   string
		path   string
		handle http.HandlerFunc
		param  string // PathValue("id") 用真实 id 填充
	}{
		{"主页", "/", f.srv.home, ""},
		{"地点库", "/places", f.srv.placesPage, ""},
		{"地点详情", "/places/x", f.srv.placePage, strconv.FormatInt(placeID, 10)},
		{"统计", "/stats", f.srv.statsPage, ""},
		{"时间线", "/timeline", f.srv.timelinePage, ""},
		{"年度报告", "/report", f.srv.reportPage, ""},
		{"地图", "/map", f.srv.mapPage, ""},
		{"专题列表", "/topics", f.srv.topicsListPage, ""},
		{"专题详情", "/topics/x", f.srv.topicPage, strconv.FormatInt(topicID, 10)},
		{"点位接口", "/api/points", f.srv.apiPoints, ""},
		{"轨迹接口", "/api/track", f.srv.apiTrack, ""},
	}
	for _, p := range pages {
		t.Run(p.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", p.path, nil)
			// 访客 = 上下文里没有登录态（withUser 中间件没跑）
			req = req.WithContext(context.Background())
			if p.param != "" {
				req.SetPathValue("id", p.param)
			}
			rec := httptest.NewRecorder()
			p.handle(rec, req)
			if rec.Code >= 500 {
				t.Errorf("%s 访客访问返回 %d：%s", p.path, rec.Code, firstLine(rec.Body.String()))
			}
		})
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
