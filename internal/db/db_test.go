package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestOpenCreatesMissingDatabase 验证「配置里写了不存在的库，启动时自动建出来」。
// 需要一个可连的库：用 ROND_DSN 指过来（会临时建一个库并在结束时删掉），未设置时跳过。
func TestOpenCreatesMissingDatabase(t *testing.T) {
	dsn := os.Getenv("ROND_DSN")
	if dsn == "" {
		t.Skip("设置 ROND_DSN 后运行自动建库检查")
	}
	name := fmt.Sprintf("rond_test_autocreate_%d", time.Now().UnixNano())
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析 ROND_DSN: %v", err)
	}
	u.Path = "/" + name
	target := u.String()

	d, err := Open(target)
	if err != nil {
		t.Fatalf("库不存在时应自动建出来再连，实际报错: %v", err)
	}
	defer func() {
		d.Close()
		// DROP DATABASE 要求没有连接挂着，所以先关掉上面的句柄
		dropDatabase(t, dsn, name)
	}()

	// 库真的建出来了
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM pg_database WHERE datname=$1`, name).Scan(&n); err != nil {
		t.Fatalf("查 pg_database: %v", err)
	}
	if n != 1 {
		t.Fatalf("库 %s 未被创建（匹配到 %d 条）", name, n)
	}
	// 建表也能正常跑（否则「建了个空库」没有意义）
	if err := Migrate(context.Background(), d); err != nil {
		t.Fatalf("初始化表结构: %v", err)
	}
	var tables int
	if err := d.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables < 10 {
		t.Errorf("表结构不完整，只有 %d 张表", tables)
	}
}

// TestOpenExistingDatabase 守住常规路径：库已存在时不该多做事（也不该报错）。
func TestOpenExistingDatabase(t *testing.T) {
	dsn := os.Getenv("ROND_DSN")
	if dsn == "" {
		t.Skip("设置 ROND_DSN 后运行连接检查")
	}
	d, err := Open(dsn)
	if err != nil {
		t.Fatalf("连接既有库失败: %v", err)
	}
	d.Close()
}

// TestDatabaseMissingDetection 确认只有「库不存在」会被当成需要建库的信号，
// 否则连错服务器、口令错也会被当成「那就建个新的」。
func TestDatabaseMissingDetection(t *testing.T) {
	if isDatabaseMissing(nil) {
		t.Error("nil 不该判为库不存在")
	}
	if !isDatabaseMissing(fmt.Errorf(`failed to connect: database "rond_x" does not exist (SQLSTATE 3D000)`)) {
		t.Error("库不存在的错误应被识别")
	}
	for _, msg := range []string{
		"failed SASL auth: 用户 \"postgres\" Password 认证失败 (SQLSTATE 28P01)",
		"dial tcp 127.0.0.1:5499: connectex: No connection could be made",
	} {
		if isDatabaseMissing(fmt.Errorf("%s", msg)) {
			t.Errorf("不该判为库不存在: %s", msg)
		}
	}
}

func dropDatabase(t *testing.T, dsn, name string) {
	t.Helper()
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Logf("清理时解析 dsn 失败: %v", err)
		return
	}
	cc.Database = "postgres"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		t.Logf("清理时连维护库失败: %v", err)
		return
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Logf("删除测试库失败: %v", err)
	}
}
