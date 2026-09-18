// Package fog 解析与生成世界迷雾（Fog of World）的快照数据。
//
// 快照是一个 zip，其中 Model/*/ 下的每个文件是一块「瓦片」：全球 512x512 块瓦片，
// 每块瓦片 128x128 个块，每块是 64x64 的位图，一个位对应全球 2^22 x 2^22 网格里
// 约 9.55m*cos(纬度) 宽的一格。网格走网页墨卡托，经纬度即 WGS-84（已实测确认）。
// 文件内容 zlib 压缩：头 32768 字节是 16384 个 uint16（LE），值 = 该块在块体里的
// 序号（1 基，0 表示无数据）；块体每块 512 字节位图 + 3 字节附加（地区码与置位校验）。
package fog

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"
)

const (
	// MapTiles 全球瓦片数（每边）。
	MapTiles = 512
	// TileBlocks 每块瓦片的块数（每边）。
	TileBlocks = 128
	// BlockCells 每块的格子数（每边）。
	BlockCells = 64
	// WorldCells 全球格子数（每边），2^22。
	WorldCells = MapTiles * TileBlocks * BlockCells

	bitmapSize = BlockCells * BlockCells / 8 // 512
	extraSize  = 3
	blockSize  = bitmapSize + extraSize // 515
	headerLen  = TileBlocks * TileBlocks
	headerSize = headerLen * 2 // 32768

	mask1 = "olhwjsktri"
	mask2 = "eizxdwknmo"
)

// Block 是一片 64x64 的已探索位图。GX/GY 为全局块坐标（0..65535），
// Bitmap 固定 512 字节，行优先、每行 8 字节、字节内高位在前。
type Block struct {
	GX, GY int
	Bitmap []byte
}

// CellCount 返回位图里的置位数（已探索格子数）。
func (b *Block) CellCount() int {
	n := 0
	for _, v := range b.Bitmap {
		n += int(popcount[v])
	}
	return n
}

var popcount = func() [256]byte {
	var t [256]byte
	for i := range t {
		t[i] = byte(strings.Count(strconv.FormatUint(uint64(i), 2), "1"))
	}
	return t
}()

// tileIDFromName 从文件名还原瓦片编号，并校验 md5 前缀与 M2 尾缀。
func tileIDFromName(name string) (int, bool) {
	if len(name) < 7 || len(name[4:]) < 3 {
		return 0, false
	}
	digits := make([]byte, 0, len(name))
	for _, c := range name[4 : len(name)-2] {
		v := strings.IndexRune(mask1, c)
		if v < 0 {
			return 0, false
		}
		digits = append(digits, byte('0'+v))
	}
	id, err := strconv.Atoi(string(digits))
	if err != nil {
		return 0, false
	}
	s := strconv.Itoa(id)
	if fmt.Sprintf("%x", md5.Sum([]byte(s)))[:4] != name[:4] {
		return 0, false
	}
	tail := name[len(name)-2:]
	for i := 0; i < 2; i++ {
		if int(s[len(s)-2+i]-'0') != strings.IndexByte(mask2, tail[i]) {
			return 0, false
		}
	}
	return id, true
}

// tileName 把瓦片编号编码回文件名。
func tileName(id int) string {
	s := strconv.Itoa(id)
	sum := fmt.Sprintf("%x", md5.Sum([]byte(s)))
	var b strings.Builder
	b.WriteString(sum[:4])
	for i := 0; i < len(s); i++ {
		b.WriteByte(mask1[s[i]-'0'])
	}
	for i := len(s) - 2; i < len(s); i++ {
		b.WriteByte(mask2[s[i]-'0'])
	}
	return b.String()
}

// Decode 读取 .fwss（或任何包含迷雾瓦片文件的 zip），返回去重后的块与格子总数。
// 兼容 Model/*/、Sync/、根目录等布局：凡文件名能通过瓦片校验的都算。
func Decode(data []byte) ([]Block, int, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, 0, fmt.Errorf("打开迷雾快照: %w", err)
	}
	merged := map[[2]int][]byte{}
	cells := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		id, ok := tileIDFromName(path.Base(f.Name))
		if !ok {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, 0, fmt.Errorf("读取 %s: %w", f.Name, err)
		}
		raw, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("读取 %s: %w", f.Name, err)
		}
		zdata, err := zlibDecompress(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("解压 %s: %w", f.Name, err)
		}
		if len(zdata) < headerSize {
			continue
		}
		tx, ty := id%MapTiles, id/MapTiles
		for i := 0; i < headerLen; i++ {
			idx := int(binary.LittleEndian.Uint16(zdata[2*i:]))
			if idx == 0 {
				continue
			}
			off := headerSize + (idx-1)*blockSize
			if off+blockSize > len(zdata) {
				return nil, 0, fmt.Errorf("%s: 块体越界", f.Name)
			}
			bm := make([]byte, bitmapSize)
			copy(bm, zdata[off:off+bitmapSize])
			key := [2]int{tx*TileBlocks + i%TileBlocks, ty*TileBlocks + i/TileBlocks}
			if old, dup := merged[key]; dup {
				for j := range bm {
					old[j] |= bm[j]
				}
				continue
			}
			merged[key] = bm
			cells += popcountSum(bm)
		}
	}
	blocks := make([]Block, 0, len(merged))
	for k, bm := range merged {
		blocks = append(blocks, Block{GX: k[0], GY: k[1], Bitmap: bm})
	}
	sortBlocks(blocks)
	return blocks, cells, nil
}

