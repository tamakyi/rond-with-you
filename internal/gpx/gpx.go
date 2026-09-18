// Package gpx 解析 GPX 轨迹文件（标准 WGS-84 坐标），
// 用来给 rond 的「地点到地点」直线位移补上真实路径。
package gpx

import (
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Track 是一条轨迹（GPX 的 trk / rte），坐标是 WGS-84。
type Track struct {
	Name   string
	Points []Point
	Time   time.Time
}

type Point struct {
	Lat float64
	Lon float64
	Ele float64
	T   time.Time
}

type gpxDoc struct {
	XMLName xml.Name `xml:"gpx"`
	Name    string   `xml:"metadata>name"`
	Trks    []trk    `xml:"trk"`
	Rtes    []rte    `xml:"rte"`
}

type trk struct {
	Name   string   `xml:"name"`
	Trkseg []trkseg `xml:"trkseg"`
}

type trkseg struct {
	Pts []wpt `xml:"trkpt"`
}

type rte struct {
	Name string `xml:"name"`
	Pts  []wpt  `xml:"rtept"`
}

type wpt struct {
	Lat float64 `xml:"lat,attr"`
	Lon float64 `xml:"lon,attr"`
	Ele float64 `xml:"ele"`
	T   string  `xml:"time"`
}

// Parse 解析 GPX 文本，返回其中的全部轨迹。trk 优先，没有则退回 rte。
func Parse(data []byte) ([]Track, error) {
	var doc gpxDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("GPX 解析失败: %w", err)
	}
	var out []Track
	base := strings.TrimSpace(doc.Name)
	for _, t := range doc.Trks {
		tr := Track{Name: firstNonEmpty(strings.TrimSpace(t.Name), base)}
		for _, seg := range t.Trkseg {
			for _, p := range seg.Pts {
				if p.Lat == 0 && p.Lon == 0 {
					continue
				}
				pt := Point{Lat: p.Lat, Lon: p.Lon, Ele: p.Ele}
				if p.T != "" {
					pt.T, _ = time.Parse(time.RFC3339, strings.TrimSpace(p.T))
				}
				tr.Points = append(tr.Points, pt)
			}
		}
		if len(tr.Points) > 1 {
			if tr.Time.IsZero() {
				tr.Time = tr.Points[0].T
			}
			out = append(out, tr)
		}
	}
	for _, r := range doc.Rtes {
		tr := Track{Name: firstNonEmpty(strings.TrimSpace(r.Name), base)}
		for _, p := range r.Pts {
			if p.Lat == 0 && p.Lon == 0 {
				continue
			}
			pt := Point{Lat: p.Lat, Lon: p.Lon}
			if p.T != "" {
				pt.T, _ = time.Parse(time.RFC3339, strings.TrimSpace(p.T))
			}
			tr.Points = append(tr.Points, pt)
		}
		if len(tr.Points) > 1 {
			if tr.Time.IsZero() {
				tr.Time = tr.Points[0].T
			}
			out = append(out, tr)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("文件里没有找到轨迹点（需要 trk/trkpt 或 rte/rtept）")
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// LengthKm 计算轨迹里程（Haversine）。
func (t Track) LengthKm() float64 {
	sum := 0.0
	for i := 1; i < len(t.Points); i++ {
		sum += haversine(t.Points[i-1], t.Points[i])
	}
	return sum
}

// BBox 返回 [minLon, minLat, maxLon, maxLat]。
func (t Track) BBox() [4]float64 {
	b := [4]float64{180, 90, -180, -90}
	for _, p := range t.Points {
		if p.Lon < b[0] {
			b[0] = p.Lon
		}
		if p.Lat < b[1] {
			b[1] = p.Lat
		}
		if p.Lon > b[2] {
			b[2] = p.Lon
		}
		if p.Lat > b[3] {
			b[3] = p.Lat
		}
	}
	return b
}

func haversine(a, b Point) float64 {
	const r = 6371.0
	dLat := (b.Lat - a.Lat) * math.Pi / 180
	dLon := (b.Lon - a.Lon) * math.Pi / 180
	la1 := a.Lat * math.Pi / 180
	la2 := b.Lat * math.Pi / 180
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(la1)*math.Cos(la2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(h))
}
