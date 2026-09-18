package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rond-with-you/internal/auth"
	"rond-with-you/internal/config"
	"rond-with-you/internal/db"
	"rond-with-you/internal/ingest"
	"rond-with-you/internal/web"
)

func main() {
	confPath := flag.String("c", "conf/app.ini", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*confPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	database, err := db.Open(cfg.DSN)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}
	defer database.Close()

	// 预建几条连接：PG 建连约 300ms，冷启动第一个页面会全吃掉
	db.WarmUp(database, 6)
	if err := db.Migrate(context.Background(), database); err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	// 自愈历史留档：早期回填的判据把「站点导出时给新建地点合成的 ZLOCATION 行」
	// 当成了真机的原始 GPS，还误用了显示经度当原始经度。这里按 GCJ-02 往返关系重算。
	if err := ingest.BackfillAllCoordSource(context.Background(), database); err != nil {
		log.Printf("坐标来源留档自愈失败（不影响启动）: %v", err)
	}

	adminUser := os.Getenv("ROND_ADMIN_USER")
	if adminUser == "" {
		adminUser = "admin"
	}
	adminPass := os.Getenv("ROND_ADMIN_PASSWORD")
	if adminPass == "" {
		adminPass = "rond-admin"
	}
	store := &auth.Store{DB: database, Secret: []byte(cfg.SecretKey), Days: cfg.SessionDays}
	if created, err := store.EnsureDefaultUser(context.Background(), adminUser, adminPass); err != nil {
		log.Fatalf("初始化管理员失败: %v", err)
	} else if created {
		log.Printf("已创建初始管理员：%s / %s（请登录后立即修改密码）", adminUser, adminPass)
	}

	srv, err := web.New(cfg, database)
	if err != nil {
		log.Fatalf("初始化服务失败: %v", err)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	go func() {
		log.Printf("足迹站点已启动：http://localhost%s（后台 /admin）", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务异常退出: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	log.Println("已退出")
}
