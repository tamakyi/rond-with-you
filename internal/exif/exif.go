// Package exif 从 JPEG 里读出拍摄时间与 GPS 坐标（照片导入用）。
//
// 只做两件事：EXIF 的 DateTimeOriginal 和 GPS IFD，其余一概不碰。
// 之所以自己解析而不引第三方库：站点是内网/离线构建，少一个依赖少一份麻烦；
// JPEG 的 EXIF 就在紧跟在 SOI 之后的 APP1 段里，格式固定，值得手写。
//
// 注意 EXIF 里的 GPS 是 WGS-84，而 rond / 站点内部是 GCJ-02，
// 调用方拿到坐标后要自己转（见 internal/geo.WGS84ToGCJ02）。
package exif

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Info 是从一张照片里读到的有用信息。
type Info struct {
	Lat, Lon float64
	HasGPS   bool
	ShotAt   time.Time
	HasTime  bool
}

var (
	errNotImage  = errors.New("不认识的图片格式（只支持 JPEG 与 HEIC/HEIF）")
	errNoEXIF    = errors.New("这张图片没有 EXIF 信息")
	errNeedField = errors.New("EXIF 里没有拍摄时间或 GPS")
	// exif 时间没有时区，按本地理解（手机拍的就是当地时间）
	timeLayout = "2006:01:02 15:04:05"
)

// Parse 读出照片的拍摄时间与 GPS，自动识别 JPEG 与 HEIC/HEIF。
// 缺 GPS 或时间都能容忍，用 Has* 判断。
func Parse(r io.Reader) (Info, error) {
	// HEIC 的 Exif item 位置由 iloc 指到绝对偏移，可能在文件靠后，所以要读全；
	// 上限 32MB 兜底（调用方另有更严的大小限制）。
	buf, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil {
		return Info{}, err
	}
	if len(buf) >= 2 && buf[0] == 0xFF && buf[1] == 0xD8 {
		return parseJPEG(buf)
	}
	// ISO BMFF：第 5~8 字节是 ftyp
	if len(buf) >= 12 && string(buf[4:8]) == "ftyp" {
		return parseHEIF(buf)
	}
	return Info{}, errNotImage
}

// parseJPEG 扫 JPEG 的段，找到 APP1 里的 EXIF。
func parseJPEG(buf []byte) (Info, error) {
	i := 2
	for i+4 <= len(buf) {
		if buf[i] != 0xFF { // 段间的填充字节
			i++
			continue
		}
		marker := buf[i+1]
		switch {
		case marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			i += 2 // 这些标记后面没有长度字段
			continue
		case marker == 0xDA || marker == 0xD9: // 图像数据开始 / 文件结束
			return Info{}, errNoEXIF
		}
		size := int(buf[i+2])<<8 | int(buf[i+3])
		if size < 2 || i+2+size > len(buf) {
			break
		}
		seg := buf[i+4 : i+2+size]
		if marker == 0xE1 && len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" {
			return parseTIFF(seg[6:])
		}
		i += 2 + size
	}
	return Info{}, errNoEXIF
}

// ParseAll 是 Parse 的便利版：把「有照片但没 GPS」也算成软失败，
// 返回尽量多的信息，同时给出缺什么。
func ParseAll(r io.Reader) (Info, error) {
	info, err := Parse(r)
	if err != nil {
		return info, err
	}
	if !info.HasGPS && !info.HasTime {
		return info, errNeedField
	}
	return info, nil
}

type entry struct {
	typ    uint16
	count  uint32
	valOff [4]byte
}

// readIFD 读出某个 IFD 的所有条目（tag -> entry）。
func readIFD(t []byte, off int, bo binary.ByteOrder) map[uint16]entry {
	out := map[uint16]entry{}
	if off < 0 || off+2 > len(t) {
		return out
	}
	n := int(bo.Uint16(t[off : off+2]))
	base := off + 2
	if base+n*12 > len(t) {
		n = (len(t) - base) / 12
	}
	for i := 0; i < n; i++ {
		p := base + i*12
		e := entry{
			typ:   bo.Uint16(t[p+2 : p+4]),
			count: bo.Uint32(t[p+4 : p+8]),
		}
		copy(e.valOff[:], t[p+8:p+12])
		out[bo.Uint16(t[p:p+2])] = e
	}
	return out
}

