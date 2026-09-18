package web

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rond-with-you/internal/auth"
	"rond-with-you/internal/captcha"
	"rond-with-you/internal/config"
	"rond-with-you/internal/media"
	"rond-with-you/internal/notes"
	"rond-with-you/internal/setting"
	"rond-with-you/internal/stats"
	"rond-with-you/internal/votes"
)

//go:embed templates/*.html templates/admin/*.html
var tmplFS embed.FS

//go:embed static
var staticFS embed.FS

// pageFiles 每个页面单独成组解析：页面文件都定义 "content"，
// 与 layout.html 一起构成一个独立模板集，避免定义名冲突。
var pageFiles = map[string]struct {
	files []string
	entry string
}{
	"home":              {[]string{"templates/layout.html", "templates/home.html"}, "layout"},
	"map":               {[]string{"templates/layout.html", "templates/map.html"}, "layout"},
	"places":            {[]string{"templates/layout.html", "templates/places.html"}, "layout"},
	"place":             {[]string{"templates/layout.html", "templates/place.html"}, "layout"},
	"timeline":          {[]string{"templates/layout.html", "templates/timeline.html"}, "layout"},
	"stats":             {[]string{"templates/layout.html", "templates/stats.html"}, "layout"},
	"report":            {[]string{"templates/layout.html", "templates/report.html"}, "layout"},
	"admin/index":       {[]string{"templates/layout.html", "templates/admin/index.html"}, "layout"},
	"admin/settings":    {[]string{"templates/layout.html", "templates/admin/settings.html"}, "layout"},
	"admin/password":    {[]string{"templates/layout.html", "templates/admin/password.html"}, "layout"},
	"admin/login":       {[]string{"templates/admin/login.html"}, "login"},
	"admin/twofa-login": {[]string{"templates/admin/twofa-login.html"}, "login2"},
	"admin/twofa":       {[]string{"templates/layout.html", "templates/admin/twofa.html"}, "layout"},
	"admin/fog-mask":    {[]string{"templates/layout.html", "templates/admin/fog-mask.html"}, "layout"},
	"admin/notes":       {[]string{"templates/layout.html", "templates/admin/notes.html"}, "layout"},
	"admin/edit":        {[]string{"templates/layout.html", "templates/admin/edit.html"}, "layout"},
	"admin/topics":      {[]string{"templates/layout.html", "templates/admin/topics.html"}, "layout"},
	"admin/photos":      {[]string{"templates/layout.html", "templates/admin/photos.html"}, "layout"},
	"topics":            {[]string{"templates/layout.html", "templates/topics.html"}, "layout"},
	"topic":             {[]string{"templates/layout.html", "templates/topic.html"}, "layout"},
}

type pageSet struct {
	t     *template.Template
	entry string
}

type Server struct {
	cfg       *config.Config
	db        *sql.DB
	q         stats.DB
	auth      *auth.Store
	sets      *setting.Store
	notes     *notes.Store
	votes     *votes.Store
	pages     map[string]pageSet
	mux       *http.ServeMux
	cookie    string
	tiles     tileSet
	tileCache *tileCache
	// fogTiles 缓存渲染好的迷雾瓦片：每张都要现画逐格位图再 PNG 编码，
	// 而内容只随「迷雾版本 + 遮罩 + 底图坐标系」变化。
	fogTiles *tileCache
	// masks 缓存遮罩列表：它不在 fogIndex 里，不然每张瓦片都要查一次库。
	masks maskCache
	// captcha 存登录验证码的答案（内存、一次性、带过期）
	captcha *captcha.Store
	// media 管地点图片写哪（本地磁盘 / 对象存储）与怎么取址
	media *media.Store
}

