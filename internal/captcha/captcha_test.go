package captcha

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"
)

func TestIssueAndVerify(t *testing.T) {
	s := NewStore(time.Minute)
	id, pngBytes, err := s.Issue()
	if err != nil {
		t.Fatalf("出图失败: %v", err)
	}
	if id == "" || len(pngBytes) == 0 {
		t.Fatal("id 或图片为空")
	}
	code, ok := s.Answer(id)
	if !ok || len([]rune(code)) != CodeLen {
		t.Fatalf("答案不对: %q ok=%v", code, ok)
	}
	for _, r := range code {
		if !strings.ContainsRune(alphabet, r) {
			t.Errorf("答案含取码字符集之外的字符 %q", r)
		}
	}

	// 大小写与首尾空格都该容忍（用户手打容易带上）
	if !s.Verify(id, " "+strings.ToLower(code)+" ") {
		t.Error("正确验证码应通过（应容忍大小写与空格）")
	}
	// 一次性：同一张图不能再用
	if s.Verify(id, code) {
		t.Error("同一张验证码不应能被验证两次")
	}

	// 错误答案
	id2, _, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if s.Verify(id2, "XXXX") {
		t.Error("错误答案不应通过")
	}
	if s.Verify(id2, "XXXX") {
		t.Error("失败后该验证码也应作废")
	}

	// 未发出的 id
	if s.Verify("nope", "ABCD") {
		t.Error("未知 id 不应通过")
	}
}

func TestExpiry(t *testing.T) {
	s := NewStore(time.Millisecond)
	id, _, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}
	code, _ := s.Answer(id)
	time.Sleep(5 * time.Millisecond)
	if s.Verify(id, code) {
		t.Error("过期后不应通过")
	}
}

func TestPrune(t *testing.T) {
	s := NewStore(time.Millisecond)
	for i := 0; i < 5; i++ {
		if _, _, err := s.Issue(); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(5 * time.Millisecond)
	// 下一次 Issue 会顺手清掉过期的，池子不该越攒越大
	if _, _, err := s.Issue(); err != nil {
		t.Fatal(err)
	}
	if n := len(s.items); n > 1 {
		t.Errorf("过期项应被清掉，实际还剩 %d 条", n)
	}
}

// TestImageShowsAnswer 确认图里画的确实是答案：按已知点阵模板逐位回读。
// 若哪天改了绘制偏移、抖动范围或字形表却忘了同步，用户会连自己也登不进后台，
// 这条测试会立刻失败——它同时就是「图还认得出」的最低保证。
func TestImageShowsAnswer(t *testing.T) {
	s := NewStore(time.Minute)
	for n := 0; n < 8; n++ {
		id, pngBytes, err := s.Issue()
		if err != nil {
			t.Fatal(err)
		}
		want, _ := s.Answer(id)
		img, err := png.Decode(bytes.NewReader(pngBytes))
		if err != nil {
			t.Fatalf("图片无法解码: %v", err)
		}
		if b := img.Bounds(); b.Dx() != Width || b.Dy() != Height {
			t.Fatalf("图片尺寸应为 %dx%d，实际 %dx%d", Width, Height, b.Dx(), b.Dy())
		}
		if got := readCode(t, img); got != want {
			t.Fatalf("图里读出 %q，答案却是 %q", got, want)
		}
	}
}

// darkGrid 把图片预先取成「哪些像素属于字符笔画」的网格：
// 字形用 0~110 的深色，干扰线与噪点都在 120 以上。逐像素调 img.At 太慢，先摊平。
func darkGrid(img image.Image) [][]bool {
	grid := make([][]bool, Height)
	for y := 0; y < Height; y++ {
		row := make([]bool, Width)
		for x := 0; x < Width; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			row[x] = r>>8 < 120 && g>>8 < 120 && b>>8 < 120
		}
		grid[y] = row
	}
	return grid
}

// readCode 逐位回读验证码：每个字符的位置只在基准点 ±2 内抖动，
// 于是在这个小邻域里让每个字形去套，取重合度最高的那个。
func readCode(t *testing.T, img image.Image) string {
	t.Helper()
	grid := darkGrid(img)
	var out strings.Builder
	// 基准位置与 render 保持一致
	for i := 0; i < CodeLen; i++ {
		baseX := padX + i*stepX
		best, bestGlyph := -1, '?'
		for ch, g := range glyphs {
			if !strings.ContainsRune(alphabet, ch) {
				continue
			}
			for dx := -3; dx <= 3; dx++ {
				for dy := -3; dy <= 3; dy++ {
					score := 0
					for row := 0; row < glyphH; row++ {
						for c := 0; c < glyphW; c++ {
							on := g[row]&(1<<uint(glyphW-1-c)) != 0
							if !on {
								continue
							}
							// 只看笔画中心，避开边缘被抖动与噪点糊掉的部分
							x := baseX + dx + c*scale + scale/2
							y := padY + dy + row*scale + scale/2
							if x < 0 || y < 0 || x >= Width || y >= Height {
								continue
							}
							// 命中加分、误命中扣分：只看命中数的话，笔画多的字形
							// （比如 B 比 C 多一竖）光靠瞎撞也能赢
							if grid[y][x] {
								score++
							} else {
								score--
							}
						}
					}
					if score > best {
						best, bestGlyph = score, ch
					}
				}
			}
		}
		// 一个小字形最多 (5*7*scale*scale) 个像素，笔画中心数远小于它；
		// 命中太少说明这个位置没有字符，读数就不可信了
		if best < 8 {
			t.Fatalf("第 %d 个字符位置没读到笔画（最高分 %d）", i, best)
		}
		out.WriteRune(bestGlyph)
	}
	return out.String()
}
