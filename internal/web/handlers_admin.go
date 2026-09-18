package web

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"rond-with-you/internal/auth"
	"rond-with-you/internal/gpx"
	"rond-with-you/internal/ingest"
	"rond-with-you/internal/rond"
	"rond-with-you/internal/setting"
	"rond-with-you/internal/votes"

	"github.com/skip2/go-qrcode"
)

const maxUploadBytes = 512 << 20 // 512MB

// ---------- 登录 ----------

// captchaCookie 是验证码 id 的 cookie 名，与登录会话分开。
const captchaCookie = "rond_captcha"

// captchaImage 出一张验证码图，并把答案的 id 写进 cookie。
// 登录页的 <img src="/captcha"> 直接引用它，重新加载页面就等于换一张。
func (s *Server) captchaImage(w http.ResponseWriter, r *http.Request) {
	id, png, err := s.captcha.Issue()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: captchaCookie, Value: id, Path: "/", MaxAge: 600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.Write(png)
}

// checkCaptcha 校验提交上来的验证码；无论对错都会作废当前这一张。
func (s *Server) checkCaptcha(r *http.Request) bool {
	c, err := r.Cookie(captchaCookie)
	if err != nil {
		return false
	}
	return s.captcha.Verify(c.Value, r.PostFormValue("captcha"))
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	sets, err := s.sets.Load(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if u, _ := s.sessionUser(r); u != nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	d := LoginData{Settings: sets, Next: r.URL.Query().Get("next")}
	if r.URL.Query().Get("expired") == "1" {
		d.Flash = "请先登录后台"
	}
	s.render(w, "admin/login", d)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := r.PostFormValue("next")

	// 先过验证码：错误与过期都只提示重试，不透露别的信息。
	// 页面重新渲染时 <img> 会再取一张新图（响应带 no-store）。
	if !s.checkCaptcha(r) {
		sets, _ := s.sets.Load(ctx)
		s.renderStatus(w, http.StatusOK, "admin/login", LoginData{
			Settings: sets, Err: "验证码不正确或已过期，请重新输入", Next: next,
		})
		return
	}

	u, err := s.auth.Authenticate(ctx, username, password)
	if err != nil {
		sets, _ := s.sets.Load(ctx)
		code := http.StatusOK
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			code = http.StatusInternalServerError
		}
		msg := "用户名或密码错误"
		if code == http.StatusInternalServerError {
			msg = "登录失败：" + err.Error()
		}
		s.renderStatus(w, code, "admin/login", LoginData{Settings: sets, Err: msg, Next: next})
		return
	}
	// 已开启两步验证：口令过关只算第一步，签发短时中间令牌进入验证码页
	if _, enabled, _ := s.auth.TOTPState(ctx, u.ID); enabled {
		sets, _ := s.sets.Load(ctx)
		s.render(w, "admin/twofa-login", TwoFALoginData{
			Settings: sets, Pending: s.auth.SignPending2FA(u.ID, u.Hash), Next: next,
		})
		return
	}
	s.login(w, u.ID, u.Hash)
	target := "/admin"
	if strings.HasPrefix(next, "/admin") {
		target = next
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// ---------- 后台首页 ----------

func (s *Server) adminHome(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "admin", "后台管理")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// 上传备份、导入迷雾、开关两步验证都是「处理完跳回本页」，提示挂在 URL 上，这里得接住
	p.SetFlashFromQuery(r)
	d := AdminData{Page: p, MaxSize: maxUploadBytes}
	if p.User != nil {
		d.DefaultPW = auth.VerifyPassword(p.User.Hash, "rond-admin")
	}
	if ix, err := s.fogIndex(); err == nil {
		d.FogBlocks, d.FogCells = ix.Stats()
		if d.FogBlocks > 0 {
			d.FogArea = ix.AreaKm2()
		}
	}
	d.FogMasks = len(s.masksFor())
	var fogUpdated sql.NullTime
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT max(updated_at) FROM fog_blocks`).Scan(&fogUpdated); err != nil {
		s.serverError(w, r, err)
		return
	}
	if fogUpdated.Valid {
		t := fogUpdated.Time
		d.FogUpdatedAt = &t
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, original_name, uploaded_at, visit_count, place_count,
		raw_visit_count, first_visit_at, last_visit_at, size_bytes
		FROM datasets WHERE status='done' ORDER BY uploaded_at DESC`)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ds Dataset
		if err := rows.Scan(&ds.ID, &ds.Name, &ds.UploadedAt, &ds.Visits, &ds.Places,
			&ds.RawVisits, &ds.First, &ds.Last, &ds.Size); err != nil {
			s.serverError(w, r, err)
			return
		}
		d.Datasets = append(d.Datasets, ds)
	}
	s.render(w, "admin/index", d)
}

