package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"rond-with-you/internal/geo"
)

// 按名称搜坐标：调用高德 Web 服务接口，密钥只在服务端拼接。
// 高德返回的就是 GCJ-02，与库内坐标同系，前端拿到即可直接入库，无需任何坐标转换。
const (
	amapPlaceTextURL = "https://restapi.amap.com/v3/place/text"  // 关键字/POI 搜索，适合地名
	amapGeocodeURL   = "https://restapi.amap.com/v3/geocode/geo" // 结构化地址解析，POI 无结果时兜底
)

var geocodeHTTPClient = &http.Client{Timeout: 8 * time.Second}

// geocodeItem 是返回给前端的候选点。
type geocodeItem struct {
	Name     string  `json:"name"`
	Address  string  `json:"address"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Province string  `json:"province"`
	City     string  `json:"city"`
	District string  `json:"district"`
}

// apiGeocode 供后台「新增地点」的搜索框使用，仅管理员可调用。
func (s *Server) apiGeocode(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GeocodeKey == "" {
		s.json(w, map[string]any{"error": "服务端未配置 geocode_key，暂时无法按名称搜索坐标"})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		s.json(w, map[string]any{"error": "请输入要搜索的名称"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	items, err := s.amapSearchPOI(ctx, q)
	if err != nil {
		s.json(w, map[string]any{"error": err.Error()})
		return
	}
	if len(items) == 0 {
		// 地块/门牌这类不是 POI 的地址，再走一次结构化地理编码
		items, err = s.amapGeocode(ctx, q)
		if err != nil {
			s.json(w, map[string]any{"error": err.Error()})
			return
		}
	}
	if len(items) == 0 {
		s.json(w, map[string]any{"error": "没有找到匹配的地点，换个更完整的名称试试"})
		return
	}
	s.json(w, map[string]any{"items": items})
}

func (s *Server) amapSearchPOI(ctx context.Context, q string) ([]geocodeItem, error) {
	v := url.Values{}
	v.Set("key", s.cfg.GeocodeKey)
	v.Set("keywords", q)
	v.Set("offset", "8")
	v.Set("page", "1")
	v.Set("extensions", "base")
	body, err := s.amapGet(ctx, amapPlaceTextURL+"?"+v.Encode())
	if err != nil {
		return nil, err
	}
	var raw struct {
		Status string `json:"status"`
		Info   string `json:"info"`
		Pois   []struct {
			Name     string          `json:"name"`
			Location string          `json:"location"`
			Address  json.RawMessage `json:"address"`
			Pname    json.RawMessage `json:"pname"`
			Cityname json.RawMessage `json:"cityname"`
			Adname   json.RawMessage `json:"adname"`
		} `json:"pois"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw.Status != "1" {
		return nil, &geoErr{"高德返回：" + raw.Info}
	}
	out := make([]geocodeItem, 0, len(raw.Pois))
	for _, p := range raw.Pois {
		lon, lat, ok := splitLocation(p.Location)
		if !ok {
			continue
		}
		out = append(out, geocodeItem{
			Name: p.Name, Address: amapStr(p.Address), Lat: lat, Lon: lon,
			Province: amapStr(p.Pname), City: amapStr(p.Cityname), District: amapStr(p.Adname),
		})
	}
	return out, nil
}