func New(cfg *config.Config, db *sql.DB) (*Server, error) {
	initAssetVersion(cfg.Debug)
	pages := make(map[string]pageSet, len(pageFiles))
	for key, def := range pageFiles {
		t, err := template.New(filepath.Base(def.files[0])).Funcs(templateFuncs()).
			ParseFS(tmplFS, def.files...)
		if err != nil {
			return nil, fmt.Errorf("解析模板 %s 失败: %w", key, err)
		}
		pages[key] = pageSet{t: t, entry: def.entry}
	}
	s := &Server{
		cfg:       cfg,
		db:        db,
		q:         stats.DB{DB: db},
		auth:      &auth.Store{DB: db, Secret: []byte(cfg.SecretKey), Days: cfg.SessionDays},
		sets:      &setting.Store{DB: db},
		notes:     &notes.Store{DB: db},
		votes:     &votes.Store{DB: db},
		pages:     pages,
		mux:       http.NewServeMux(),
		cookie:    "rond_session",
		tiles:     buildTileSet(cfg),
		tileCache: newTileCache(1024),
		fogTiles:  newTileCache(512),
		captcha:   captcha.NewStore(10 * time.Minute),
		media:     media.New(cfg),
	}
	s.routes()
	return s, nil
}

// guardPage 拦截「站长已关闭」的公开页面：管理员照常访问，访客拿到 404。
// withUser 在 mux 之前运行，这里能直接拿到登录态。
func (s *Server) guardPage(nav string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r.Context()) == nil {
			if sets, err := s.sets.Load(r.Context()); err == nil && !sets.HasPage(nav) {
				s.pausePage(w, http.StatusNotFound, "🔒", "该页面未对访客开放",
					"站长还没有把这一页开放给访客。", 0)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Handler() http.Handler {
	return s.recover(gzipHandler(s.logRequests(s.withUser(s.mux))))
}

// cacheStatic 给带了当前版本号的静态资源加长期强缓存。
// 版本号随编译产物变化（静态文件内容哈希），旧链接即使被缓存也不会命中新版本。
func (s *Server) cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.URL.Query().Get("v"); v != "" && v == assetVersion {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

// withUser 为所有请求解析登录态。公开页面也要知道访问者是访客还是站长，
// 才能决定是否渲染「含家/工作」这类只给自己看的开关。
// 未携带会话 cookie 时不会查库，匿名访问没有额外开销。
func (s *Server) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, err := s.sessionUser(r); err == nil && u != nil {
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	// 静态资源：调试模式下直接读磁盘，改样式/脚本不用重新编译。
	// 页面链接都带 ?v=<版本>，版本一致才给一年强缓存，重新编译后版本变了自然失效。
	if s.cfg.Debug {
		s.mux.Handle("GET /static/", s.cacheStatic(
			http.StripPrefix("/static/", http.FileServer(http.Dir("internal/web/static")))))
	} else {
		sub, _ := fs.Sub(staticFS, "static")
		s.mux.Handle("GET /static/", s.cacheStatic(
			http.StripPrefix("/static/", http.FileServer(http.FS(sub)))))
	}

	// 公开页面：受「公开页面」开关控制，关闭后仅管理员可访问
	s.mux.HandleFunc("GET /{$}", s.home)
	s.mux.Handle("GET /map", s.guardPage("map", s.mapPage))
	s.mux.Handle("GET /places", s.guardPage("places", s.placesPage))
	s.mux.HandleFunc("GET /places/{id}", s.placePage)
	s.mux.Handle("GET /timeline", s.guardPage("timeline", s.timelinePage))
	s.mux.Handle("GET /stats", s.guardPage("stats", s.statsPage))
	s.mux.HandleFunc("GET /api/points", s.apiPoints)
	s.mux.HandleFunc("GET /api/track", s.apiTrack)
	s.mux.HandleFunc("GET /api/cities", s.apiCityAgg)
	s.mux.HandleFunc("GET /api/gpx", s.apiGpx)
	s.mux.HandleFunc("GET /api/votes", s.apiVotes)
	s.mux.HandleFunc("POST /api/vote", s.apiVote)
	s.mux.HandleFunc("POST /api/verdict", s.requireAdmin(s.apiVerdict))
	s.mux.HandleFunc("GET /api/map-config", s.mapConfig)
	// 地理编码走服务端代理，高德密钥不出现在前端源码里；仅管理员可用，避免配额被外部消耗
	s.mux.HandleFunc("GET /api/geocode", s.requireAdmin(s.apiGeocode))
	s.mux.HandleFunc("GET /api/coords", s.requireAdmin(s.apiCoords))
	s.mux.HandleFunc("POST /api/regeo", s.requireAdmin(s.apiRegeo))
	s.mux.HandleFunc("GET /tiles/{layer}/{z}/{x}/{y}", s.handleTile)
	s.mux.HandleFunc("GET /tiles/fog/{z}/{x}/{y}", s.handleFogTile)
	// 专题：把某段时间 / 某个区域的足迹打包成可对外展示的页面。
	// 不做页面级开关——访客能不能看由「每个专题的展示开关」决定（详情页对未展示的专题 404）。
	s.mux.HandleFunc("GET /admin/topics", s.requireAdmin(s.topicsAdminPage))
	s.mux.HandleFunc("POST /admin/topics/save", s.requireAdmin(s.adminTopicSave))
	s.mux.HandleFunc("POST /admin/topics/delete", s.requireAdmin(s.adminTopicDelete))
	s.mux.HandleFunc("POST /admin/topics/toggle", s.requireAdmin(s.adminTopicToggle))
	s.mux.HandleFunc("GET /topics", s.topicsListPage)
	s.mux.HandleFunc("GET /topics/{id}", s.topicPage)
	s.mux.Handle("GET /report", s.guardPage("report", s.reportPage))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.db.PingContext(r.Context()); err != nil {
			http.Error(w, "db down", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok")
	})

	// 后台
	s.mux.HandleFunc("GET /captcha", s.captchaImage)
	s.mux.HandleFunc("GET /admin/login", s.loginPage)
	s.mux.HandleFunc("POST /admin/login", s.loginSubmit)
	s.mux.HandleFunc("POST /admin/login/twofa", s.login2FA)
	s.mux.HandleFunc("POST /admin/logout", s.logout)
	s.mux.HandleFunc("GET /admin/twofa", s.requireAdmin(s.twofaPage))
	s.mux.HandleFunc("POST /admin/twofa/setup", s.requireAdmin(s.twofaSetup))
	s.mux.HandleFunc("POST /admin/twofa/confirm", s.requireAdmin(s.twofaConfirm))
	s.mux.HandleFunc("POST /admin/twofa/disable", s.requireAdmin(s.twofaDisable))
	s.mux.HandleFunc("GET /admin", s.requireAdmin(s.adminHome))
	s.mux.HandleFunc("POST /admin/upload", s.requireAdmin(s.adminUpload))
	s.mux.HandleFunc("POST /admin/datasets/{id}/activate", s.requireAdmin(s.adminActivate))
	s.mux.HandleFunc("POST /admin/datasets/{id}/delete", s.requireAdmin(s.adminDelete))
	s.mux.HandleFunc("POST /admin/place/note", s.requireAdmin(s.adminPlaceNote))
	s.mux.HandleFunc("POST /admin/place/vote", s.requireAdmin(s.adminPlaceVote))
	// 地点图片：字节由浏览器转成 WebP 后发过来，这里只负责收下、落库、取址
	s.mux.HandleFunc("POST /admin/place/images", s.requireAdmin(s.adminPlaceImageUpload))
	s.mux.HandleFunc("POST /admin/place/images/delete", s.requireAdmin(s.adminPlaceImageDelete))
	// 本地存储的图片直出；对象存储的图走公开域名，不经过这里
	s.mux.HandleFunc("GET /media/", s.serveMedia)
	s.mux.HandleFunc("POST /admin/notes/bulk", s.requireAdmin(s.adminNotesBulk))
	s.mux.HandleFunc("POST /admin/notes/rond", s.requireAdmin(s.adminNotesRond))
	s.mux.HandleFunc("GET /admin/notes", s.requireAdmin(s.notesPage))
	// 数据编辑：手动补录记录点，改完可导出回 rondbackup
	s.mux.HandleFunc("GET /admin/edit", s.requireAdmin(s.editPage))
	s.mux.HandleFunc("POST /admin/edit/place", s.requireAdmin(s.editPlaceCreate))
	s.mux.HandleFunc("POST /admin/edit/place/update", s.requireAdmin(s.editPlaceUpdate))
	s.mux.HandleFunc("POST /admin/edit/place/delete", s.requireAdmin(s.editPlaceDelete))
	s.mux.HandleFunc("POST /admin/edit/visit/update", s.requireAdmin(s.editVisitUpdate))
	s.mux.HandleFunc("POST /admin/edit/activity", s.requireAdmin(s.editActivityCreate))
	s.mux.HandleFunc("POST /admin/edit/activity/update", s.requireAdmin(s.editActivityUpdate))
	s.mux.HandleFunc("POST /admin/edit/activity/delete", s.requireAdmin(s.editActivityDelete))
	// 照片导入：EXIF 由浏览器读（不上传照片），确认后只把元数据提交上来落库
	s.mux.HandleFunc("GET /admin/photos", s.requireAdmin(s.adminPhotosPage))
	s.mux.HandleFunc("POST /admin/photos/import", s.requireAdmin(s.adminPhotosImport))
	s.mux.HandleFunc("POST /admin/edit/visit/delete", s.requireAdmin(s.editVisitDelete))
	s.mux.HandleFunc("POST /admin/edit/visit/bookmark", s.requireAdmin(s.adminVisitBookmark))
	s.mux.HandleFunc("POST /admin/edit/movement", s.requireAdmin(s.editMovementCreate))
	s.mux.HandleFunc("POST /admin/edit/movement/update", s.requireAdmin(s.editMovementUpdate))
	s.mux.HandleFunc("POST /admin/edit/movement/delete", s.requireAdmin(s.editMovementDelete))
	s.mux.HandleFunc("POST /admin/edit/weather", s.requireAdmin(s.editWeatherCreate))
	s.mux.HandleFunc("POST /admin/gpx", s.requireAdmin(s.adminGpxUpload))
	s.mux.HandleFunc("POST /admin/gpx/delete", s.requireAdmin(s.adminGpxDelete))
	s.mux.HandleFunc("POST /admin/fog", s.requireAdmin(s.adminFogUpload))
	s.mux.HandleFunc("POST /admin/fog/clear", s.requireAdmin(s.adminFogClear))
	s.mux.HandleFunc("GET /admin/fog/export", s.requireAdmin(s.adminFogExport))
	s.mux.HandleFunc("POST /admin/places/backfill", s.requireAdmin(s.adminBackfillPlaces))
	s.mux.HandleFunc("GET /admin/fog/mask", s.requireAdmin(s.fogMaskPage))
	s.mux.HandleFunc("POST /admin/fog/mask/save", s.requireAdmin(s.fogMaskSave))
	s.mux.HandleFunc("POST /admin/fog/mask/delete", s.requireAdmin(s.fogMaskDelete))
	s.mux.HandleFunc("GET /admin/export/places.geojson", s.requireAdmin(s.exportGeoJSON))
	s.mux.HandleFunc("GET /admin/export/places.gpx", s.requireAdmin(s.exportGPX))
	s.mux.HandleFunc("GET /admin/export/rondbackup", s.requireAdmin(s.exportRondbackup))
	s.mux.HandleFunc("GET /admin/settings", s.requireAdmin(s.settingsPage))
	s.mux.HandleFunc("POST /admin/settings", s.requireAdmin(s.settingsSubmit))
	s.mux.HandleFunc("GET /admin/password", s.requireAdmin(s.passwordPage))
	s.mux.HandleFunc("POST /admin/password", s.requireAdmin(s.passwordSubmit))
	s.mux.HandleFunc("POST /admin/username", s.requireAdmin(s.usernameSubmit))
}

// ---------- 中间件 ----------

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if s.cfg.Debug || strings.HasPrefix(r.URL.Path, "/admin") {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic %s %s: %v", r.Method, r.URL.Path, rec)
				http.Error(w, "服务器内部错误", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// sessionUser 从签名 cookie 还原登录态；口令变更后旧会话自动失效。
func (s *Server) sessionUser(r *http.Request) (*auth.User, error) {
	c, err := r.Cookie(s.cookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	uidStr, _, ok := strings.Cut(c.Value, "|")
	if !ok {
		return nil, nil
	}
	uid, err := strconv.ParseInt(uidStr, 10, 64)
	if err != nil {
		return nil, nil
	}
	u, err := s.auth.UserByID(r.Context(), uid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, ok := s.auth.ParseToken(c.Value, u.Hash); !ok {
		return nil, nil
	}
	return u, nil
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := userFrom(r.Context())
		if u == nil {
			http.Redirect(w, r, "/admin/login?next="+path.Clean(r.URL.Path), http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

type userKey struct{}

func userFrom(ctx context.Context) *auth.User {
	u, _ := ctx.Value(userKey{}).(*auth.User)
	return u
}

func (s *Server) login(w http.ResponseWriter, userID int64, hash string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookie,
		Value:    s.auth.SignToken(userID, hash),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   s.cfg.SessionDays * 86400,
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: s.cookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- 渲染 ----------

// render 渲染指定页面（key 见 pageFiles）。
func (s *Server) render(w http.ResponseWriter, key string, data any) {
	set, err := s.pageSet(key)
	if err != nil {
		log.Printf("加载页面模板 %s 失败: %v", key, err)
		http.Error(w, "页面不存在", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := set.t.ExecuteTemplate(w, set.entry, data); err != nil {
		log.Printf("渲染 %s 失败: %v", key, err)
	}
}

func (s *Server) renderStatus(w http.ResponseWriter, code int, key string, data any) {
	set, err := s.pageSet(key)
	if err != nil {
		http.Error(w, "页面不存在", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := set.t.ExecuteTemplate(w, set.entry, data); err != nil {
		log.Printf("渲染 %s 失败: %v", key, err)
	}
}

// pageSet 返回模板集；调试模式下每次从磁盘重新解析，改完模板刷新即可见效。
func (s *Server) pageSet(key string) (pageSet, error) {
	def, ok := pageFiles[key]
	if !ok {
		return pageSet{}, fmt.Errorf("未知页面模板 %s", key)
	}
	if !s.cfg.Debug {
		return s.pages[key], nil
	}
	files := make([]string, len(def.files))
	for i, f := range def.files {
		// 嵌入路径相对包目录，调试时按工作目录拼接
		files[i] = filepath.Join("internal", "web", f)
	}
	t, err := template.New(filepath.Base(files[0])).Funcs(templateFuncs()).ParseFiles(files...)
	if err != nil {
		return pageSet{}, err
	}
	return pageSet{t: t, entry: def.entry}, nil
}

func (s *Server) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("JSON 输出失败: %v", err)
	}
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("处理 %s 出错: %v", r.URL.Path, err)
	http.Error(w, "服务器内部错误: "+err.Error(), http.StatusInternalServerError)
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func parseInt64s(vals []string) []int64 {
	var out []int64
	for _, v := range vals {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// datasetFor 取当前生效的数据集；无数据时返回 nil。
func (s *Server) datasetFor(ctx context.Context) (*Dataset, error) {
	var d Dataset
	err := s.db.QueryRowContext(ctx, `SELECT id, original_name, uploaded_at, visit_count, place_count,
		raw_visit_count, first_visit_at, last_visit_at, size_bytes
		FROM datasets WHERE is_active AND status='done' ORDER BY uploaded_at DESC LIMIT 1`).
		Scan(&d.ID, &d.Name, &d.UploadedAt, &d.Visits, &d.Places, &d.RawVisits, &d.First, &d.Last, &d.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

type Dataset struct {
	ID         int64
	Name       string
	UploadedAt time.Time
	Visits     int
	Places     int
	RawVisits  int
	First      *time.Time
	Last       *time.Time
	Size       int64
}