func popcountSum(bm []byte) int {
	n := 0
	for _, v := range bm {
		n += int(popcount[v])
	}
	return n
}

func sortBlocks(bs []Block) {
	for i := 1; i < len(bs); i++ {
		for j := i; j > 0 && (bs[j-1].GY > bs[j].GY || (bs[j-1].GY == bs[j].GY && bs[j-1].GX > bs[j].GX)); j-- {
			bs[j-1], bs[j] = bs[j], bs[j-1]
		}
	}
}

func zlibDecompress(raw []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// Encode 把块集还原成 .fwss 结构（Model/*/ 由块重建），可用于导出与再导入。
// layers 是留存的非已探索层原始文件（Model/~ 与 Model/#），原样写回——
// 它们不参与渲染，但少了它们 fog of world 那边看到的就是残缺快照。
func Encode(blocks []Block, layers map[string][]byte) ([]byte, error) {
	byTile := map[[2]int][]Block{}
	for _, b := range blocks {
		tx, ty := b.GX/TileBlocks, b.GY/TileBlocks
		byTile[[2]int{tx, ty}] = append(byTile[[2]int{tx, ty}], b)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for t, list := range byTile {
		sortByLinear(list)
		header := make([]byte, headerSize)
		body := make([]byte, 0, len(list)*blockSize)
		for k, b := range list {
			i := (b.GY%TileBlocks)*TileBlocks + b.GX%TileBlocks
			binary.LittleEndian.PutUint16(header[2*i:], uint16(k+1))
			body = append(body, b.Bitmap...)
			body = append(body, extraBytes(b)...)
		}
		var raw bytes.Buffer
		zc := zlib.NewWriter(&raw)
		if _, err := zc.Write(header); err != nil {
			return nil, err
		}
		if _, err := zc.Write(body); err != nil {
			return nil, err
		}
		if err := zc.Close(); err != nil {
			return nil, err
		}
		id := t[0] + t[1]*MapTiles
		name := "Model/*/" + tileName(id)
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(raw.Bytes()); err != nil {
			return nil, err
		}
	}
	// 其它层直接写原始字节，不做二次压缩，免得多一层格式风险
	for p, data := range layers {
		w, err := zw.Create(p)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sortByLinear(list []Block) {
	lin := func(b Block) int { return (b.GY%TileBlocks)*TileBlocks + b.GX%TileBlocks }
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && lin(list[j-1]) > lin(list[j]); j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}

// extraBytes 生成块尾那 3 字节。真机存的是一个**大端 24 位数**，值为 置位数*2+1：
// 实测真机包 8701/8701 个块精确满足（如 118 格 -> 0x0000ED = 237 = 118*2+1）。
// 一个 64x64 块最多 4096 格，该值 <= 0x2001，所以最高字节在数学上恒为 0。
// （旧实现写的是 0x10 / 0x80|…，纯属臆造，会让导出包里每个块的元信息都与真机对不上。）
func extraBytes(b Block) []byte {
	sum := b.CellCount()*2 + 1
	return []byte{0x00, byte(sum >> 8), byte(sum & 0xFF)}
}

// CellSideM 是格子在赤道的边长（米）。宽随纬度 cos 收缩，高恒定（网页墨卡托）。
const CellSideM = 9.55462861

// CellOf 把 WGS-84 经纬度投到全球格坐标（网页墨卡托，2^22 网格）。
func CellOf(lat, lon float64) (cx, cy int) {
	cx = int((lon + 180.0) / 360.0 * WorldCells)
	rad := lat * math.Pi / 180
	cy = int((math.Pi - math.Asinh(math.Tan(rad))) / (2 * math.Pi) * WorldCells)
	return
}

// AreaKm2 估算块集覆盖的实地面积：每格面积按块中心纬度做 cos 修正。
func AreaKm2(blocks []Block) float64 {
	var m2 float64
	for _, b := range blocks {
		m2 += float64(b.CellCount()) * cellAreaM2(b.GX, b.GY)
	}
	return m2 / 1e6
}

// cellAreaM2 是某块内单格的面积（m²）：一格宽 CellSideM、随纬度乘 cos 收缩，高恒定。
func cellAreaM2(gx, gy int) float64 {
	_, lat := cellLonLat(gx*BlockCells+BlockCells/2, gy*BlockCells+BlockCells/2)
	return CellSideM * math.Cos(lat*math.Pi/180) * CellSideM
}
