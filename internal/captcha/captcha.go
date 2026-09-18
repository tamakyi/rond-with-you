// Package captcha 生成登录页用的图形验证码。
//
// 刻意不引第三方库、也不依赖字体文件：字形是内置的 5×7 点阵，放大后逐个加抖动，
// 再叠干扰线与噪点。服务端只存答案（不存图片），答案一次性使用并带过期时间。
package captcha

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math/big"
	mrand "math/rand"
	"strings"
	"sync"
	"time"
)

const (
	// alphabet 是取码字符集：去掉 0/O、1/I/L 这些互相像的，省得用户反复认错。
	alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	// CodeLen 是验证码长度。
	CodeLen = 4

	glyphW, glyphH = 5, 7
	scale          = 4
	padX, padY     = 10, 6
	// stepX 要给抖动留余量：字符宽 20，抖动 ±2，步长 26 时最小间距只剩 2px，
	// 相邻字符的笔画会串在一起（人眼也一样难看）。30 保证最小间距仍有 6px。
	stepX = 30
	// Width/Height 是出图尺寸。
	Width  = padX*2 + (CodeLen-1)*stepX + glyphW*scale // 130
	Height = padY*2 + glyphH*scale                     // 40

	defaultTTL = 10 * time.Minute
	maxItems   = 4096
)

// Store 保存待验证的答案。站点是单进程部署，放内存即可；
// 重启会让已发出的验证码失效，用户重新获取一次就行。
type Store struct {
	mu    sync.Mutex
	items map[string]item
	ttl   time.Duration
}

type item struct {
	code string
	exp  time.Time
}

// NewStore 建一个验证码池；ttl <= 0 时用 10 分钟。
func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Store{items: map[string]item{}, ttl: ttl}
}

// Issue 生成一张新验证码，返回用于对答的 id 与 PNG 字节。
func (s *Store) Issue() (string, []byte, error) {
	code, err := randomCode()
	if err != nil {
		return "", nil, err
	}
	id, err := randomID()
	if err != nil {
		return "", nil, err
	}
	s.mu.Lock()
	s.pruneLocked()
	s.items[id] = item{code: code, exp: time.Now().Add(s.ttl)}
	s.mu.Unlock()

	png, err := render(code)
	if err != nil {
		return "", nil, err
	}
	return id, png, nil
}

// Verify 校验答案。无论对错都作废该 id——一次一用，避免拿同一张图反复试。
func (s *Store) Verify(id, answer string) bool {
	s.mu.Lock()
	it, ok := s.items[id]
	delete(s.items, id)
	s.mu.Unlock()
	if !ok || time.Now().After(it.exp) {
		return false
	}
	got := strings.ToUpper(strings.TrimSpace(answer))
	return got == it.code
}

// Answer 返回某个 id 当前对应的答案，不作废它。
// 只给测试与本地排查用：答案本身不会经任何接口下发到客户端。
func (s *Store) Answer(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	return it.code, ok
}

// pruneLocked 清掉过期项；仍然超过上限时随便丢一些，防止有人只取图不对答把内存撑大。
// 调用方需持有 s.mu。
func (s *Store) pruneLocked() {
	now := time.Now()
	for k, v := range s.items {
		if now.After(v.exp) {
			delete(s.items, k)
		}
	}
	for k := range s.items {
		if len(s.items) < maxItems {
			break
		}
		delete(s.items, k)
	}
}

