package fog

import (
	"math"
	"testing"
)

// allOnesBlock 造一块位图全置位的块（每格都已探索）。
func allOnesBlock(gx, gy int) Block {
	bm := make([]byte, BlockCells*BlockCells/8)
	for i := range bm {
		bm[i] = 0xFF
	}
	return Block{GX: gx, GY: gy, Bitmap: bm}
}

// blocksAround 造出覆盖这些遮罩包围盒的全部块。
func blocksAround(masks ...Mask) []Block {
	var out []Block
	seen := map[[2]int]bool{}
	for _, m := range masks {
		g := newMaskGrid(m)
		for bx := g.x0 / BlockCells; bx <= g.x1/BlockCells; bx++ {
			for by := g.y0 / BlockCells; by <= g.y1/BlockCells; by++ {
				if seen[[2]int{bx, by}] {
					continue
				}
				seen[[2]int{bx, by}] = true
				out = append(out, allOnesBlock(bx, by))
			}
		}
	}
	return out
}

// TestMaskedCellsUnion 遮罩重叠时，被遮住的格子只能算一次。
// 曾经的实现是「按遮罩逐个累加命中格数」，重叠区域会被统计两三遍，
// 于是覆盖率里扣掉的格数偏大、与面积（另一套逐格算法）对不上。
func TestMaskedCellsUnion(t *testing.T) {
	a := Mask{ID: 1, Lat: 24.70, Lon: 108.03, RadiusM: 2000}
	b := Mask{ID: 2, Lat: 24.705, Lon: 108.035, RadiusM: 2000}

	onlyA, onlyB := NewMaskSet([]Mask{a}), NewMaskSet([]Mask{b})
	both := NewMaskSet([]Mask{a, b})
	if onlyA == nil || onlyB == nil || both == nil {
		t.Fatal("遮罩集合不应为空")
	}

	blocks := blocksAround(a, b)
	var want int
	for _, blk := range blocks {
		for row := 0; row < BlockCells; row++ {
			for col := 0; col < BlockCells; col++ {
				if both.Hit(blk.GX*BlockCells+col, blk.GY*BlockCells+row) {
					want++
				}
			}
		}
	}

	// 用例前提：两块遮罩真的有重叠，否则测不出重复计数
	if sum := countMasked(blocks, onlyA) + countMasked(blocks, onlyB); sum <= want {
		t.Fatalf("两块遮罩没有重叠（单独 %d + %d，并集 %d），用例无效", sum, want, want)
	}

	if got := countMasked(blocks, both); got != want {
		t.Errorf("重叠遮罩下 MaskedCells = %d，应为并集 %d（偏大即重复计数）", got, want)
	}
}

// TestMaskedAreaMatchesCells 面积与格数必须出自同一个集合：
// 扣掉的面积换算成格数后要与统计出的格数相等。
func TestMaskedAreaMatchesCells(t *testing.T) {
	a := Mask{ID: 1, Lat: 24.70, Lon: 108.03, RadiusM: 2000}
	b := Mask{ID: 2, Lat: 24.705, Lon: 108.035, RadiusM: 2000}
	set := NewMaskSet([]Mask{a, b})
	blocks := blocksAround(a, b)

	// 逐块按块中心纬度算格子面积（与 AreaKm2Excluding 内同一套算法）
	var lostM2 float64
	for _, blk := range blocks {
		_, lat := cellLonLat(blk.GX*BlockCells+BlockCells/2, blk.GY*BlockCells+BlockCells/2)
		cell := CellSideM * math.Cos(lat*math.Pi/180) * CellSideM
		n := 0
		for row := 0; row < BlockCells; row++ {
			for col := 0; col < BlockCells; col++ {
				if set.Hit(blk.GX*BlockCells+col, blk.GY*BlockCells+row) {
					n++
				}
			}
		}
		lostM2 += float64(n) * cell
	}

	byKey := map[[2]int][]byte{}
	for _, blk := range blocks {
		byKey[[2]int{blk.GX, blk.GY}] = blk.Bitmap
	}
	gotCells, gotM2 := maskedStats(byKey, set)
	if gotCells != countMasked(blocks, set) {
		t.Errorf("遮罩格数 %d，暴力统计 %d", gotCells, countMasked(blocks, set))
	}
	if diff := math.Abs(gotM2 - lostM2); diff > 1e-9 {
		t.Errorf("遮罩面积 %.6f m²，按格数算应为 %.6f m²（差 %.3f）", gotM2, lostM2, diff)
	}
}

// countMasked 暴力数一遍被遮住的格子（测试基准）。
func countMasked(blocks []Block, set *MaskSet) int {
	n := 0
	for _, blk := range blocks {
		for row := 0; row < BlockCells; row++ {
			for col := 0; col < BlockCells; col++ {
				if set.Hit(blk.GX*BlockCells+col, blk.GY*BlockCells+row) {
					n++
				}
			}
		}
	}
	return n
}

// TestMaskFingerprint 指纹要能反映遮罩的任何改动，否则访客的瓦片缓存不会失效。
func TestMaskFingerprint(t *testing.T) {
	a := Mask{ID: 1, Lat: 24.70, Lon: 108.03, RadiusM: 2000}
	b := Mask{ID: 2, Lat: 24.705, Lon: 108.035, RadiusM: 1500}

	if MaskFingerprint(nil) != "0" {
		t.Error("空遮罩的指纹应为 0")
	}
	base := MaskFingerprint([]Mask{a})
	cases := map[string][]Mask{
		"多了一块":  {a, b},
		"半径改了":  {{ID: 1, Lat: a.Lat, Lon: a.Lon, RadiusM: 3000}},
		"位置挪了":  {{ID: 1, Lat: a.Lat + 0.001, Lon: a.Lon, RadiusM: a.RadiusM}},
		"换成另一块": {b},
	}
	for name, list := range cases {
		if MaskFingerprint(list) == base {
			t.Errorf("%s 之后指纹没变，访客缓存不会失效", name)
		}
	}
}
