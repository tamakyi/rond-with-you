package setting

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestVisitorHomePath(t *testing.T) {
	full := Default() // Pages 默认全开

	noMap := full
	noMap.Pages = []string{"places", "stats"}

	cases := []struct {
		name string
		set  Settings
		want string
	}{
		{"默认停在主页", full, ""},
		{"空值（历史配置）停在主页", func() Settings { s := full; s.VisitorHome = ""; return s }(), ""},
		{"显式主页", func() Settings { s := full; s.VisitorHome = "home"; return s }(), ""},
		{"跳到地图", func() Settings { s := full; s.VisitorHome = "map"; return s }(), "/map"},
		{"跳到统计", func() Settings { s := full; s.VisitorHome = "stats"; return s }(), "/stats"},
		{"目标页没对访客开放则不跳", noMap, ""},
		{"非法值不跳", func() Settings { s := full; s.VisitorHome = "javascript:alert(1)"; return s }(), ""},
		{"不给访客任何页面时不跳", func() Settings { s := full; s.Pages = nil; s.VisitorHome = "map"; return s }(), ""},
	}
	for _, c := range cases {
		if got := c.set.VisitorHomePath(); got != c.want {
			t.Errorf("%s: VisitorHomePath = %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestNormalizeVisitorHome(t *testing.T) {
	for _, v := range []string{"", "home", "nope", "/map", "../admin", "MAP"} {
		if got := NormalizeVisitorHome(v); got != VisitorHomeMain {
			t.Errorf("NormalizeVisitorHome(%q) = %q，期望 %q", v, got, VisitorHomeMain)
		}
	}
	for _, p := range AllPages {
		if got := NormalizeVisitorHome(p.Key); got != p.Key {
			t.Errorf("NormalizeVisitorHome(%q) = %q，期望原样保留", p.Key, got)
		}
	}
}

// TestVisitorHomeStore 覆盖存取：旧配置（没有该键）必须回落到主页，
// 而不是把零值当成「访客落地到空路径」。
// 只在库名带 test 的库上跑——它会改写 settings 里的 site 键，不能碰正式库。
func TestVisitorHomeStore(t *testing.T) {
	dsn := os.Getenv("ROND_DSN")
	if dsn == "" {
		t.Skip("未设置 ROND_DSN")
	}
	if !strings.Contains(dsn, "test") {
		t.Skipf("库名不含 test，跳过以免动到正式站的设置：%s", dsn)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	st := Store{DB: db}

	var orig []byte
	had := true
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='site'`).Scan(&orig); err != nil {
		if err != sql.ErrNoRows {
			t.Fatal(err)
		}
		had = false
	}
	defer func() { // 还原原配置，别把测试库的设置留下改动
		if had {
			db.ExecContext(ctx, `UPDATE settings SET value=$1 WHERE key='site'`, orig)
		} else {
			db.ExecContext(ctx, `DELETE FROM settings WHERE key='site'`)
		}
	}()

	set := func(raw string) {
		if _, err := db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('site', $1)
			ON CONFLICT (key) DO UPDATE SET value=$1`, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}

	// 旧配置：整个键都不存在 → 默认停在主页
	set(`{"pages":["map","stats"]}`)
	if s, err := st.Load(ctx); err != nil {
		t.Fatal(err)
	} else if s.VisitorHome != VisitorHomeMain {
		t.Errorf("缺键时 VisitorHome = %q，期望 %q", s.VisitorHome, VisitorHomeMain)
	}

	// 正常往返
	s, err := st.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.VisitorHome = "map"
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if s2, err := st.Load(ctx); err != nil {
		t.Fatal(err)
	} else if s2.VisitorHome != "map" {
		t.Errorf("存回来成了 %q，期望 map", s2.VisitorHome)
	}

	// 库里被塞了非法值 → 读出来必须收敛到主页
	set(`{"pages":["map"],"visitor_home":"/etc/passwd"}`)
	if s3, err := st.Load(ctx); err != nil {
		t.Fatal(err)
	} else if s3.VisitorHome != VisitorHomeMain {
		t.Errorf("非法值读出 %q，期望收敛为 %q", s3.VisitorHome, VisitorHomeMain)
	}

	// 目标页没开放时不跳（即便设置里写着 map）
	set(`{"pages":["stats"],"visitor_home":"map"}`)
	if s4, err := st.Load(ctx); err != nil {
		t.Fatal(err)
	} else if got := s4.VisitorHomePath(); got != "" {
		t.Errorf("目标页未开放却仍要跳 %q", got)
	}
}
