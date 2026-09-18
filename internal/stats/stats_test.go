package stats_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"rond-with-you/internal/stats"
)

// 这些是只读的对照测试：把统计结果和「手写 SQL 数一遍」的结果比对，
// 专门钉住「区域限制漏在某个查询里」这类问题（轨迹、里程、出行方式都犯过）。
func setup(t *testing.T) (*sql.DB, int64) {
	t.Helper()
	dsn := os.Getenv("ROND_DSN")
	if dsn == "" {
		t.Skip("未设置 ROND_DSN")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var id int64
	if err := db.QueryRow(`SELECT id FROM datasets WHERE is_active LIMIT 1`).Scan(&id); err != nil {
		t.Skipf("没有生效数据集：%v", err)
	}
	return db, id
}

func TestOverviewMatchesManualSQL(t *testing.T) {
	db, dsID := setup(t)
	d := stats.DB{DB: db}
	f := stats.Filter{DatasetID: dsID}

	got, err := d.Overview(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	var places, cities, days, visits, movements int
	var dist float64
	if err := db.QueryRow(`SELECT count(DISTINCT v.place_id), count(DISTINCT p.city),
			count(DISTINCT (v.arrival AT TIME ZONE 'Asia/Shanghai')::date), count(*)
		FROM visits v LEFT JOIN places p ON p.id = v.place_id WHERE v.dataset_id=$1`, dsID).
		Scan(&places, &cities, &days, &visits); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COALESCE(sum(distance_km),0), count(*) FROM movements WHERE dataset_id=$1`, dsID).
		Scan(&dist, &movements); err != nil {
		t.Fatal(err)
	}
	if got.Places != places || got.Cities != cities || got.Days != days || got.Visits != visits {
		t.Errorf("总览与手工 SQL 不一致：%+v vs places=%d cities=%d days=%d visits=%d",
			got, places, cities, days, visits)
	}
	if diff := got.DistanceKm - dist; diff > 0.01 || diff < -0.01 {
		t.Errorf("里程 %.4f，手工 SQL %.4f", got.DistanceKm, dist)
	}
	if got.Movements != movements {
		t.Errorf("行程数 %d，手工 SQL %d", got.Movements, movements)
	}
}

// 区域限制必须同时作用于 visits 与 movements：只改其中一边，
// 访客会看到「到访次数藏了、里程还留着」这种自相矛盾的数字。
func TestRegionFilterAppliesToVisitsAndMovements(t *testing.T) {
	db, dsID := setup(t)
	d := stats.DB{DB: db}
	ctx := context.Background()

	// 挑一个有记录的省份做白名单，避免用空区域（那样等于没限制）
	var region string
	if err := db.QueryRow(`SELECT p.province FROM places p JOIN visits v ON v.place_id=p.id
		WHERE v.dataset_id=$1 AND p.province IS NOT NULL AND p.province <> ''
		GROUP BY 1 ORDER BY count(*) DESC LIMIT 1`, dsID).Scan(&region); err != nil {
		t.Skipf("找不到可用省份：%v", err)
	}

	all, err := d.Overview(ctx, stats.Filter{DatasetID: dsID})
	if err != nil {
		t.Fatal(err)
	}
	only, err := d.Overview(ctx, stats.Filter{DatasetID: dsID, Regions: []string{region}})
	if err != nil {
		t.Fatal(err)
	}
	if only.Visits >= all.Visits {
		t.Errorf("白名单 %s 后到访数没减少：%d → %d", region, all.Visits, only.Visits)
	}
	if only.DistanceKm >= all.DistanceKm {
		t.Errorf("白名单 %s 后里程没减少：%.1f → %.1f（movements 的区域过滤可能漏了）",
			region, all.DistanceKm, only.DistanceKm)
	}

	// 黑名单：反过来，排除这个省后剩下的应等于「全部 - 只有它」
	var other string
	if err := db.QueryRow(`SELECT p.province FROM places p JOIN visits v ON v.place_id=p.id
		WHERE v.dataset_id=$1 AND p.province IS NOT NULL AND p.province <> '' AND p.province <> $2
		GROUP BY 1 ORDER BY count(*) DESC LIMIT 1`, dsID, region).Scan(&other); err == nil {
		notRegion, err := d.Overview(ctx, stats.Filter{DatasetID: dsID, Regions: []string{region}, RegionsExclude: true})
		if err != nil {
			t.Fatal(err)
		}
		if notRegion.Visits == all.Visits {
			t.Errorf("黑名单 %s 后到访数没减少：仍为 %d", region, all.Visits)
		}
	}

	// 出行方式统计同样要跟着缩水
	tAll, err := d.TransportStats(ctx, stats.Filter{DatasetID: dsID})
	if err != nil {
		t.Fatal(err)
	}
	tOnly, err := d.TransportStats(ctx, stats.Filter{DatasetID: dsID, Regions: []string{region}})
	if err != nil {
		t.Fatal(err)
	}
	sum := func(ts []stats.TransportStat) float64 {
		var v float64
		for _, x := range ts {
			v += x.Distance
		}
		return v
	}
	if sum(tOnly) >= sum(tAll) {
		t.Errorf("白名单 %s 后出行方式里程没减少：%.1f → %.1f", region, sum(tAll), sum(tOnly))
	}
}

// 过滤条件不该把时间范围弄丢。
func TestOverviewTimeRangeStillApplies(t *testing.T) {
	db, dsID := setup(t)
	d := stats.DB{DB: db}
	ctx := context.Background()

	all, err := d.Overview(ctx, stats.Filter{DatasetID: dsID})
	if err != nil {
		t.Fatal(err)
	}
	if all.FirstAt == nil || all.LastAt == nil {
		t.Skip("没有时间范围可测")
	}
	mid := all.FirstAt.Add(all.LastAt.Sub(*all.FirstAt) / 2)
	half, err := d.Overview(ctx, stats.Filter{DatasetID: dsID, From: &mid})
	if err != nil {
		t.Fatal(err)
	}
	if half.Visits >= all.Visits {
		t.Errorf("限定后半段后到访数没减少：%d → %d", all.Visits, half.Visits)
	}
	if half.LastAt == nil || all.LastAt == nil || half.LastAt.Before(mid) {
		t.Errorf("后半段的最后时间 %v 早于起点 %v", half.LastAt, mid)
	}
}

// TestWeatherStatsRespectsFilter 天气统计必须跟着筛选走。
//
// 以前 WeatherStats 声明了 Filter 却只用了 DatasetID，HourlyTemp 干脆只收 datasetID：
// 统计页筛完之后「气温」还是全量汇总，跟同一页的到访数对不上。
func TestWeatherStatsRespectsFilter(t *testing.T) {
	db, dsID := setup(t)
	d := stats.DB{DB: db}
	ctx := context.Background()

	var minAt, maxAt time.Time
	if err := db.QueryRow(`SELECT min(at), max(at) FROM weather WHERE dataset_id=$1`, dsID).
		Scan(&minAt, &maxAt); err != nil {
		t.Skipf("没有天气数据可测：%v", err)
	}
	// 取「最后一个月」当区间：它必然只覆盖一部分天气
	from := time.Date(maxAt.Year(), maxAt.Month(), 1, 0, 0, 0, 0, maxAt.Location())
	if !from.After(minAt) {
		t.Skip("天气数据只落在一个月内，构造不出有效区间")
	}

	sum := func(ws []stats.WeatherStat) int {
		n := 0
		for _, w := range ws {
			n += w.Count
		}
		return n
	}

	all, err := d.WeatherStats(ctx, stats.Filter{DatasetID: dsID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.WeatherStats(ctx, stats.Filter{DatasetID: dsID, From: &from})
	if err != nil {
		t.Fatal(err)
	}

	// 与手写 SQL 对账
	var wantTotal, wantWindow int
	if err := db.QueryRow(`SELECT count(*) FROM weather WHERE dataset_id=$1`, dsID).Scan(&wantTotal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM weather w WHERE w.dataset_id=$1 AND w.at >= $2`,
		dsID, from).Scan(&wantWindow); err != nil {
		t.Fatal(err)
	}

	// 分组数是 12 上限，只有没截断时求和才等于总数
	if len(got) < 12 && sum(got) != wantWindow {
		t.Errorf("按时间筛选后天气行数 %d，手写 SQL %d", sum(got), wantWindow)
	}
	if wantWindow < wantTotal && sum(got) >= sum(all) {
		t.Errorf("筛了时间但天气行数没减少（%d → %d），筛选没生效", sum(all), sum(got))
	}

	// 逐月气温同样要跟着区间走
	temps, err := d.HourlyTemp(ctx, stats.Filter{DatasetID: dsID, From: &from})
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 1 {
		t.Errorf("区间只覆盖 %s 一个月，逐月气温应当只剩 1 组，实际 %d 组：%+v",
			from.Format("2006-01"), len(temps), temps)
	}

	// 城市筛选：只有归属于该城市到访的天气算数
	var city string
	if err := db.QueryRow(`SELECT p.city FROM weather w
		JOIN visits v ON v.dataset_id=w.dataset_id AND v.src_pk=w.visit_src
		JOIN places p ON p.id=v.place_id
		WHERE w.dataset_id=$1 AND p.city IS NOT NULL
		GROUP BY 1 ORDER BY count(*) DESC LIMIT 1`, dsID).Scan(&city); err != nil {
		t.Skipf("没有带城市的天气：%v", err)
	}
	byCity, err := d.WeatherStats(ctx, stats.Filter{DatasetID: dsID, City: city})
	if err != nil {
		t.Fatal(err)
	}
	var wantCity int
	if err := db.QueryRow(`SELECT count(*) FROM weather w
		WHERE w.dataset_id=$1 AND EXISTS (SELECT 1 FROM visits v
			WHERE v.dataset_id=w.dataset_id AND v.src_pk=w.visit_src
			AND EXISTS (SELECT 1 FROM places px WHERE px.id=v.place_id AND px.city=$2))`,
		dsID, city).Scan(&wantCity); err != nil {
		t.Fatal(err)
	}
	if len(byCity) < 12 && sum(byCity) != wantCity {
		t.Errorf("按城市「%s」筛选后天气行数 %d，手写 SQL %d", city, sum(byCity), wantCity)
	}
	if sum(byCity) >= wantTotal {
		t.Errorf("按城市筛选后行数（%d）不该等于全量（%d）", sum(byCity), wantTotal)
	}
}
