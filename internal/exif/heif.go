// HEIF/HEIC 容器里的 EXIF 提取。
//
// HEIC 是 ISO BMFF（和 MP4 同一套盒子结构）：文件头是 ftyp，后面跟一个 meta box，
// 里面 iinf 描述每个 item（其中有一个 item_type == "Exif"），iloc 给出它的位置。
// 取出来的 Exif item 是「4 字节 TIFF 偏移 + 标准 TIFF」，后半段与 JPEG 的 APP1 完全一样，
// 所以直接交给 parseTIFF 复用。
//
// 纯 Go 实现，不引 libheif：内网/离线构建环境下 CGO 依赖太麻烦，而我们只要 EXIF 这一小块。
package exif

import (
	"encoding/binary"
	"errors"
)

var errNotHEIF = errors.New("不是 HEIF/HEIC 容器")

// boxHeader 读一个 box 的头，返回类型、负载起点与下一个 box 的起点。
func boxHeader(b []byte, off int) (typ string, payload, next int, ok bool) {
	if off < 0 || off+8 > len(b) {
		return "", 0, 0, false
	}
	size := int(binary.BigEndian.Uint32(b[off : off+4]))
	hdr := 8
	switch size {
	case 1: // largesize
		if off+16 > len(b) {
			return "", 0, 0, false
		}
		size = int(binary.BigEndian.Uint64(b[off+8 : off+16]))
		hdr = 16
	case 0: // 一直到文件末尾
		size = len(b) - off
	}
	if size < hdr || off+size > len(b) {
		return "", 0, 0, false
	}
	return string(b[off+4 : off+8]), off + hdr, off + size, true
}

// parseHEIF 从 HEIC/HEIF 里取 EXIF。
func parseHEIF(b []byte) (Info, error) {
	for off := 0; off+8 <= len(b); {
		typ, payload, next, ok := boxHeader(b, off)
		if !ok {
			break
		}
		if typ == "meta" {
			// meta 是 FullBox：负载前面还有 4 字节 version+flags
			if payload+4 > next {
				return Info{}, errNotHEIF
			}
			return parseMeta(b, payload+4, next)
		}
		if next <= off {
			break
		}
		off = next
	}
	return Info{}, errNoEXIF
}

func parseMeta(b []byte, start, end int) (Info, error) {
	iinfStart, iinfEnd := -1, -1
	ilocStart, ilocEnd := -1, -1
	for off := start; off+8 <= end; {
		typ, payload, next, ok := boxHeader(b, off)
		if !ok || next > end {
			break
		}
		switch typ {
		case "iinf":
			iinfStart, iinfEnd = payload, next
		case "iloc":
			ilocStart, ilocEnd = payload, next
		}
		off = next
	}
	if iinfStart < 0 || ilocStart < 0 {
		return Info{}, errNoEXIF
	}
	itemID, ok := findExifItem(b, iinfStart, iinfEnd)
	if !ok {
		return Info{}, errNoEXIF
	}
	off, length, ok := findItemExtent(b, ilocStart, ilocEnd, itemID)
	if !ok || off < 0 || length <= 0 || off+length > len(b) {
		return Info{}, errNoEXIF
	}
	item := b[off : off+length]
	if len(item) < 4 {
		return Info{}, errNoEXIF
	}
	// Exif item 开头 4 字节是 TIFF 头相对本 item 起点的偏移
	tiff := int(binary.BigEndian.Uint32(item[:4]))
	if tiff < 0 || tiff+8 > len(item) {
		tiff = 4 // 少数写入方这里填得不准，退化成「紧跟这 4 字节」
	}
	return parseTIFF(item[tiff:])
}

// findExifItem 在 iinf 里找 item_type 为 "Exif" 的 item。
func findExifItem(b []byte, start, end int) (uint32, bool) {
	if start+4 > end {
		return 0, false
	}
	ver := b[start]
	p := start + 4
	var count int
	if ver == 0 {
		if p+2 > end {
			return 0, false
		}
		count = int(binary.BigEndian.Uint16(b[p : p+2]))
		p += 2
	} else {
		if p+4 > end {
			return 0, false
		}
		count = int(binary.BigEndian.Uint32(b[p : p+4]))
		p += 4
	}
	for i := 0; i < count; i++ {
		typ, payload, next, ok := boxHeader(b, p)
		if !ok || next > end {
			break
		}
		// infe v2 起：version+flags(4) + item_ID(2) + protection(2) + item_type(4)
		if typ == "infe" && payload < next && b[payload] >= 2 && payload+12 <= next {
			if string(b[payload+8:payload+12]) == "Exif" {
				return uint32(binary.BigEndian.Uint16(b[payload+4 : payload+6])), true
			}
		}
		p = next
	}
	return 0, false
}

// findItemExtent 在 iloc 里找指定 item 的第一段数据（偏移 + 长度）。
func findItemExtent(b []byte, start, end int, wantID uint32) (int, int, bool) {
	if start+8 > end {
		return 0, 0, false
	}
	ver := b[start]
	p := start + 4
	offsetSize := int(b[p] >> 4)
	lengthSize := int(b[p] & 0x0F)
	p++
	baseSize := int(b[p] >> 4)
	indexSize := 0
	if ver >= 1 {
		indexSize = int(b[p] & 0x0F)
	}
	p++
	if offsetSize == 0 || lengthSize == 0 || p+2 > end {
		return 0, 0, false
	}
	count := int(binary.BigEndian.Uint16(b[p : p+2]))
	p += 2

	// 越过越界就读不动了，返回 false 让上层当作没找到
	readN := func(n int) (uint64, bool) {
		if n < 0 || p+n > end {
			return 0, false
		}
		var v uint64
		for i := 0; i < n; i++ {
			v = v<<8 | uint64(b[p+i])
		}
		p += n
		return v, true
	}

	for i := 0; i < count; i++ {
		var id uint32
		if ver == 0 {
			v, ok := readN(2)
			if !ok {
				return 0, 0, false
			}
			id = uint32(v)
		} else {
			v, ok := readN(4)
			if !ok {
				return 0, 0, false
			}
			id = uint32(v)
		}
		if ver >= 1 {
			if _, ok := readN(2); !ok { // construction_method + reserved
				return 0, 0, false
			}
		}
		if _, ok := readN(2); !ok { // data_reference_index
			return 0, 0, false
		}
		base, ok := readN(baseSize)
		if !ok {
			return 0, 0, false
		}
		extentCountV, ok := readN(2)
		if !ok {
			return 0, 0, false
		}
		for e := 0; e < int(extentCountV); e++ {
			if ver >= 1 && indexSize > 0 {
				if _, ok := readN(indexSize); !ok {
					return 0, 0, false
				}
			}
			off, ok1 := readN(offsetSize)
			length, ok2 := readN(lengthSize)
			if !ok1 || !ok2 {
				return 0, 0, false
			}
			if id == wantID {
				return int(base + off), int(length), true
			}
		}
	}
	return 0, 0, false
}