func (s *Server) adminUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		s.adminFlash(w, r, "err", "读取上传内容失败："+err.Error())
		return
	}
	file, hdr, err := r.FormFile("backup")
	if err != nil {
		s.adminFlash(w, r, "err", "请选择要上传的 .rondbackup 文件")
		return
	}
	defer file.Close()

	name := filepath.Base(hdr.Filename)
	if !strings.HasSuffix(strings.ToLower(name), ".rondbackup") && !strings.HasSuffix(strings.ToLower(name), ".zip") {
		s.adminFlash(w, r, "err", "文件类型不正确，请上传 rond 导出的 .rondbackup 备份")
		return
	}

	stored := filepath.Join(s.cfg.UploadDir, fmt.Sprintf("%d-%s", time.Now().Unix(), sanitize(name)))
	out, err := os.Create(stored)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		s.serverError(w, r, err)
		return
	}
	out.Close()

	sha, size, err := ingest.File(stored)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// 同一个文件重复上传：直接复用已有数据集，既不新建也不重建。
	// 顺手补一次基线库（早期版本导入的数据集还没留），这样导出 rondbackup 立刻可用。
	var oldID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM datasets WHERE user_id=$1 AND sha256=$2`,
		u.ID, sha).Scan(&oldID); err == nil && oldID > 0 {
		base := s.baselinePath(oldID)
		if _, serr := os.Stat(base); serr != nil {
			if werr := rond.WriteBaseline(stored, base); werr != nil {
				log.Printf("补写基线库失败（导出 rondbackup 时需重新上传）: %v", werr)
			}
		}
		os.Remove(stored)
		s.adminFlash(w, r, "ok", fmt.Sprintf("这个文件已经导入过（数据集 #%d），未重复导入", oldID))
		return
	}
	tmp, err := ingest.TempDir(s.cfg.DataDir, name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer os.RemoveAll(tmp)

	sqlDB, err := rond.Open(stored, tmp)
	if err != nil {
		s.adminFlash(w, r, "err", "解析备份失败："+err.Error())
		return
	}
	backup, perr := rond.Parse(sqlDB)
	sqlDB.Close()
	if perr != nil {
		s.adminFlash(w, r, "err", "读取备份数据失败："+perr.Error())
		return
	}

	datasetID, st, err := ingest.Import(ctx, s.db, u.ID, name, sha, size, backup)
	if err != nil {
		log.Printf("导入失败: %v", err)
		s.adminFlash(w, r, "err", "写入数据库失败："+err.Error())
		return
	}
	// 留一份基线库：导出 rondbackup 时以它为底、只覆盖我们管理的实体表。
	// 只在「新导入」时写——若拿导出包重传（内容与已有数据集一致），绝不能用它当基线，
	// 否则 rond 原生的标志位（ZPINSTYLE_/ZRADIUS/ZVISIT.ZRAW…）会被洗成 NULL，越导越失真。
	// 老数据集缺基线的情况，交给 ensureBaseline 按当初上传的原始包重建。
	if !st.Identical {
		if err := rond.WriteBaseline(stored, s.baselinePath(datasetID)); err != nil {
			log.Printf("写入基线库失败（导出 rondbackup 时需重新上传原始备份）: %v", err)
		}
	}

	if st.Identical {
		s.adminFlash(w, r, "ok", fmt.Sprintf(
			"这份备份的记录与已有数据集 #%d 完全一致（%d 条到访、%d 个地点），未新建快照，当前展示不变",
			datasetID, st.Visits, st.Places))
		return
	}
	msg := fmt.Sprintf("导入成功：新增 %d 条到访记录、%d 个地点、%d 条原始定位，时间跨度 %s ~ %s（数据集 #%d，已切换为当前展示）",
		st.Visits, st.Places, st.RawVisits, fmtDate(st.FirstVisit), fmtDate(st.LastVisit), datasetID)
	if st.Skipped > 0 {
		msg += fmt.Sprintf("；另有 %d 条重复记录已跳过", st.Skipped)
	}
	// 缺地址 / 没名字的地点顺手反查补全：它们既匹配不上城市黑名单，地图上还显示成「未命名」
	msg += s.backfillAfterImport(ctx, datasetID)
	// rond 里按次到访写的备注顺带回填成地点备注
	if n, err := s.notes.BackfillRond(ctx, u.ID, datasetID); err != nil {
		log.Printf("回填 rond 备注失败: %v", err)
	} else if n > 0 {
		msg += fmt.Sprintf("；已从 rond 备注回填 %d 个地点", n)
	}
	s.adminFlash(w, r, "ok", msg)
}

// adminNotesRond 手动重跑「把 rond 备份里的备注回填成地点备注」。
func (s *Server) adminNotesRond(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds, err := s.datasetFor(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if ds == nil {
		redirectNotes(w, r, "err", "还没有导入任何备份")
		return
	}
	n, err := s.notes.BackfillRond(ctx, userFrom(ctx).ID, ds.ID)
	if err != nil {
		redirectNotes(w, r, "err", "回填失败："+err.Error())
		return
	}
	redirectNotes(w, r, "ok", fmt.Sprintf("已从 rond 备注回填 %d 个地点", n))
}

func (s *Server) adminActivate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		s.adminFlash(w, r, "err", "无效的数据集编号")
		return
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE datasets SET is_active = (id = $1) WHERE user_id = $2`, id, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	// 备注是跟着「当前展示的数据集」来的，换了数据集要重算一遍
	if n, err := s.notes.BackfillRond(ctx, u.ID, id); err != nil {
		log.Printf("回填 rond 备注失败: %v", err)
	} else if n > 0 {
		s.adminFlash(w, r, "ok", fmt.Sprintf("已切换展示的数据集；已从 rond 备注回填 %d 个地点", n))
		return
	}
	s.adminFlash(w, r, "ok", "已切换展示的数据集")
}

