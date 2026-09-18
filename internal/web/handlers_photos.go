package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"rond-with-you/internal/geo"
)

// 照片批量导入：浏览器读 EXIF（static/js/photo-exif.js），只把坐标与时间提交上来。
//
// 照片本身默认不上传：手机相册一次几十张、几百 MB，为了几十字节的 EXIF 全传一遍
// 既慢又占服务器磁盘（早期版本就是这么做的，51 张 334MB 直接把请求撑爆）。
// 例外是行内勾了「存图」的那几张——那几张会在浏览器里先转成 WebP（长边 2000px、
// 通常几百 KB）再随表单发过来，作为地点的配图。
const (
	photoMaxRows = 300
	// photoMaxBodyBytes 是整个导入请求的上限。不勾「存图」时表单只有几十 KB；
	// 勾了之后每张图约 100~250KB（长边 1440px 的 WebP），60MB 够一次带两三百张。
	photoMaxBodyBytes = 60 << 20
	// 默认离开时间 = 到达后 10 分钟（用户可改）
	photoDefaultStay = 10 * time.Minute
)

// PhotoData 是照片导入页的数据。页面只放一个文件选择框，
// 预览表由前端脚本填，所以这里没有表格数据。
type PhotoData struct {
	Page
	Err   string
	Flash string
	// GeocodeReady 决定前端能否反查地址（没配高德 key 就只能看经纬度）
	GeocodeReady bool
	// 与「新增地点」同一套选项：类别 / 活动 / 标签
	Categories []EditOption
	Activities []EditOption
	Tags       []EditOption
}

