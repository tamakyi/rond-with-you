package fog

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path"
	"strings"
	"testing"
)

// TestNativeSnapshotRoundtrip 守住「真机快照解码再编码后内容不变」。
// 需要环境变量 FOG_SRC 指向真机导出的 .fwss；未设置时跳过。
//
// 重点守三件事：
//  1. Model/* 的每块逐字节一致（512 字节位图 + 尾 3 字节元信息）。尾 3 字节曾按臆造的
//     0x10/0x80 生成，实测 8701/8701 个块与真机全不符——它其实是一个大端 24 位数，
//     值为置位数*2+1；
//  2. Model/* 的条目集合与文件名不变（名字是 md5 前缀 + 掩码数字，重建时必须能原样还原）；
//  3. Model/~ 与 Model/# 两层原样带出，条目名与内容一个不差——少了它们快照就残缺 55%。
//
// 比对按「格」而不是按块体槽位：槽位序号只是头里的指针，顺序允许不同，指向同一张位图即可。
func TestNativeSnapshotRoundtrip(t *testing.T) {
	src := os.Getenv("FOG_SRC")
	if src == "" {
		t.Skip("设置 FOG_SRC 后运行迷雾快照往返检查")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取 %s: %v", src, err)
	}

	blocks, cells, err := Decode(data)
	if err != nil {
		t.Fatalf("解码: %v", err)
	}
	layers, err := RawLayers(data)
	if err != nil {
		t.Fatalf("读取其它层: %v", err)
	}
	out, err := Encode(blocks, layers)
	if err != nil {
		t.Fatalf("编码: %v", err)
	}

	orig, err := unzipAll(data)
	if err != nil {
		t.Fatalf("解开原始包: %v", err)
	}
	got, err := unzipAll(out)
	if err != nil {
		t.Fatalf("解开导出包: %v", err)
	}

	if len(orig) != len(got) {
		t.Errorf("条目数不一致: 原始 %d vs 导出 %d", len(orig), len(got))
	}
	for name := range orig {
		if _, ok := got[name]; !ok {
			t.Errorf("导出包缺少条目 %s", name)
		}
	}
	for name := range got {
		if _, ok := orig[name]; !ok {
			t.Errorf("导出包多出条目 %s", name)
		}
	}

	checked := 0
	for name, a := range orig {
		if !isModelStar(name) {
			continue
		}
		b, ok := got[name]
		if !ok {
			continue
		}
		pa, err := zlibDecompress(a)
		if err != nil {
			t.Fatalf("%s 原始载荷解压失败: %v", name, err)
		}
		pb, err := zlibDecompress(b)
		if err != nil {
			t.Fatalf("%s 导出载荷解压失败: %v", name, err)
		}
		if len(pa) != len(pb) {
			t.Errorf("%s: 载荷长度 %d vs %d", name, len(pa), len(pb))
			continue
		}
		for cell := 0; cell < headerLen; cell++ {
			ba, bb := slotAt(pa, cell), slotAt(pb, cell)
			if (ba == nil) != (bb == nil) {
				t.Errorf("%s: 格 %d 一边有块一边没有", name, cell)
				continue
			}
			if ba == nil {
				continue
			}
			if !bytes.Equal(ba[:bitmapSize], bb[:bitmapSize]) {
				t.Errorf("%s: 格 %d 的块位图不一致", name, cell)
			}
			if !bytes.Equal(ba[bitmapSize:], bb[bitmapSize:]) {
				t.Errorf("%s: 格 %d 的块元信息不一致: 原始 %x vs 导出 %x（置位数 %d）",
					name, cell, ba[bitmapSize:], bb[bitmapSize:], popcountSum(ba[:bitmapSize]))
			}
			checked++
		}
	}
	if checked == 0 {
		t.Error("没有比对到任何块，样本可能不对")
	}

	// 其它层：条目内容（zlib 流本身）逐字节一致
	for name, v := range layers {
		if !bytes.Equal(got[name], v) {
			t.Errorf("其它层 %s 未原样带出", name)
		}
	}
	t.Logf("已核对 %d 个块、%d 个其它层条目（%d 格）", checked, len(layers), cells)
}

// TestExtraBytesEncoding 钉住块尾 3 字节的编码：大端 24 位数，值 = 置位数*2+1，最高字节恒为 0。
func TestExtraBytesEncoding(t *testing.T) {
	bm := make([]byte, bitmapSize)
	for _, c := range []struct{ bit, cells int }{{0, 1}, {7, 2}, {8, 3}, {511, 4}} {
		bm[c.bit/8] |= 0x80 >> (c.bit % 8)
		if got := extraBytes(Block{Bitmap: bm}); got[0] != 0 || int(got[1])<<8|int(got[2]) != c.cells*2+1 {
			t.Errorf("置位数 %d 时得到 %x", c.cells, got)
		}
	}
	// 满块 4096 格：值 8193 = 0x2001，最高字节仍应为 0
	full := make([]byte, bitmapSize)
	for i := range full {
		full[i] = 0xFF
	}
	if got := extraBytes(Block{Bitmap: full}); !bytes.Equal(got, []byte{0x00, 0x20, 0x01}) {
		t.Errorf("满块应为 002001，实际 %x", got)
	}
}

func isModelStar(name string) bool {
	name = strings.ReplaceAll(name, "\\", "/")
	rest := strings.TrimPrefix(name, "Model/")
	return rest != name && strings.HasPrefix(rest, "*/")
}

// unzipAll 读出包内全部条目的内容——那是 zlib 流本身，要 zlibDecompress 才是载荷。
func unzipAll(data []byte) (map[string][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		out[path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))] = b
	}
	return out, nil
}

// slotAt 取出某个格子所指向的块（头里存的是 1 基槽位序号，0 表示该格没有数据）。
func slotAt(payload []byte, cell int) []byte {
	idx := int(binary.LittleEndian.Uint16(payload[2*cell:]))
	if idx == 0 {
		return nil
	}
	off := headerSize + (idx-1)*blockSize
	if off+blockSize > len(payload) {
		return nil
	}
	return payload[off : off+blockSize]
}