func (s *Server) adminDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		s.adminFlash(w, r, "err", "无效的数据集编号")
		return
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM datasets WHERE id=$1 AND user_id=$2`, id, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	// 删除后若没有生效数据集，自动启用最新的一份。
	// 这一步失败必须说出来：否则站上会一份生效数据都没有，页面全空，提示却说「已删除」。
	var cnt int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM datasets WHERE user_id=$1 AND is_active`, u.ID).Scan(&cnt); err == nil && cnt == 0 {
		if _, err := s.db.ExecContext(ctx, `UPDATE datasets SET is_active=true WHERE id = (
			SELECT id FROM datasets WHERE user_id=$1 AND status='done' ORDER BY uploaded_at DESC LIMIT 1)`, u.ID); err != nil {
			log.Printf("删除数据集后自动启用最新一份失败: %v", err)
			s.adminFlash(w, r, "err", "数据集已删除，但没有找到可自动启用的备份，请重新上传一次备份")
			return
		}
	}
	s.adminFlash(w, r, "ok", "已删除该数据集及其全部记录")
}

// ---------- 账号：用户名 ----------

// usernameSubmit 改用户名。用户名是登录凭据，所以要再验一次当前口令。
func (s *Server) usernameSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("username"))
	cur := r.PostFormValue("current")
	p, err := s.pageBase(r, "admin", "账号设置")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := PasswordData{Page: p, Username: u.Username}
	fail := func(msg string) {
		d.Err = msg
		s.renderStatus(w, http.StatusOK, "admin/password", d)
	}
	if msg := validateUsername(name, u.Username); msg != "" {
		fail(msg)
		return
	}
	if err := s.auth.ChangeUsername(ctx, u.ID, cur, name); err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			fail("当前密码不正确")
		case errors.Is(err, auth.ErrUsernameTaken):
			fail("用户名「" + name + "」已被占用")
		default:
			s.serverError(w, r, err)
		}
		return
	}
	http.Redirect(w, r, "/admin/password?saved=name", http.StatusSeeOther)
}

// validateUsername 校验用户名，返回中文错误信息（空串表示通过）。
// 用户名只用于口令登录，允许中文；但空格与控制字符会让登录框里难以辨认，禁掉。
func validateUsername(name, current string) string {
	if n := utf8.RuneCountInString(name); n < 2 || n > 32 {
		return "用户名需要 2~32 个字符"
	}
	if name == current {
		return "新用户名与当前用户名相同"
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "用户名不能包含空格或不可见字符"
		}
	}
	return ""
}

// ---------- 站点设置 ----------

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "admin", "站点设置")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	layers := make([]MapLayerOption, 0, len(s.tiles.base))
	for _, l := range s.tiles.base {
		layers = append(layers, MapLayerOption{Key: l.Key, Label: l.Label})
	}
	d := SettingsData{Page: p, Modules: setting.AllModules, Pages: setting.AllPages,
		MapInfo: s.mapInfo(), MapLayers: layers, S3Info: s.s3Info()}
	d.Flash = r.URL.Query().Get("saved")
	s.render(w, "admin/settings", d)
}