func (s *Server) adminPhotosPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageBase(r, "admin", "照片导入")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Flatpickr = true
	p.HasMap = true
	d := PhotoData{Page: p, GeocodeReady: s.cfg.GeocodeKey != ""}
	ds, err := s.datasetFor(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if ds != nil {
		// 与数据编辑页共用同一批选项，保证两边能选的东西一致
		if d.Activities, err = s.editOptions(r.Context(), `SELECT a.id, COALESCE(a.name,'') FROM activities a
			WHERE a.dataset_id=$1 ORDER BY a.name`, ds.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
		if d.Tags, err = s.editOptions(r.Context(), `SELECT t.id, COALESCE(t.name,'') FROM tags t
			WHERE t.dataset_id=$1 ORDER BY t.name`, ds.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
		crows, err := s.db.QueryContext(r.Context(), `SELECT DISTINCT poi_category FROM places
			WHERE dataset_id=$1 AND COALESCE(poi_category,'')<>'' ORDER BY poi_category`, ds.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for crows.Next() {
			var c string
			if err := crows.Scan(&c); err != nil {
				crows.Close()
				s.serverError(w, r, err)
				return
			}
			d.Categories = append(d.Categories, EditOption{Label: c, Name: poiCategoryLabel(c)})
		}
		crows.Close()
	}
	d.Flash, d.Err = r.URL.Query().Get("ok"), r.URL.Query().Get("err")
	s.render(w, "admin/photos", d)
}

// adminPhotosImport 把前端解析好、用户确认过的行写入 places + visits。
// 坐标默认按 WGS-84 处理（EXIF 就是 WGS-84），转成站点用的 GCJ-02 再落库。
func (s *Server) adminPhotosImport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	ds, err := s.datasetFor(ctx)
	if err != nil || ds == nil {
		redirectPhotos(w, r, "err", "还没有可编辑的数据集")
		return
	}
	// 整个请求体封顶：勾了「存图」时表单是 multipart，最多可能带 300 张小图。
	// 不设上限的话，一个畸形请求就能把这台 1 核机器吃满。
	r.Body = http.MaxBytesReader(w, r.Body, photoMaxBodyBytes)
	// 必须用 ParseMultipartForm 而不是 ParseForm：ParseForm 只认 urlencoded，
	// 会把 PostForm 先填成空的那一份，后面 PostFormValue/FormFile 全是空的。
	// urlencoded 的调用方（脚本、测试）会拿到 ErrNotMultipart —— 那种情况它已经
	// 把普通表单解析好了，放过即可。
	if err := r.ParseMultipartForm(imageUploadMem); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		redirectPhotos(w, r, "err", "表单太大或读不出来（图片总量请控制在 60MB 以内）")
		return
	}
	n := atoiOr(r.PostFormValue("n"), 0)
	if n <= 0 {
		redirectPhotos(w, r, "err", "没有可导入的照片")
		return
	}
	if n > photoMaxRows {
		redirectPhotos(w, r, "err", fmt.Sprintf("一次最多导入 %d 张，当前 %d 张", photoMaxRows, n))
		return
	}
	// EXIF 标准是 WGS-84，但部分国产手机直接写 GCJ-02——由用户在页面上选择，
	// 只有明确说「已是 GCJ-02」时才不转换
	fromWGS84 := r.PostFormValue("coord") != "gcj02"
	// 这次导入用的坐标系原样留档：以后要复算、要核对，都从这一个字段读，
	// 不用再去猜「当初到底选了什么」（2026-09 那批 63 个点就是栽在没有它）
	coordSys := "wgs84"
	if !fromWGS84 {
		coordSys = "gcj02"
	}

	// 地点的编号逐条分配（见下面 nextSrcPK）；到访的主键由 insertVisit 自己取
	var imported, skipped, withImage int
	var firstErr, imageErr string
	for i := 0; i < n; i++ {
		if r.PostFormValue(fmt.Sprintf("keep_%d", i)) != "on" {
			skipped++
			continue
		}
		lat, ok1 := parseFloat(r.PostFormValue(fmt.Sprintf("lat_%d", i)))
		lon, ok2 := parseFloat(r.PostFormValue(fmt.Sprintf("lon_%d", i)))
		if !ok1 || !ok2 || (lat == 0 && lon == 0) {
			firstErr = fmt.Sprintf("第 %d 行坐标无效，已跳过", i+1)
			skipped++
			continue
		}
		// 换算前的原值先存下来：显示坐标换算错了还能反推回去，也能和原图 EXIF 直接对账
		srcLat, srcLon := lat, lon
		if fromWGS84 {
			lat, lon = geo.WGS84ToGCJ02(lat, lon)
		}
		arrival, ok := parseLocalDT(r.PostFormValue(fmt.Sprintf("arrival_%d", i)))
		if !ok {
			firstErr = fmt.Sprintf("第 %d 行到达时间无效，已跳过", i+1)
			skipped++
			continue
		}
		var departure *time.Time
		if v := strings.TrimSpace(r.PostFormValue(fmt.Sprintf("departure_%d", i))); v != "" {
			if t, ok := parseLocalDT(v); ok && !t.Before(arrival) {
				departure = &t
			}
		}
		if departure == nil {
			d := arrival.Add(photoDefaultStay)
			departure = &d
		}
		name := strings.TrimSpace(r.PostFormValue(fmt.Sprintf("name_%d", i)))
		if name == "" {
			name = fmt.Sprintf("照片地点 %d", i+1)
		}

		var placeID int64
		// 字段与「新增地点」一致：地址相关字段来自反查（也可能被用户改过），
		// 空串交给 NULLIF 存成 NULL。类别支持下拉里的「自定义…」。
		category := r.PostFormValue(fmt.Sprintf("category_%d", i))
		if category == categoryCustomValue {
			category = r.PostFormValue(fmt.Sprintf("category_custom_%d", i))
		}
		// 地点与到访一个事务：以前分两步走，写到访失败时地点会留下成为孤儿
		//（visit_count=0），导进 rond 就是个永远「无数据」的点。
		// 到访走与「新增到访」同一条路径：活动 / 备注 / 收藏 / 标签都能带上
		actID, isHome, isWork := s.resolveActivity(ctx, ds.ID, r.PostFormValue(fmt.Sprintf("activity_%d", i)))
		var placeSrc int
		if err := s.withTx(ctx, func(ex execer) error {
			// 编号按「库里 + Core Data + 留档」三处最大值取：留档里存着导入时缺必填
			// 字段的 skipped 行，那些号没有对应实体行却一直占着，只看 MAX(places) 会复用，
			// 导出时新行被当旧行、继承对方的未管理列。
			pk, err := s.nextSrcPK(ctx, ex, ds.ID, placeSeq)
			if err != nil {
				return err
			}
			if err := clearStaleRaw(ctx, ex, ds.ID, "ZLOCATION", pk); err != nil {
				return err
			}
			placeSrc = pk
			if err := ex.QueryRowContext(ctx, `INSERT INTO places
				(dataset_id, src_pk, name, poi_category, lat, lon, country_code,
				 province, city, district, thoroughfare, timezone,
				 coord_source, coord_sys, src_lat, src_lon)
				VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),$13,$14,$15,$16)
				RETURNING id`,
				ds.ID, pk, name, category,
				lat, lon, r.PostFormValue(fmt.Sprintf("country_%d", i)),
				r.PostFormValue(fmt.Sprintf("province_%d", i)),
				r.PostFormValue(fmt.Sprintf("city_%d", i)), r.PostFormValue(fmt.Sprintf("district_%d", i)),
				r.PostFormValue(fmt.Sprintf("street_%d", i)), r.PostFormValue(fmt.Sprintf("timezone_%d", i)),
				"photo", coordSys, srcLat, srcLon).Scan(&placeID); err != nil {
				return fmt.Errorf("写地点: %w", err)
			}
			if _, err := s.insertVisit(ctx, ex, ds.ID, u.ID, placeID, arrival, departure, actID, isHome, isWork,
				r.PostFormValue(fmt.Sprintf("bookmarked_%d", i)) == "on",
				strings.TrimSpace(r.PostFormValue(fmt.Sprintf("remark_%d", i))), "",
				r.PostForm[fmt.Sprintf("tags_%d", i)]); err != nil {
				return fmt.Errorf("写到访: %w", err)
			}
			return nil
		}); err != nil {
			log.Printf("照片导入：第 %d 行落库失败（已整体回滚）%v", i+1, err)
			firstErr = "部分照片落库失败，已跳过"
			skipped++
			continue
		}
		// 勾了「存图」的行，浏览器已经把这张照片转成 WebP 一起发过来了。
		// 图片存失败只记日志、不回滚地点：地点已经写好，丢一张图不该连点一起丢。
		if n, err := s.attachImportedImage(ctx, r, ds.ID, placeSrc, i); err != nil {
			log.Printf("照片导入：第 %d 行配图失败（地点已保留）: %v", i+1, err)
			imageErr = "部分照片的配图没存上（地点已导入）"
		} else if n > 0 {
			withImage += n
		}
		imported++
	}

	s.refreshDatasetAgg(ctx, ds.ID)
	msg := fmt.Sprintf("已导入 %d 个地点（照片）", imported)
	if withImage > 0 {
		msg += fmt.Sprintf("，配了 %d 张图", withImage)
	}
	if skipped > 0 {
		msg += fmt.Sprintf("，跳过 %d 张", skipped)
	}
	if firstErr != "" {
		msg += "。" + firstErr
	}
	if imageErr != "" {
		msg += "。" + imageErr
	}
	redirectPhotos(w, r, "ok", msg)
}

// attachImportedImage 收下行内附带的 WebP 并挂到刚建好的地点上。
// 浏览器已经把原图转码过了，这里只做形状校验与转存；没有这一项就返回 0。
func (s *Server) attachImportedImage(ctx context.Context, r *http.Request, datasetID int64, srcPK, row int) (int, error) {
	f, _, err := r.FormFile(fmt.Sprintf("image_%d", row))
	if err != nil {
		return 0, nil // 这一行没勾「存图」
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, imageMaxBytes+1))
	if err != nil {
		return 0, fmt.Errorf("读图失败: %w", err)
	}
	if len(data) == 0 {
		return 0, nil
	}
	if len(data) > imageMaxBytes {
		return 0, fmt.Errorf("图片超过 %dMB", imageMaxBytes>>20)
	}
	if _, _, err := s.savePlaceImage(ctx, datasetID, srcPK, data,
		atoiOr(r.PostFormValue(fmt.Sprintf("image_w_%d", row)), 0),
		atoiOr(r.PostFormValue(fmt.Sprintf("image_h_%d", row)), 0)); err != nil {
		return 0, err
	}
	return 1, nil
}

// redirectPhotos 把结果带回照片导入页。
func redirectPhotos(w http.ResponseWriter, r *http.Request, kind, msg string) {
	http.Redirect(w, r, "/admin/photos?"+kind+"="+urlQueryEscape(msg), http.StatusSeeOther)
}
