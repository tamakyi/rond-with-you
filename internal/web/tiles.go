package web

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"rond-with-you/internal/config"
)

// mapLayer 描述一个可供前端使用的底图/叠加图层。
// crs 表示该底图自身的坐标系：国内底图是 gcj02，天地图与 OSM 是 wgs84。
type mapLayer struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	CRS         string `json:"crs"`
	URL         string `json:"url"`
	MaxZoom     int    `json:"maxZoom"`
	Attribution string `json:"attr"`
}

// tileSet 是当前部署的底图集合。
// upstream 记录哪些图层必须经服务端转发：命中即说明该图层带密钥，
// 密钥只在上游模板里拼接，不会出现在页面源码中。
type tileSet struct {
	base     []mapLayer
	overlays []mapLayer
	upstream map[string]string
	subStart int // 瓦片子域起始序号：高德从 1 起，天地图从 0 起
	subCount int // 瓦片子域个数：高德 4 个，天地图 8 个
}

// 高德免密钥瓦片（三种样式实测：同一块的「近黑像素占比」≈ 文字量）
//
//	style=8  街道图，带 POI 与路名标注（长沙一块 dark≈1.5%，wprd 主机更密≈3.5%）
//	style=7  纯路网，只画道路、几乎不标注（同一块 dark≈0.2%）—— 点密集时用它才看得清自己的点
//	style=6  卫星影像（走 wprd 主机）
const (
	amapStyleStreet    = "https://webrd0{s}.is.autonavi.com/appmaptile?lang=zh_cn&size=1&scale=1&style=8&x={x}&y={y}&z={z}"
	amapStyleRoadnet   = "https://webrd0{s}.is.autonavi.com/appmaptile?lang=zh_cn&size=1&scale=1&style=7&x={x}&y={y}&z={z}"
	amapStyleSatellite = "https://wprd0{s}.is.autonavi.com/appmaptile?lang=zh_cn&size=1&scale=1&style=6&x={x}&y={y}&z={z}"
)

const tdtBase = "https://t{s}.tianditu.gov.cn/%s/wmts?SERVICE=WMTS&REQUEST=GetTile&VERSION=1.0.0" +
	"&LAYER=%s&STYLE=default&TILEMATRIXSET=w&FORMAT=tiles&TILEMATRIX={z}&TILEROW={y}&TILECOL={x}&tk={key}"