// mapInfo 汇总当前生效的底图，供后台只读展示。
func (s *Server) mapInfo() string {
	names := make([]string, 0, len(s.tiles.base))
	for _, l := range s.tiles.base {
		names = append(names, l.Label)
	}
	provider := map[string]string{"amap": "高德地图", "tianditu": "天地图", "custom": "自定义图层"}[s.cfg.MapProvider]
	note := "免密钥瓦片，浏览器直连"
	if _, ok := s.tiles.upstream[s.tiles.base[0].Key]; ok {
		note = "经服务端代理，密钥不出现在页面源码中"
	}
	return fmt.Sprintf("%s（%s）· %s · %s", provider, s.cfg.MapProvider, strings.Join(names, " / "), note)
}

// s3Info 汇总对象存储的配置状态，供后台只读展示（配没配齐一眼可见）。
func (s *Server) s3Info() string {
	if !s.media.S3Ready() {
		return "未配置（s3_endpoint / s3_bucket / s3_access_key / s3_secret_key 有缺项）"
	}
	out := fmt.Sprintf("已配置：%s / %s / %s", s.cfg.S3Endpoint, s.cfg.S3Bucket, s.cfg.S3Region)
	if s.cfg.S3PublicBase == "" {
		out += "；缺 s3_public_base，浏览器取不到图片"
	} else {
		out += "；公开域名 " + s.cfg.S3PublicBase
	}
	return out
}

func (s *Server) settingsSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	cur, err := s.sets.Load(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	next := cur
	next.SiteTitle = strings.TrimSpace(firstNonEmpty(r.PostFormValue("site_title"), "我的足迹地图"))
	next.SiteSubtitle = strings.TrimSpace(r.PostFormValue("site_subtitle"))
	next.OwnerName = strings.TrimSpace(firstNonEmpty(r.PostFormValue("owner_name"), "站长"))
	next.OwnerAvatar = strings.TrimSpace(firstNonEmpty(r.PostFormValue("owner_avatar"), "🐾"))
	next.OwnerBio = strings.TrimSpace(r.PostFormValue("owner_bio"))
	next.HideHome = r.PostFormValue("hide_home") == "on"
	next.HideWork = r.PostFormValue("hide_work") == "on"
	next.NotesPublic = r.PostFormValue("notes_public") == "on"
	next.MarksPublic = r.PostFormValue("marks_public") == "on"
	next.MinDwellMin = atoiOr(r.PostFormValue("min_dwell_min"), 0)
	if next.MinDwellMin < 0 {
		next.MinDwellMin = 0
	}
	next.AccentColor = validHex(firstNonEmpty(r.PostFormValue("accent_color"), "#2f7d6e"), cur.AccentColor)
	next.VisitorHome = setting.NormalizeVisitorHome(r.PostFormValue("visitor_home"))
	// 底图只接受当前实际存在的图层 Key（换过 map_provider 后旧值就该失效），
	// 空串表示「用第一张」——让 visitor 至少能拿到一张能用的底图。
	next.MapDefaultBase = ""
	if want := strings.TrimSpace(r.PostFormValue("map_default_base")); want != "" {
		for _, l := range s.tiles.base {
			if l.Key == want {
				next.MapDefaultBase = want
				break
			}
		}
	}
	// 图片存哪边：选了对象存储但凭据没配齐时给出提示（仍照存设置，方便先配好再用）
	next.MediaBackend = setting.MediaBackendLocal
	if r.PostFormValue("media_backend") == setting.MediaBackendS3 {
		next.MediaBackend = setting.MediaBackendS3
		if !s.media.S3Ready() {
			log.Printf("设置里选了对象存储，但 conf/app.ini 的 s3_* 没配齐，图片上传会失败")
		}
	}

	mods := r.PostForm["modules"]
	next.Modules = nil
	for _, m := range setting.AllModules {
		for _, picked := range mods {
			if picked == m.Key {
				next.Modules = append(next.Modules, m.Key)
			}
		}
	}
	// 一个都不选会让主页彻底空白，这种情况按「全部开启」处理
	if len(next.Modules) == 0 {
		for _, m := range setting.AllModules {
			next.Modules = append(next.Modules, m.Key)
		}
	}
	// 页面可见性按勾选原样保存：全不选 = 访客只能看主页
	next.Pages = nil
	for _, pg := range setting.AllPages {
		for _, picked := range r.PostForm["pages"] {
			if picked == pg.Key {
				next.Pages = append(next.Pages, pg.Key)
			}
		}
	}
	// 区域列表：一行一个，省 / 市 / 区都认（如「广东省」「贵阳市」「南明区」）
	next.Regions = nil
	for _, line := range strings.Split(r.PostFormValue("regions"), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			next.Regions = append(next.Regions, v)
		}
	}
	// 只认这两个值，表单被改坏时回退到白名单（更保守的一侧）
	next.RegionsMode = setting.RegionsWhitelist
	if r.PostFormValue("regions_mode") == setting.RegionsBlacklist {
		next.RegionsMode = setting.RegionsBlacklist
	}
	if err := s.sets.Save(ctx, next); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/settings?saved=1", http.StatusSeeOther)
}