func (s *Server) amapGeocode(ctx context.Context, q string) ([]geocodeItem, error) {
	v := url.Values{}
	v.Set("key", s.cfg.GeocodeKey)
	v.Set("address", q)
	body, err := s.amapGet(ctx, amapGeocodeURL+"?"+v.Encode())
	if err != nil {
		return nil, err
	}
	var raw struct {
		Status   string `json:"status"`
		Info     string `json:"info"`
		Geocodes []struct {
			FormattedAddress string          `json:"formatted_address"`
			Location         string          `json:"location"`
			Province         json.RawMessage `json:"province"`
			City             json.RawMessage `json:"city"`
			District         json.RawMessage `json:"district"`
		} `json:"geocodes"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw.Status != "1" {
		return nil, &geoErr{"高德返回：" + raw.Info}
	}
	out := make([]geocodeItem, 0, len(raw.Geocodes))
	for _, g := range raw.Geocodes {
		lon, lat, ok := splitLocation(g.Location)
		if !ok {
			continue
		}
		out = append(out, geocodeItem{
			Name: g.FormattedAddress, Address: g.FormattedAddress, Lat: lat, Lon: lon,
			Province: amapStr(g.Province), City: amapStr(g.City), District: amapStr(g.District),
		})
	}
	return out, nil
}

func (s *Server) amapGet(ctx context.Context, u string) ([]byte, error) {
	// 网络抖动很常见，失败退避重试几次；限速令牌每次请求都要重新领
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 400 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		select {
		case <-amapLimiter.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "rond-with-you/1.0")
		resp, err := geocodeHTTPClient.Do(req)
		if err != nil {
			lastErr = &geoErr{"请求高德失败：" + err.Error()}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = &geoErr{"高德返回 " + resp.Status}
			continue
		}
		if readErr != nil {
			lastErr = &geoErr{"读取高德响应失败：" + readErr.Error()}
			continue
		}
		return body, nil
	}
	return nil, lastErr
}

// amapStr 处理高德字段既可能是字符串、也可能是空数组的情况（直辖市常为空数组）。
func amapStr(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		return arr[0]
	}
	return ""
}

// splitLocation 解析高德的 "经度,纬度"。
func splitLocation(v string) (lon, lat float64, ok bool) {
	a, b, found := strings.Cut(strings.TrimSpace(v), ",")
	if !found {
		return 0, 0, false
	}
	lon, err1 := strconv.ParseFloat(strings.TrimSpace(a), 64)
	lat, err2 := strconv.ParseFloat(strings.TrimSpace(b), 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return lon, lat, true
}

type geoErr struct{ msg string }

func (e *geoErr) Error() string { return e.msg }

// ---------- 逆地理编码（坐标 -> 地址），照片导入用 ----------

// regeoItem 是一个坐标反查到的地址信息。坐标是**转换后**的 GCJ-02
// （EXIF 是 WGS-84，前端把原始值发上来，这里统一转，避免两份实现）。
type regeoItem struct {
	// Key 由调用方给（照片导入里是行号）：同一坐标可能被多张照片共用，
	// 服务端做了去重，没有这个 key 就配不回原来的行。
	Key      string  `json:"key"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Address  string  `json:"address"`
	Province string  `json:"province"`
	City     string  `json:"city"`
	District string  `json:"district"`
	Street   string  `json:"street"`
	Township string  `json:"township"`
	// Poi / PoiDist 是最近的兴趣点（店名）与距离（米）——
	// 照片导入里用它判断「这个点到底在哪儿」，比只给街道好认得多
	Poi     string  `json:"poi"`
	PoiDist float64 `json:"poiDist"`
}

const regeoURL = "https://restapi.amap.com/v3/geocode/regeo"
const aroundURL = "https://restapi.amap.com/v3/place/around"

// 高德对免费 key 有 QPS 限制，一次性甩几十个请求容易被限流（返回
// CUQPS_HAS_EXCEEDED_THE_LIMIT）。所有高德请求统一在这里排队，按 10 次/秒放行。
const amapQPS = 10

// amapLimiter 是全局限速器：令牌在每次真正发请求前领取，
// 所以一个点位内部发几次请求（regeo + 周边搜索）也照样受限。
var amapLimiter = time.NewTicker(time.Second / amapQPS)

// apiRegeo 批量把坐标换成地址：照片导入时用来让用户判断「这个点对不对」。
// 只传坐标、不传照片；相同坐标会去重，避免同一地点拍多张时重复调用。
func (s *Server) apiRegeo(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GeocodeKey == "" {
		s.json(w, map[string]any{"error": "服务端未配置 geocode_key，无法反查地址"})
		return
	}
	var req struct {
		Coord  string `json:"coord"`
		Points []struct {
			Lat float64 `json:"lat"`
			Lon float64 `json:"lon"`
			Key string  `json:"key"`
		} `json:"points"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		s.json(w, map[string]any{"error": "请求格式不对"})
		return
	}
	if len(req.Points) > 200 {
		s.json(w, map[string]any{"error": "一次最多反查 200 个坐标"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// 先把坐标规整成 GCJ-02（EXIF 是 WGS-84；部分国产手机可能已经是 GCJ-02，由前端选择）
	type job struct {
		lat, lon float64
		dedup    string // 去重用
		keys     []string
	}
	jobs := make([]job, 0, len(req.Points))
	at := map[string]int{}
	for _, p := range req.Points {
		lat, lon := p.Lat, p.Lon
		if req.Coord != "gcj02" {
			lat, lon = geo.WGS84ToGCJ02(lat, lon)
		}
		// 5 位小数约 1 米：同一地点拍的多张只查一次，省配额也省时间
		k := fmt.Sprintf("%.5f,%.5f", lat, lon)
		if i, ok := at[k]; ok {
			jobs[i].keys = append(jobs[i].keys, p.Key)
			continue
		}
		at[k] = len(jobs)
		jobs = append(jobs, job{lat: lat, lon: lon, dedup: k, keys: []string{p.Key}})
	}

	// 一个去重后的点位可能对应多行，结果按行返回（key 原样带回）
	perJob := make([]regeoItem, len(jobs))
	sem := make(chan struct{}, 4) // 限并发；速率由 amapGet 里的全局限速器控制
	var wg sync.WaitGroup
	for i, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			item := regeoItem{Lat: j.lat, Lon: j.lon}
			if addr, prov, city, dist, street, town, ok := s.amapRegeo(ctx, j.lat, j.lon); ok {
				item.Address, item.Province, item.City = addr, prov, city
				item.District, item.Street, item.Township = dist, street, town
			}
			// 最近的地点（店名）：regeo 返回的 pois 多是行政单位，
			// 用周边搜索按距离排序取第一条才是店名
			if name, dist, ok := s.amapNearestPOI(ctx, j.lat, j.lon); ok {
				item.Poi, item.PoiDist = name, dist
			}
			perJob[i] = item
		}(i, j)
	}
	wg.Wait()

	items := make([]regeoItem, 0, len(req.Points))
	for i, j := range jobs {
		for _, k := range j.keys {
			it := perJob[i]
			it.Key = k
			items = append(items, it)
		}
	}
	s.json(w, map[string]any{"items": items})
}

// amapNearestPOI 找坐标附近最近的一个兴趣点（店名）。
// 半径给 200 米：再大容易匹配到马路边上不相干的店；取不到就返回 ok=false，
// 界面上只显示地址。
func (s *Server) amapNearestPOI(ctx context.Context, lat, lon float64) (name string, dist float64, ok bool) {
	v := url.Values{}
	v.Set("key", s.cfg.GeocodeKey)
	v.Set("location", fmt.Sprintf("%.6f,%.6f", lon, lat))
	v.Set("radius", "200")
	v.Set("offset", "1")
	v.Set("sortrule", "distance") // 按距离排序，第一条就是最近的
	body, err := s.amapGet(ctx, aroundURL+"?"+v.Encode())
	if err != nil {
		return "", 0, false
	}
	var raw struct {
		Status string `json:"status"`
		Pois   []struct {
			Name     string `json:"name"`
			Distance string `json:"distance"`
		} `json:"pois"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Status != "1" || len(raw.Pois) == 0 {
		return "", 0, false
	}
	d, _ := strconv.ParseFloat(raw.Pois[0].Distance, 64)
	return raw.Pois[0].Name, d, true
}

// amapRegeo 调一次高德的逆地理编码。失败返回 ok=false，调用方留空即可。
func (s *Server) amapRegeo(ctx context.Context, lat, lon float64) (addr, prov, city, dist, street, town string, ok bool) {
	v := url.Values{}
	v.Set("key", s.cfg.GeocodeKey)
	// 高德要的是「经度,纬度」
	v.Set("location", fmt.Sprintf("%.6f,%.6f", lon, lat))
	v.Set("extensions", "base")
	body, err := s.amapGet(ctx, regeoURL+"?"+v.Encode())
	if err != nil {
		return
	}
	var raw struct {
		Status    string `json:"status"`
		Regeocode struct {
			Formatted string `json:"formatted_address"`
			Comp      struct {
				Province json.RawMessage `json:"province"`
				City     json.RawMessage `json:"city"`
				District json.RawMessage `json:"district"`
				Township json.RawMessage `json:"township"`
				StreetN  struct {
					Street json.RawMessage `json:"street"`
				} `json:"streetNumber"`
			} `json:"addressComponent"`
		} `json:"regeocode"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Status != "1" {
		return "", "", "", "", "", "", false
	}
	return raw.Regeocode.Formatted,
		amapStr(raw.Regeocode.Comp.Province), amapStr(raw.Regeocode.Comp.City),
		amapStr(raw.Regeocode.Comp.District), amapStr(raw.Regeocode.Comp.StreetN.Street),
		amapStr(raw.Regeocode.Comp.Township), true
}