func buildTileSet(cfg *config.Config) tileSet {
	ts := tileSet{upstream: map[string]string{}, overlays: []mapLayer{}, subStart: 1, subCount: 4}
	switch cfg.MapProvider {
	case "tianditu":
		ts.subStart, ts.subCount = 0, 8
		ts.upstream["tdt-vec"] = fmt.Sprintf(tdtBase, "vec_w", "vec")
		ts.upstream["tdt-img"] = fmt.Sprintf(tdtBase, "img_w", "img")
		ts.upstream["tdt-cva"] = fmt.Sprintf(tdtBase, "cva_w", "cva")
		ts.upstream["tdt-cia"] = fmt.Sprintf(tdtBase, "cia_w", "cia")
		ts.base = []mapLayer{
			{Key: "tdt-vec", Label: "矢量", CRS: "wgs84", URL: "/tiles/tdt-vec/{z}/{x}/{y}", MaxZoom: 18, Attribution: "© 天地图"},
			{Key: "tdt-img", Label: "影像", CRS: "wgs84", URL: "/tiles/tdt-img/{z}/{x}/{y}", MaxZoom: 18, Attribution: "© 天地图"},
		}
		// 天地图的注记是独立图层，缺了会看不清地名
		ts.overlays = []mapLayer{
			{Key: "tdt-cva", Label: "矢量注记", CRS: "wgs84", URL: "/tiles/tdt-cva/{z}/{x}/{y}", MaxZoom: 18},
			{Key: "tdt-cia", Label: "影像注记", CRS: "wgs84", URL: "/tiles/tdt-cia/{z}/{x}/{y}", MaxZoom: 18},
		}
	case "custom":
		key := cfg.MapTileURL
		if cfg.MapKey != "" {
			ts.upstream["custom"] = key
			ts.base = []mapLayer{{Key: "custom", Label: "底图", CRS: cfg.MapCRS, URL: "/tiles/custom/{z}/{x}/{y}", MaxZoom: 19}}
		} else {
			ts.base = []mapLayer{{Key: "custom", Label: "底图", CRS: cfg.MapCRS, URL: key, MaxZoom: 19}}
		}
	default: // amap
		// OSM 免密钥全球瓦片作为兜底：高德瓦片加载持续失败时前端会自动切过去
		osm := mapLayer{Key: "osm", Label: "OSM", CRS: "wgs84",
			URL: "https://tile.openstreetmap.org/{z}/{x}/{y}.png", MaxZoom: 19, Attribution: "© OpenStreetMap"}
		if cfg.MapKey == "" {
			// 免密钥瓦片，浏览器直连，服务端零负担
			ts.base = []mapLayer{
				{Key: "amap-street", Label: "街道", CRS: "gcj02", URL: amapStyleStreet, MaxZoom: 18, Attribution: "© 高德地图"},
				{Key: "amap-roadnet", Label: "纯路网", CRS: "gcj02", URL: amapStyleRoadnet, MaxZoom: 18, Attribution: "© 高德地图"},
				{Key: "amap-satellite", Label: "卫星影像", CRS: "gcj02", URL: amapStyleSatellite, MaxZoom: 18, Attribution: "© 高德地图"},
				osm,
			}
		} else {
			ts.upstream["amap-street"] = amapStyleStreet
			ts.upstream["amap-roadnet"] = amapStyleRoadnet
			ts.upstream["amap-satellite"] = amapStyleSatellite
			ts.base = []mapLayer{
				{Key: "amap-street", Label: "街道", CRS: "gcj02", URL: "/tiles/amap-street/{z}/{x}/{y}", MaxZoom: 18, Attribution: "© 高德地图"},
				{Key: "amap-roadnet", Label: "纯路网", CRS: "gcj02", URL: "/tiles/amap-roadnet/{z}/{x}/{y}", MaxZoom: 18, Attribution: "© 高德地图"},
				{Key: "amap-satellite", Label: "卫星影像", CRS: "gcj02", URL: "/tiles/amap-satellite/{z}/{x}/{y}", MaxZoom: 18, Attribution: "© 高德地图"},
				osm,
			}
		}
	}
	return ts
}

// ---------- 瓦片代理 ----------

var tilePathRe = regexp.MustCompile(`^/tiles/([a-z0-9-]+)/(\d{1,2})/(\d{1,8})/(\d{1,8})$`)

type cachedTile struct {
	body []byte
	ct   string
	at   time.Time
}

type tileCache struct {
	mu    sync.Mutex
	items map[string]cachedTile
	limit int
}

func newTileCache(limit int) *tileCache {
	return &tileCache{items: map[string]cachedTile{}, limit: limit}
}

func (c *tileCache) get(k string) (cachedTile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.items[k]
	return t, ok
}

// clear 清空缓存（数据变更后调用）。
func (c *tileCache) clear() {
	c.mu.Lock()
	c.items = map[string]cachedTile{}
	c.mu.Unlock()
}

func (c *tileCache) put(k string, t cachedTile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= c.limit {
		// 简单的整表淘汰：够用且不会让内存无上限增长
		c.items = map[string]cachedTile{}
	}
	c.items[k] = t
}

var tileHTTPClient = &http.Client{Timeout: 15 * time.Second}

