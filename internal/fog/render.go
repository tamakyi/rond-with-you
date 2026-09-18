package fog

import (
	"image"
	"math"

	"rond-with-you/internal/geo"
)

// 迷雾覆盖层配色：填充用偏亮的蓝，边界再描一圈更深的轮廓，
// 这样在浅色底图上也能一眼看出「已揭开」的范围。
var (
	FogColor = [4]byte{48, 122, 224, 132}
	FogEdge  = [4]byte{16, 52, 132, 232}
)

// RenderTile 渲染一张 256x256 的迷雾透明瓦片。
//
// z/x/y 为 slippy 瓦片坐标。crs 指明底图坐标系：
// 迷雾网格是 WGS-84，而高德这类底图把 GCJ-02 坐标画在自己的名义位置上，
// 所以 gcj02 底图下每个块要先做 WGS84→GCJ02 正变换再投影；
// 块只有 600m 见方，偏移场在块内变化不足 1px，按块中心算一次即可。
// wgs84 底图（天地图/自备）则不偏移。
// masks 是被遮罩盖住、对访客不显示的区域（nil = 全部显示）。被遮住的格子既不画填充，
// 也不算作「未探索」的邻居——否则遮罩边缘会被描出一圈轮廓，等于把隐藏范围本身暴露了。
func RenderTile(blocks map[[2]int][]byte, z, x, y int, crs string, masks *MaskSet) *image.NRGBA {
	if z < 0 || z > 22 {
		return nil
	}
	step := WorldCells >> z // 该瓦片每边覆盖的格子数
	scale := 256.0 / float64(step)
	ox, oy := float64(x*step), float64(y*step)
	// 第一遍只画掩码，第二遍再按邻域决定填充色还是轮廓色：
	// 只有紧邻「未探索」的像素才描边，已探索区域内部保持平涂。
	var mask [256 * 256]bool
	// hole 记录「因为遮罩而不画」的像素，第二遍描边时把它当作正常像素跳过
	var hole []bool
	if !masks.Empty() {
		hole = make([]bool, 256*256)
	}
	mark := func(px, py int) {
		if px < 0 || px > 255 || py < 0 || py > 255 {
			return
		}
		mask[py*256+px] = true
	}
	fill := func(px, py, w, h int) {
		for r := 0; r < h; r++ {
			for c := 0; c < w; c++ {
				mark(px+c, py+r)
			}
		}
	}
	// markHole 把一段像素标成「被遮罩隐去」
	markHole := func(px, py, w, h int) {
		if hole == nil {
			return
		}
		for r := 0; r < h; r++ {
			for c := 0; c < w; c++ {
				x, y := px+c, py+r
				if x >= 0 && x <= 255 && y >= 0 && y <= 255 {
					hole[y*256+x] = true
				}
			}
		}
	}

	// 直接遍历块表而不是全球块范围：低缩放级下范围循环会膨胀到数十亿次空查询。
	// GCJ 偏移最多约 700m（74 格），粗裁剪时两侧各放宽两块保证不漏。
	const margin = 2 * BlockCells
	lo, hi := float64(x*step-margin), float64((x+1)*step+margin)
	rlo, rhi := float64(y*step-margin), float64((y+1)*step+margin)
	shift := crs == "gcj02"

	for key, bm := range blocks {
		gbx, gby := key[0], key[1]
		bx, by := float64(gbx*BlockCells), float64(gby*BlockCells)
		if bx+BlockCells <= lo || bx >= hi || by+BlockCells <= rlo || by >= rhi {
			continue
		}
		ddx, ddy := 0.0, 0.0
		if shift {
			ddx, ddy = blockShift(gbx, gby)
		}
		u0 := (bx + ddx - ox) * scale
		v0 := (by + ddy - oy) * scale

		if scale >= 1 {
			w := int(math.Ceil(scale))
			for row := 0; row < BlockCells; row++ {
				line := bm[row*8 : row*8+8]
				if line[0]|line[1]|line[2]|line[3]|line[4]|line[5]|line[6]|line[7] == 0 {
					continue
				}
				py := int(math.Floor(v0 + float64(row)*scale))
				for colB, byteVal := range line {
					if byteVal == 0 {
						continue
					}
					for bit := 0; bit < 8; bit++ {
						if byteVal&(0x80>>bit) == 0 {
							continue
						}
						px := int(math.Floor(u0 + float64(colB*8+bit)*scale))
						if masks.Hit(gbx*BlockCells+colB*8+bit, gby*BlockCells+row) {
							markHole(px, py, w, w)
							continue
						}
						fill(px, py, w, w)
					}
				}
			}
			continue
		}

		// 格子比像素还小（低缩放级）：整块只要有探索就整块涂上。
		// 这时按块中心判遮罩——缩到这种级别，块本身还不到一个像素。
		if popcountSum(bm) == 0 {
			continue
		}
		if masks.Hit(gbx*BlockCells+BlockCells/2, gby*BlockCells+BlockCells/2) {
			continue
		}
		u1 := (bx + BlockCells + ddx - ox) * scale
		v1 := (by + BlockCells + ddy - oy) * scale
		px0, py0 := int(math.Floor(u0)), int(math.Floor(v0))
		px1, py1 := int(math.Ceil(u1)), int(math.Ceil(v1))
		if px1-px0 < 1 {
			px1 = px0 + 1
		}
		if py1-py0 < 1 {
			py1 = py0 + 1
		}
		fill(px0, py0, px1-px0, py1-py0)
	}

	// 第二遍着色。瓦片外的邻格按「已探索」处理，避免在瓦片接缝处
	// 凭空描出一条网格线（真实边界由邻瓦片自己画）。
	img := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	for py := 0; py < 256; py++ {
		for px := 0; px < 256; px++ {
			if !mask[py*256+px] {
				continue
			}
			// 已探索的邻居：被遮罩隐去的也算「不算未探索」，免得把隐藏范围描出轮廓
			lit := func(i int) bool { return mask[i] || (hole != nil && hole[i]) }
			edge := false
			if px > 0 && !lit(py*256+px-1) {
				edge = true
			} else if px < 255 && !lit(py*256+px+1) {
				edge = true
			} else if py > 0 && !lit((py-1)*256+px) {
				edge = true
			} else if py < 255 && !lit((py+1)*256+px) {
				edge = true
			}
			c := FogColor
			if edge {
				c = FogEdge
			}
			i := (py*256 + px) * 4
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c[0], c[1], c[2], c[3]
		}
	}
	return img
}

// blockShift 返回该块中心在 GCJ-02 底图下的像素偏移（单位：基网格格子）。
func blockShift(gbx, gby int) (float64, float64) {
	lon, lat := cellLonLat(gbx*BlockCells+BlockCells/2, gby*BlockCells+BlockCells/2)
	glat, glon := geo.WGS84ToGCJ02(lat, lon)
	return mercX(glon) - mercX(lon), mercY(glat) - mercY(lat)
}

func mercX(lon float64) float64 { return (lon + 180.0) / 360.0 * WorldCells }

func mercY(lat float64) float64 {
	rad := lat * math.Pi / 180
	return (math.Pi - math.Asinh(math.Tan(rad))) / (2 * math.Pi) * WorldCells
}

func cellLonLat(cx, cy int) (float64, float64) {
	lon := (float64(cx)+0.5)/WorldCells*360.0 - 180.0
	n := 2 * math.Pi * (float64(cy) + 0.5) / WorldCells
	lat := math.Atan(math.Sinh(math.Pi-n)) * 180.0 / math.Pi
	return lon, lat
}
