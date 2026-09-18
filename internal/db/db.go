package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema.sql
var schemaSQL string

// WarmUp 在后台把连接先建好放进池里。
//
// 建连接是这套系统里最贵的一步（见上面 SetMaxIdleConns 的注释），冷启动时
// 第一个页面会把这笔开销全吃掉；并发建 n 条（不是串行）只需约一次建连的时间。
// 失败就放弃——真实的报错由后续请求自己暴露，这里不必打扰启动流程。
func WarmUp(d *sql.DB, n int) {
	for i := 0; i < n; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			conn, err := d.Conn(ctx)
			if err != nil {
				return
			}
			conn.Close() // 归还到池里，之后就是热连接
		}()
	}
}

func Open(dsn string) (*sql.DB, error) {
	d, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	// 空闲连接数必须与上限相同：PostgreSQL 建连接的开销远大于查询本身
	// （本机实测建连 ~300ms、查询 ~2ms，psql 也一样）。池里留不住连接时，
	// 每个并发请求都要重新建连，页面耗时反而比串行还高。
	d.SetMaxOpenConns(8)
	d.SetMaxIdleConns(8)
	d.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = d.PingContext(ctx)
	if err == nil {
		return d, nil
	}
	// 配置文件里写了一个还不存在的库：先用维护库把它建出来再连一次。
	// 只在「库不存在」时兜底——口令错、端口不通这些要原样报出来，
	// 否则连错服务器也会被当成「建个新的」。
	if !isDatabaseMissing(err) {
		d.Close()
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	if cerr := createDatabase(ctx, dsn); cerr != nil {
		d.Close()
		return nil, fmt.Errorf("数据库不存在，自动创建失败: %w", cerr)
	}
	if err := d.PingContext(ctx); err != nil {
		d.Close()
		return nil, fmt.Errorf("连接 PostgreSQL 失败（库已创建）: %w", err)
	}
	return d, nil
}

// isDatabaseMissing 判断错误是不是「目标库不存在」（SQLSTATE 3D000）。
// 驱动有时只给文本，所以再按消息兜一层。
func isDatabaseMissing(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "3D000" {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database") && strings.Contains(msg, "does not exist")
}

// createDatabase 连到维护库把 dsn 里指定的库建出来。
// 库名走 pgx.Identifier 转义，配置里写什么名字都不会拼坏语句。
func createDatabase(ctx context.Context, dsn string) error {
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("解析 dsn 失败: %w", err)
	}
	name := cc.Database
	if name == "" {
		return errors.New("dsn 里没有指定库名")
	}
	// 维护库优先 postgres，个别部署会删掉它，那就退回 template1
	var lastErr error
	for _, maint := range []string{"postgres", "template1"} {
		mc, err := pgx.ParseConfig(dsn)
		if err != nil {
			return fmt.Errorf("解析 dsn 失败: %w", err)
		}
		mc.Database = maint
		conn, err := pgx.ConnectConfig(ctx, mc)
		if err != nil {
			lastErr = err
			continue
		}
		_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
		conn.Close(ctx)
		if err != nil {
			// 并发启动时可能已经被另一个实例建好了，那就算成功
			if strings.Contains(strings.ToLower(err.Error()), "already exists") {
				return nil
			}
			return fmt.Errorf("%w（当前账号可能没有 CREATEDB 权限）", err)
		}
		log.Printf("数据库 %s 不存在，已自动创建", name)
		return nil
	}
	return fmt.Errorf("连不上维护库（postgres / template1）: %w", lastErr)
}

func Migrate(ctx context.Context, d *sql.DB) error {
	if _, err := d.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("初始化表结构失败: %w", err)
	}
	return nil
}

func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, "SQLSTATE 23505") || contains(s, "duplicate key value")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