// handleTile 代理瓦片请求。上游地址与密钥都在服务端拼接，前端只拿到本站路径。
func (s *Server) handleTile(w http.ResponseWriter, r *http.Request) {
	m := tilePathRe.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	layer := m[1]
	tpl, ok := s.tiles.upstream[layer]
	if !ok {
		http.NotFound(w, r)
		return
	}
	z, _ := strconv.Atoi(m[2])
	x, _ := strconv.Atoi(m[3])
	y, _ := strconv.Atoi(m[4])
	if z < 0 || z > 22 {
		http.NotFound(w, r)
		return
	}
	if x < 0 || y < 0 || x >= 1<<uint(z) || y >= 1<<uint(z) {
		http.NotFound(w, r)
		return
	}

	cacheKey := fmt.Sprintf("%s/%d/%d/%d", layer, z, x, y)
	if t, ok := s.tileCache.get(cacheKey); ok {
		writeTile(w, t.body, t.ct, true)
		return
	}

	up := tpl
	if s.tiles.subCount > 0 {
		// 子域按瓦片坐标轮转。写死 0 会打到不存在的域名（高德只有 01~04）
		up = strings.ReplaceAll(up, "{s}", strconv.Itoa(s.tiles.subStart+(x+y)%s.tiles.subCount))
	}
	up = strings.ReplaceAll(up, "{z}", strconv.Itoa(z))
	up = strings.ReplaceAll(up, "{x}", strconv.Itoa(x))
	up = strings.ReplaceAll(up, "{y}", strconv.Itoa(y))
	up = strings.ReplaceAll(up, "{key}", s.cfg.MapKey)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, up, nil)
	if err != nil {
		http.Error(w, "上游地址无效", http.StatusBadGateway)
		return
	}
	req.Header.Set("User-Agent", "rond-with-you/1.0")
	resp, err := tileHTTPClient.Do(req)
	if err != nil {
		http.Error(w, "拉取瓦片失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "上游返回 "+resp.Status, http.StatusBadGateway)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		http.Error(w, "读取瓦片失败", http.StatusBadGateway)
		return
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		ct = "image/png"
	}
	s.tileCache.put(cacheKey, cachedTile{body: body, ct: ct, at: time.Now()})
	writeTile(w, body, ct, false)
}

func writeTile(w http.ResponseWriter, body []byte, ct string, cached bool) {
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if cached {
		w.Header().Set("X-Tile-Cache", "hit")
	}
	w.Write(body)
}

// mapConfig 返回前端构建底图所需的全部信息。
// subs 用于浏览器直连的免密钥底图：这类 URL 里带 {s} 占位符，
// 必须由前端交给 Leaflet 展开，服务端不参与，因此子域范围得下发。
// default 是后台设的「默认铺哪张底图」（空 = 第一张）；
// 访客在图上手动切换过的话，前端会优先用他自己记在 localStorage 里的选择。
func (s *Server) mapConfig(w http.ResponseWriter, r *http.Request) {
	subs := make([]string, 0, s.tiles.subCount)
	for i := 0; i < s.tiles.subCount; i++ {
		subs = append(subs, strconv.Itoa(s.tiles.subStart+i))
	}
	def := ""
	if st, err := s.sets.Load(r.Context()); err == nil {
		def = st.MapDefaultBase
	}
	resp := map[string]any{
		"base":     s.tiles.base,
		"overlays": s.tiles.overlays,
		"subs":     subs,
		"default":  def,
	}
	if ix, err := s.fogIndex(); err == nil {
		if blocks, cells := ix.Stats(); blocks > 0 {
			// ver 里带上遮罩指纹：访客的瓦片允许公共缓存，遮罩改了 URL 必须跟着变，
			// 否则访客会一直看到缓存里那份（最长 1 小时），像「遮罩没生效」。
			ver := strconv.FormatInt(ix.Version(), 10) + "-" + s.maskFP()
			resp["fog"] = map[string]any{
				"blocks": blocks, "cells": cells, "ver": ver,
			}
		}
	}
	s.json(w, resp)
}