func randomCode() (string, error) {
	var b strings.Builder
	for i := 0; i < CodeLen; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String(), nil
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// render 把验证码画成 PNG：底 → 干扰线 → 字符（各自抖动）→ 噪点。
func render(code string) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{248, 250, 251, 255}}, image.Point{}, draw.Src)

	for i := 0; i < 3; i++ {
		drawLine(img, mrand.Intn(Width), mrand.Intn(Height), mrand.Intn(Width), mrand.Intn(Height), rgb(160, 215))
	}
	for i, ch := range code {
		g, ok := glyphs[ch]
		if !ok {
			continue
		}
		// 每个字符独立抖动，位置与颜色都不同——不抖的话点阵字太规整，等于白送
		ox := padX + i*stepX + mrand.Intn(5) - 2
		oy := padY + mrand.Intn(5) - 2
		col := rgb(0, 110)
		for row := 0; row < glyphH; row++ {
			for c := 0; c < glyphW; c++ {
				if g[row]&(1<<uint(glyphW-1-c)) == 0 {
					continue
				}
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						img.Set(ox+c*scale+dx, oy+row*scale+dy, col)
					}
				}
			}
		}
	}
	// 压在最上面的噪点少一些，别把字盖住
	for i := 0; i < 60; i++ {
		img.Set(mrand.Intn(Width), mrand.Intn(Height), rgb(120, 200))
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// rgb 返回每个通道取 [lo, hi) 的随机颜色。
func rgb(lo, hi int) color.RGBA {
	f := func() uint8 { return uint8(lo + mrand.Intn(hi-lo)) }
	return color.RGBA{f(), f(), f(), 255}
}

// drawLine 是 Bresenham 画线（只用标准库，不引画图包）。
func drawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	dx, dy := abs(x1-x0), -abs(y1-y0)
	sx, sy := -1, -1
	if x0 < x1 {
		sx = 1
	}
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy
	for {
		img.Set(x0, y0, c)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// glyphs 是 5×7 点阵字形：每行低 5 位有效，最高位在最左边。
var glyphs = map[rune][glyphH]byte{
	'0': {0b01110, 0b10001, 0b10011, 0b10101, 0b11001, 0b10001, 0b01110},
	'1': {0b00100, 0b01100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	'2': {0b01110, 0b10001, 0b00001, 0b00010, 0b00100, 0b01000, 0b11111},
	'3': {0b11111, 0b00010, 0b00100, 0b00010, 0b00001, 0b10001, 0b01110},
	'4': {0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010},
	'5': {0b11111, 0b10000, 0b11110, 0b00001, 0b00001, 0b10001, 0b01110},
	'6': {0b00110, 0b01000, 0b10000, 0b11110, 0b10001, 0b10001, 0b01110},
	'7': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b01000, 0b01000},
	'8': {0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110},
	'9': {0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b00010, 0b01100},
	'A': {0b01110, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
	'B': {0b11110, 0b10001, 0b10001, 0b11110, 0b10001, 0b10001, 0b11110},
	'C': {0b01110, 0b10001, 0b10000, 0b10000, 0b10000, 0b10001, 0b01110},
	'D': {0b11110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b11110},
	'E': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b11111},
	'F': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b10000},
	'G': {0b01110, 0b10001, 0b10000, 0b10111, 0b10001, 0b10001, 0b01111},
	'H': {0b10001, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
	'I': {0b01110, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	'J': {0b00111, 0b00010, 0b00010, 0b00010, 0b00010, 0b10010, 0b01100},
	'K': {0b10001, 0b10010, 0b10100, 0b11000, 0b10100, 0b10010, 0b10001},
	'L': {0b10000, 0b10000, 0b10000, 0b10000, 0b10000, 0b10000, 0b11111},
	'M': {0b10001, 0b11011, 0b10101, 0b10101, 0b10001, 0b10001, 0b10001},
	'N': {0b10001, 0b11001, 0b10101, 0b10011, 0b10001, 0b10001, 0b10001},
	'O': {0b01110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	'P': {0b11110, 0b10001, 0b10001, 0b11110, 0b10000, 0b10000, 0b10000},
	'Q': {0b01110, 0b10001, 0b10001, 0b10001, 0b10101, 0b10010, 0b01101},
	'R': {0b11110, 0b10001, 0b10001, 0b11110, 0b10100, 0b10010, 0b10001},
	'S': {0b01111, 0b10000, 0b10000, 0b01110, 0b00001, 0b00001, 0b11110},
	'T': {0b11111, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100},
	'U': {0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	'V': {0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01010, 0b00100},
	'W': {0b10001, 0b10001, 0b10001, 0b10101, 0b10101, 0b11011, 0b10001},
	'X': {0b10001, 0b10001, 0b01010, 0b00100, 0b01010, 0b10001, 0b10001},
	'Y': {0b10001, 0b10001, 0b01010, 0b00100, 0b00100, 0b00100, 0b00100},
	'Z': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b10000, 0b11111},
}