// ---------- 修改口令 ----------

func (s *Server) passwordPage(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	p, err := s.pageBase(r, "admin", "账号设置")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := PasswordData{Page: p, Username: u.Username}
	d.Flash = r.URL.Query().Get("saved")
	s.render(w, "admin/password", d)
}

func (s *Server) passwordSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	cur := r.PostFormValue("current")
	next := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm")
	p, err := s.pageBase(r, "admin", "修改密码")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := PasswordData{Page: p, Username: u.Username}
	fail := func(msg string) {
		d.Err = msg
		s.renderStatus(w, http.StatusOK, "admin/password", d)
	}
	if len(next) < 8 {
		fail("新密码至少 8 位")
		return
	}
	if next != confirm {
		fail("两次输入的新密码不一致")
		return
	}
	if err := s.auth.ChangePassword(ctx, u.ID, cur, next); err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			fail("当前密码不正确")
			return
		}
		s.serverError(w, r, err)
		return
	}
	// 口令变更后旧会话失效，需要重新登录
	http.SetCookie(w, &http.Cookie{Name: s.cookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/admin/login?expired=1", http.StatusSeeOther)
}

// ---------- 小工具 ----------

func (s *Server) adminFlash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	http.Redirect(w, r, "/admin?"+kind+"="+urlQueryEscape(msg), http.StatusSeeOther)
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range []byte(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '~':
			b.WriteByte(r)
		case r == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}

func sanitize(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			if r > 127 {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
	}
	if b.Len() == 0 {
		return "backup.rondbackup"
	}
	return b.String()
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscan(strings.TrimSpace(s), &n)
	return n, err
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func validHex(s, fallback string) string {
	if len(s) == 7 && s[0] == '#' {
		if _, err := fmt.Sscanf(s[1:], "%06x", new(uint32)); err == nil {
			return s
		}
	}
	return fallback
}

func fmtDate(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Local().Format("2006-01-02")
}

// ---------- 地点备注与别名 ----------

// ---------- 地点备注管理 ----------

// NoteRow 是备注管理页的一行：一个地点 + 它的备注 / 别名 / 结论。
type NoteRow struct {
	SrcPK      int
	Alias      string
	Content    string
	RondNote   string
	Verdict    int
	Up         int
	Down       int
	UpdatedAt  time.Time
	PlaceID    int64
	PlaceName  string
	PlaceCity  string
	PlaceGone  bool
	VisitCount int
	HasNote    bool
}

const notesPerPage = 50

// notesPage 列出当前数据集的全部地点（含未写备注的），支持筛选 / 分页 / 就地编辑；
// 另外单列「备注挂在已不在数据集的地点」这类孤儿备注，便于清理。
func (s *Server) notesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.pageBase(r, "admin", "地点备注")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	q := r.URL.Query()
	kw := strings.TrimSpace(q.Get("q"))
	status := q.Get("status")
	if status != "has" && status != "none" {
		status = "all"
	}
	city := strings.TrimSpace(q.Get("city"))
	sortBy := q.Get("sort")
	if sortBy != "note" && sortBy != "name" && sortBy != "talk" {
		sortBy = "visit"
	}

	d := NotesData{Page: p, Query: kw, Status: status, City: city, Sort: sortBy, NotesPublic: p.Settings.NotesPublic}
	d.Flash, d.Err = q.Get("ok"), q.Get("err")
	ds, err := s.datasetFor(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if ds == nil {
		s.render(w, "admin/notes", d)
		return
	}
	if d.CityOptions, err = s.q.Cities(ctx, ds.ID, nil, false); err != nil {
		s.serverError(w, r, err)
		return
	}

	where := []string{"p.dataset_id = $1"}
	args := []any{ds.ID}
	if kw != "" {
		args = append(args, "%"+kw+"%")
		n := len(args)
		where = append(where, fmt.Sprintf(
			"(COALESCE(n.alias,'') ILIKE $%d OR COALESCE(n.note,'') ILIKE $%d OR COALESCE(n.rond_note,'') ILIKE $%d OR COALESCE(p.name,'') ILIKE $%d OR COALESCE(p.city,'') ILIKE $%d)",
			n, n, n, n, n))
	}
	if city != "" {
		args = append(args, city)
		where = append(where, fmt.Sprintf("p.city = $%d", len(args)))
	}
	switch status {
	case "has":
		where = append(where, "(COALESCE(n.alias,'') <> '' OR COALESCE(n.note,'') <> '' OR COALESCE(n.rond_note,'') <> '')")
	case "none":
		where = append(where, "COALESCE(n.alias,'') = '' AND COALESCE(n.note,'') = '' AND COALESCE(n.rond_note,'') = ''")
	}

	order := "p.visit_count DESC, p.name"
	switch sortBy {
	case "name":
		order = "p.name"
	case "note":
		order = "n.updated_at DESC NULLS LAST, p.name"
	case "talk":
		order = "(COALESCE((SELECT count(*) FROM place_visitor_votes vv WHERE vv.src_pk = p.src_pk),0)) DESC, p.visit_count DESC"
	}

	base := `FROM places p
		LEFT JOIN place_notes n ON n.src_pk = p.src_pk AND n.dataset_id = p.dataset_id
		LEFT JOIN place_votes v ON v.src_pk = p.src_pk AND v.dataset_id = p.dataset_id
		WHERE ` + strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) `+base, args...).Scan(&total); err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Total = total
	d.PageCount = (total + notesPerPage - 1) / notesPerPage
	if d.PageNo > d.PageCount && d.PageCount > 0 {
		d.PageNo = d.PageCount
	}

	limitArgs := append(append([]any{}, args...), notesPerPage, (d.PageNo-1)*notesPerPage)
	q2 := `SELECT p.src_pk, COALESCE(n.alias,''), COALESCE(n.note,''), COALESCE(n.rond_note,''), COALESCE(v.verdict,0),
		COALESCE((SELECT count(*) FROM place_visitor_votes vv WHERE vv.src_pk=p.src_pk AND vv.vote=1),0),
		COALESCE((SELECT count(*) FROM place_visitor_votes vv WHERE vv.src_pk=p.src_pk AND vv.vote=-1),0),
		COALESCE(n.updated_at, to_timestamp(0)), p.id, COALESCE(p.name,''), COALESCE(p.city,''), COALESCE(p.visit_count,0)
		` + base + ` ORDER BY ` + order + fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)

	rows, err := s.db.QueryContext(ctx, q2, limitArgs...)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var n NoteRow
		if err := rows.Scan(&n.SrcPK, &n.Alias, &n.Content, &n.RondNote, &n.Verdict, &n.Up, &n.Down,
			&n.UpdatedAt, &n.PlaceID, &n.PlaceName, &n.PlaceCity, &n.VisitCount); err != nil {
			s.serverError(w, r, err)
			return
		}
		n.HasNote = n.Alias != "" || n.Content != "" || n.RondNote != ""
		d.Rows = append(d.Rows, n)
	}
	if err := rows.Err(); err != nil {
		s.serverError(w, r, err)
		return
	}

	// 孤儿备注：src_pk 已不在当前数据集
	orows, err := s.db.QueryContext(ctx, `SELECT n.src_pk, n.alias, n.note, n.rond_note, n.updated_at
		FROM place_notes n
		WHERE n.dataset_id=$1
		  AND NOT EXISTS (SELECT 1 FROM places p WHERE p.dataset_id=$1 AND p.src_pk=n.src_pk)
		ORDER BY n.updated_at DESC`, ds.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer orows.Close()
	for orows.Next() {
		var n NoteRow
		if err := orows.Scan(&n.SrcPK, &n.Alias, &n.Content, &n.RondNote, &n.UpdatedAt); err != nil {
			s.serverError(w, r, err)
			return
		}
		n.PlaceGone = true
		n.HasNote = true
		d.Orphans = append(d.Orphans, n)
	}
	if err := orows.Err(); err != nil {
		s.serverError(w, r, err)
		return
	}
	d.BackURL = d.PageLink(d.PageNo)
	s.render(w, "admin/notes", d)
}

// adminNotesBulk 对勾选的地点批量操作：删除备注 / 清空备注 / 设置或取消结论。
func (s *Server) adminNotesBulk(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	ds, err := s.datasetFor(r.Context())
	if err != nil || ds == nil {
		redirectNotes(w, r, "err", "还没有可编辑的数据集")
		return
	}
	ids := parseInt64s(r.PostForm["src_pk"])
	action := r.PostFormValue("action")
	if len(ids) == 0 {
		redirectNotes(w, r, "err", "没有勾选任何地点")
		return
	}
	var msg string
	switch action {
	case "delete":
		for _, id := range ids {
			if err := s.notes.Delete(r.Context(), u.ID, ds.ID, int(id)); err != nil {
				s.serverError(w, r, err)
				return
			}
		}
		msg = fmt.Sprintf("已删除 %d 个地点的备注与别名", len(ids))
	case "clear":
		for _, id := range ids {
			if err := s.notes.SetNote(r.Context(), u.ID, ds.ID, int(id), ""); err != nil {
				s.serverError(w, r, err)
				return
			}
		}
		msg = fmt.Sprintf("已清空 %d 个地点的备注（保留别名）", len(ids))
	case "verdict":
		verdict := atoiOr(r.PostFormValue("verdict"), 0)
		if verdict != votes.VerdictRecommend && verdict != votes.VerdictAvoid {
			verdict = 0
		}
		for _, id := range ids {
			if err := s.votes.SaveVerdict(r.Context(), u.ID, ds.ID, int(id), verdict); err != nil {
				s.serverError(w, r, err)
				return
			}
		}
		if verdict == 0 {
			msg = fmt.Sprintf("已取消 %d 个地点的结论", len(ids))
		} else {
			msg = fmt.Sprintf("已为 %d 个地点设置结论", len(ids))
		}
	default:
		redirectNotes(w, r, "err", "未知的批量操作")
		return
	}
	redirectNotes(w, r, "ok", msg)
}

// redirectNotes 回到备注管理页并带上提示（保留来源页的筛选条件）。
func redirectNotes(w http.ResponseWriter, r *http.Request, kind, msg string) {
	back := r.Referer()
	if !strings.Contains(back, "/admin/notes") {
		back = "/admin/notes"
	}
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	http.Redirect(w, r, back+sep+kind+"="+urlQueryEscape(msg), http.StatusSeeOther)
}

// adminPlaceNote 保存地点的别名与备注，按 src_pk 存（换数据集不丢）。
func (s *Server) adminPlaceNote(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	ds, err := s.datasetFor(r.Context())
	if err != nil || ds == nil {
		s.adminFlash(w, r, "err", "还没有可编辑的数据集")
		return
	}
	srcPK, err := strconv.ParseInt(r.PostFormValue("src_pk"), 10, 32)
	if err != nil {
		s.adminFlash(w, r, "err", "地点标识无效")
		return
	}
	if err := s.notes.Save(r.Context(), u.ID, ds.ID, int(srcPK),
		r.PostFormValue("alias"), r.PostFormValue("note")); err != nil {
		s.serverError(w, r, err)
		return
	}
	// 备注管理页的行内表单带 next，保存后回到原处（保留筛选）；
	// 没有 next 时按 Referer 判断，都没有就回后台首页。
	if next := r.PostFormValue("next"); strings.HasPrefix(next, "/") {
		sep := "?"
		if strings.Contains(next, "?") {
			sep = "&"
		}
		http.Redirect(w, r, next+sep+"ok="+urlQueryEscape("备注已保存"), http.StatusSeeOther)
		return
	}
	if strings.Contains(r.Referer(), "/admin/notes") {
		redirectNotes(w, r, "ok", "备注已保存")
		return
	}
	s.adminFlash(w, r, "ok", "备注已保存")
}

// ---------- 两步验证（TOTP） ----------

// login2FA 登录第二步：校验验证码后签发正式会话。
func (s *Server) login2FA(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	pending := r.PostFormValue("pending")
	code := r.PostFormValue("code")
	next := r.PostFormValue("next")

	// 中间令牌格式 "2fa|uid|exp|mac"，绑定了口令摘要，UserByID 取回摘要后才能验签
	var uid int64
	var err error
	if parts := strings.Split(pending, "|"); len(parts) == 4 {
		uid, err = parseInt64(parts[1])
	} else {
		err = errors.New("令牌格式无效")
	}
	var u *auth.User
	if err == nil {
		if u, err = s.auth.UserByID(ctx, uid); err == nil {
			if _, ok := s.auth.ParsePending2FA(pending, u.Hash); !ok {
				err = errors.New("令牌无效")
			}
		}
	}
	sets, _ := s.sets.Load(ctx)
	fail := func(msg string) {
		s.render(w, "admin/twofa-login", TwoFALoginData{
			Settings: sets, Pending: pending, Next: next, Err: msg,
		})
	}
	if err != nil {
		fail("登录会话已过期，请重新登录")
		return
	}
	secret, enabled, err := s.auth.TOTPState(ctx, u.ID)
	if err != nil || !enabled || secret == "" {
		fail("两步验证状态异常，请重新登录")
		return
	}
	if !auth.VerifyTOTP(secret, code) {
		fail("验证码不正确")
		return
	}
	s.login(w, u.ID, u.Hash)
	target := "/admin"
	if strings.HasPrefix(next, "/admin") {
		target = next
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// twofaPage 管理两步验证：展示状态、扫码绑定的二维码与确认 / 关闭表单。
func (s *Server) twofaPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "admin", "两步验证")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.SetFlashFromQuery(r)
	d := TwoFAData{Page: p}
	u := userFrom(r.Context())
	secret, enabled, err := s.auth.TOTPState(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Enabled = enabled
	if !enabled && secret != "" {
		// 已生成待确认：展示二维码与秘钥，等管理员输入验证码确认
		d.Pending = true
		d.Secret = secret
		url := auth.OTPAuthURL(u.Username, secret)
		if qr, err := qrcode.New(url, qrcode.Medium); err == nil {
			if png, err := qr.PNG(220); err == nil {
				d.QR = base64.StdEncoding.EncodeToString(png)
			}
		}
	}
	s.render(w, "admin/twofa", d)
}

func (s *Server) twofaSetup(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if _, enabled, _ := s.auth.TOTPState(r.Context(), u.ID); enabled {
		s.adminFlash(w, r, "err", "两步验证已开启，先关闭才能重新绑定")
		return
	}
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.auth.SetTOTPSecret(r.Context(), u.ID, secret); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/twofa", http.StatusSeeOther)
}

func (s *Server) twofaConfirm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	secret, enabled, err := s.auth.TOTPState(ctx, u.ID)
	if err != nil || enabled || secret == "" {
		s.adminFlash(w, r, "err", "当前没有待确认的两步验证绑定")
		return
	}
	if !auth.VerifyTOTP(secret, r.PostFormValue("code")) {
		s.adminFlash(w, r, "err", "验证码不正确，请确认验证器时间后重试")
		return
	}
	if err := s.auth.EnableTOTP(ctx, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.adminFlash(w, r, "ok", "两步验证已开启，下次登录需要输入验证码")
}

func (s *Server) twofaDisable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	secret, enabled, err := s.auth.TOTPState(ctx, u.ID)
	if err != nil || !enabled || secret == "" {
		s.adminFlash(w, r, "err", "两步验证尚未开启")
		return
	}
	// 关闭也要求出具新鲜验证码，防止被劫持的会话直接拆掉 2FA
	if !auth.VerifyTOTP(secret, r.PostFormValue("code")) {
		s.adminFlash(w, r, "err", "验证码不正确，未关闭")
		return
	}
	if err := s.auth.DisableTOTP(ctx, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.adminFlash(w, r, "ok", "两步验证已关闭")
}

// ---------- GPX 轨迹 ----------

const maxGpxBytes = 32 << 20

// adminGpxUpload 导入 GPX：rond 只记录地点之间的直线位移，
// 导入真实路径后可在地图上叠加显示。
func (s *Server) adminGpxUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	r.Body = http.MaxBytesReader(w, r.Body, maxGpxBytes+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		s.adminFlash(w, r, "err", "读取上传内容失败："+err.Error())
		return
	}
	file, hdr, err := r.FormFile("gpx")
	if err != nil {
		s.adminFlash(w, r, "err", "请选择要上传的 .gpx 文件")
		return
	}
	defer file.Close()
	name := strings.ToLower(filepath.Base(hdr.Filename))
	if !strings.HasSuffix(name, ".gpx") && !strings.HasSuffix(name, ".xml") {
		s.adminFlash(w, r, "err", "文件类型不正确，请上传 .gpx 轨迹文件")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, maxGpxBytes+1))
	if err != nil {
		s.adminFlash(w, r, "err", "读取文件失败："+err.Error())
		return
	}
	tracks, err := gpx.Parse(data)
	if err != nil {
		s.adminFlash(w, r, "err", err.Error())
		return
	}
	base := strings.TrimSuffix(filepath.Base(hdr.Filename), filepath.Ext(hdr.Filename))
	total := 0
	for _, t := range tracks {
		pts := make([][2]float64, 0, len(t.Points))
		for _, p := range t.Points {
			pts = append(pts, [2]float64{round6(p.Lat), round6(p.Lon)})
		}
		raw, err := json.Marshal(pts)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		label := t.Name
		if label == "" {
			label = base
		}
		var started any
		if !t.Time.IsZero() {
			started = t.Time
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO gpx_tracks
			(user_id, name, point_count, length_km, started_at, points)
			VALUES ($1,$2,$3,$4,$5,$6)`, u.ID, label, len(pts), t.LengthKm(), started, raw); err != nil {
			s.serverError(w, r, err)
			return
		}
		total += len(pts)
	}
	s.adminFlash(w, r, "ok", fmt.Sprintf("GPX 导入成功：%d 条轨迹 / %d 个点", len(tracks), total))
}

func (s *Server) adminGpxDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		s.adminFlash(w, r, "err", "轨迹编号无效")
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM gpx_tracks WHERE id=$1`, id); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.adminFlash(w, r, "ok", "轨迹已删除")
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
