package fog

import (
	"fmt"
	"hash/fnv"
	"math"
	"strconv"

	"rond-with-you/internal/geo"
)

// Mask 是一块「对访客隐藏」的圆形区域：中心（**GCJ-02**，与 places 同系）+ 半径（米）。
//
// 迷雾网格是 WGS-84、底图可能是 GCJ-02，所以判定前必须把中心换算到网格坐标，
// 与渲染瓦片时对 GCJ 底图做的纠偏是同一套变换——不然遮罩会整体偏出几百米。
type Mask struct {
	ID      int64
	Name    string
	Lat     float64
	Lon     float64
	RadiusM float64
}

// maskGrid 是遮罩在基网格里的预计算形式。
//
// 网格的纵向边长恒为 CellSideM、横向边长随纬度乘 cos(φ) 收缩，
// 所以「实地上的圆」在网格里是**椭圆**（横向更宽）。半径换算错这一步，
// 高纬度地区会明显偏窄。
type maskGrid struct {
	m      Mask
	gx, gy float64 // 圆心（格坐标，浮点）
	rx, ry float64 // 横向 / 纵向半径（格）
	x0, x1 int     // 包围盒（格），用于快速排除
	y0, y1 int
}

func newMaskGrid(m Mask) maskGrid {
	wlat, wlon := geo.GCJ02ToWGS84(m.Lat, m.Lon)
	gx, gy := mercX(wlon), mercY(wlat)
	cos := math.Cos(wlat * math.Pi / 180)
	if cos < 1e-6 {
		cos = 1e-6
	}
	ry := m.RadiusM / CellSideM
	rx := ry / cos
	g := maskGrid{m: m, gx: gx, gy: gy, rx: rx, ry: ry}
	g.x0 = int(math.Floor(gx-rx)) - 1
	g.x1 = int(math.Ceil(gx+rx)) + 1
	g.y0 = int(math.Floor(gy-ry)) - 1
	g.y1 = int(math.Ceil(gy+ry)) + 1
	return g
}

func (g maskGrid) hitCell(cx, cy int) bool {
	if cx < g.x0 || cx > g.x1 || cy < g.y0 || cy > g.y1 {
		return false
	}
	dx := (float64(cx) + 0.5 - g.gx) / g.rx
	dy := (float64(cy) + 0.5 - g.gy) / g.ry
	return dx*dx+dy*dy <= 1
}

// MaskSet 是一组遮罩的判定集合。零值/nil 表示不遮任何东西。
type MaskSet struct {
	grids []maskGrid
}

// NewMaskSet 预计算一组遮罩；传空切片会返回 nil。
func NewMaskSet(masks []Mask) *MaskSet {
	if len(masks) == 0 {
		return nil
	}
	s := &MaskSet{grids: make([]maskGrid, 0, len(masks))}
	for _, m := range masks {
		if m.RadiusM <= 0 {
			continue
		}
		s.grids = append(s.grids, newMaskGrid(m))
	}
	if len(s.grids) == 0 {
		return nil
	}
	return s
}

// Empty 报告集合里有没有遮罩（nil 安全）。
func (s *MaskSet) Empty() bool { return s == nil || len(s.grids) == 0 }

// Hit 报告某个全球格是否被遮罩盖住。nil 安全，未命中时只花一次包围盒比较。
func (s *MaskSet) Hit(cx, cy int) bool {
	if s == nil {
		return false
	}
	for _, g := range s.grids {
		if g.hitCell(cx, cy) {
			return true
		}
	}
	return false
}

// HitLatLon 按经纬度判定（同样传 GCJ-02），用于地点级的过滤。
func (s *MaskSet) HitLatLon(lat, lon float64) bool {
	if s == nil {
		return false
	}
	wlat, wlon := geo.GCJ02ToWGS84(lat, lon)
	return s.Hit(CellOf(wlat, wlon))
}

// maskedStats 统计被遮罩盖住的置位格数与面积（m²），用于从「已探索」的总量里扣掉。
//
// 三个要点：
//   - 逐格判一次 masks.Hit，而不是按遮罩逐个累加：遮罩可以互相重叠，按遮罩累加会把
//     重叠区域的格子算好几遍（格数扣多，和面积也会对不上）；
//   - 只遍历遮罩包围盒覆盖到的块（块坐标去重），不扫全库；
//   - 面积用 cellAreaM2，与 AreaKm2 同一套算法。
func maskedStats(blocks map[[2]int][]byte, masks *MaskSet) (cells int, areaM2 float64) {
	if masks.Empty() {
		return 0, 0
	}
	seen := make(map[[2]int]bool)
	for _, g := range masks.grids {
		for bx := g.x0 / BlockCells; bx <= g.x1/BlockCells; bx++ {
			for by := g.y0 / BlockCells; by <= g.y1/BlockCells; by++ {
				key := [2]int{bx, by}
				if seen[key] {
					continue // 两块遮罩可能盖到同一块，不能算两遍
				}
				seen[key] = true
				bm, ok := blocks[key]
				if !ok || len(bm) != bitmapSize {
					continue
				}
				n := 0
				for row := 0; row < BlockCells; row++ {
					line := bm[row*8 : row*8+8]
					for colB, v := range line {
						if v == 0 {
							continue
						}
						for bit := 0; bit < 8; bit++ {
							if v&(0x80>>bit) == 0 {
								continue
							}
							if masks.Hit(bx*BlockCells+colB*8+bit, by*BlockCells+row) {
								n++
							}
						}
					}
				}
				if n > 0 {
					cells += n
					areaM2 += float64(n) * cellAreaM2(bx, by)
				}
			}
		}
	}
	return cells, areaM2
}

// MaskedStats 在读锁内统计被遮罩盖住的部分。调用方本来拿不到块表（它不导出），
// 而走 All() 会为一次统计复制并排序整张块表（上万块），所以这里直接在索引上算。
func (ix *Index) MaskedStats(masks *MaskSet) (cells int, areaM2 float64) {
	if masks.Empty() {
		return 0, 0
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return maskedStats(ix.blocks, masks)
}

// MaskFingerprint 返回一组遮罩的短指纹，供前端把它拼进迷雾瓦片的 URL。
//
// 遮罩不在 fogIndex 里（每次请求都实时读库），所以服务端改完立刻生效；
// 但访客的瓦片是允许公共缓存的，URL 不变的话浏览器 / CDN 会继续拿缓存里那份
// （最长 1 小时）——设完遮罩看不到变化，很容易被当成「遮罩不生效」。
// 指纹变了 URL 就变，缓存自然失效。
func MaskFingerprint(masks []Mask) string {
	if len(masks) == 0 {
		return "0"
	}
	h := fnv.New64a()
	for _, m := range masks {
		fmt.Fprintf(h, "%d|%.6f|%.6f|%.1f|", m.ID, m.Lat, m.Lon, m.RadiusM)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}
