package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestChangeUsername 覆盖改名的正向路径、口令校验与占用冲突。
// 会临时建一个用户并在结束时删掉，所以需要一个可连的库：用 ROND_DSN 指过来，
// 未设置时跳过（与项目里其它按环境变量门控的测试一致）。
func TestChangeUsername(t *testing.T) {
	dsn := os.Getenv("ROND_DSN")
	if dsn == "" {
		t.Skip("设置 ROND_DSN 后运行改名检查")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("连接数据库: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	name := fmt.Sprintf("tmp_rename_%d", time.Now().UnixNano())
	hash, err := HashPassword("tmp-pass-123456")
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO users (username, password_hash) VALUES ($1,$2) RETURNING id`, name, hash).Scan(&id); err != nil {
		t.Fatalf("建临时用户: %v", err)
	}
	defer db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, id)

	st := &Store{DB: db, Secret: []byte("test-secret"), Days: 7}
	// 改名不该影响已登录会话：cookie 认的是 id 与口令摘要，都不含用户名
	tok := st.SignToken(id, hash)

	next := name + "_new"
	if err := st.ChangeUsername(ctx, id, "wrong-password", next); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("口令错误应返回 ErrInvalidCredentials，实际 %v", err)
	}

	// 换成一个已存在的用户名（库里除自己之外随便取一个）
	var other string
	if err := db.QueryRowContext(ctx, `SELECT username FROM users WHERE id<>$1 LIMIT 1`, id).Scan(&other); err == nil {
		if err := st.ChangeUsername(ctx, id, "tmp-pass-123456", other); !errors.Is(err, ErrUsernameTaken) {
			t.Errorf("占用他人用户名应返回 ErrUsernameTaken，实际 %v", err)
		}
	}

	if err := st.ChangeUsername(ctx, id, "tmp-pass-123456", next); err != nil {
		t.Fatalf("正常改名失败: %v", err)
	}
	u, err := st.UserByID(ctx, id)
	if err != nil {
		t.Fatalf("回读用户: %v", err)
	}
	if u.Username != next {
		t.Errorf("用户名应为 %q，实际 %q", next, u.Username)
	}
	if got, ok := st.ParseToken(tok, hash); !ok || got != id {
		t.Errorf("改名后旧会话应仍有效，实际 ok=%v id=%d", ok, got)
	}
	// 密码没被动过：改完还能用原口令登录
	if _, err := st.Authenticate(ctx, next, "tmp-pass-123456"); err != nil {
		t.Errorf("改名后原口令应仍可登录: %v", err)
	}
}