// typeSize 是 EXIF 里每种类型的字节数。
func typeSize(typ uint16) int {
	switch typ {
	case 1, 2, 6, 7: // BYTE / ASCII / SBYTE / UNDEFINED
		return 1
	case 3, 8: // SHORT / SSHORT
		return 2
	case 4, 9, 11: // LONG / SLONG / FLOAT
		return 4
	case 5, 10, 12: // RATIONAL / SRATIONAL / DOUBLE
		return 8
	}
	return 1
}

// valueOffset 返回值所在位置的偏移。总长 > 4 字节时 valOff 存的是偏移，
// 否则值直接内联在 valOff 里（左对齐）。
func (e entry) valueOffset(t []byte, bo binary.ByteOrder) int {
	if int(e.count)*typeSize(e.typ) > 4 {
		return int(bo.Uint32(e.valOff[:]))
	}
	// 内联值：算出它在整个 t 里的位置
	return -1
}

// inline 返回内联值（4 字节以内）的字节。
func (e entry) inline() []byte { return e.valOff[:] }

// ascii 读 ASCII 类型的值（EXIF 里以 NUL 结尾）。
func (e entry) ascii(t []byte, bo binary.ByteOrder) string {
	total := int(e.count)
	if total <= 0 {
		return ""
	}
	var raw []byte
	if total <= 4 {
		raw = e.inline()[:total]
	} else {
		off := int(bo.Uint32(e.valOff[:]))
		if off < 0 || off+total > len(t) {
			return ""
		}
		raw = t[off : off+total]
	}
	return strings.TrimRight(string(raw), "\x00")
}

// rationals 读一组 RATIONAL（用 uint32 对表示），n 是想要的个数。
func (e entry) rationals(t []byte, bo binary.ByteOrder, n int) ([]float64, bool) {
	off := int(bo.Uint32(e.valOff[:]))
	if off < 0 || off+n*8 > len(t) {
		return nil, false
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		num := bo.Uint32(t[off+i*8 : off+i*8+4])
		den := bo.Uint32(t[off+i*8+4 : off+i*8+8])
		if den == 0 {
			return nil, false
		}
		out[i] = float64(num) / float64(den)
	}
	return out, true
}

func parseTIFF(t []byte) (Info, error) {
	var info Info
	if len(t) < 8 {
		return info, errNoEXIF
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return info, errNoEXIF
	}
	if bo.Uint16(t[2:4]) != 0x002A {
		return info, errNoEXIF
	}
	ifd0 := readIFD(t, int(bo.Uint32(t[4:8])), bo)

	// 拍摄时间在 Exif 子 IFD（IFD0 里的 0x8769 指向它）
	if e, ok := ifd0[0x8769]; ok {
		sub := readIFD(t, int(bo.Uint32(e.valOff[:])), bo)
		if d, ok := sub[0x9003]; ok {
			if s := strings.TrimSpace(d.ascii(t, bo)); s != "" {
				if tm, err := time.ParseInLocation(timeLayout, s, time.Local); err == nil {
					info.ShotAt, info.HasTime = tm, true
				}
			}
		}
	}

	// GPS 在另一个子 IFD（IFD0 里的 0x8825）
	if e, ok := ifd0[0x8825]; ok {
		gps := readIFD(t, int(bo.Uint32(e.valOff[:])), bo)
		lat, okLat := gpsCoord(gps, t, bo, 1, 2)
		lon, okLon := gpsCoord(gps, t, bo, 3, 4)
		if okLat && okLon {
			info.Lat, info.Lon, info.HasGPS = lat, lon, true
		}
	}
	if !info.HasGPS && !info.HasTime {
		return info, fmt.Errorf("%w", errNeedField)
	}
	return info, nil
}

// gpsCoord 把「度分秒 + 方位」拼成十进制度。refTag 是 1/3（N-S、E-W），valTag 是 2/4。
func gpsCoord(gps map[uint16]entry, t []byte, bo binary.ByteOrder, refTag, valTag uint16) (float64, bool) {
	refE, ok1 := gps[refTag]
	valE, ok2 := gps[valTag]
	if !ok1 || !ok2 {
		return 0, false
	}
	parts, ok := valE.rationals(t, bo, 3)
	if !ok {
		return 0, false
	}
	v := parts[0] + parts[1]/60 + parts[2]/3600
	switch strings.ToUpper(strings.TrimSpace(refE.ascii(t, bo))) {
	case "S", "W":
		v = -v
	}
	return v, true
}
