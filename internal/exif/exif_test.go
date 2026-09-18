package exif

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// buildTIFF 拼出小端 TIFF（IFD0 + ExifIFD + GPS IFD），JPEG 与 HEIC 共用。
func buildTIFF(lat, lon float64, shot string) []byte {
	tiff := &bytes.Buffer{}
	tiff.WriteString("II")
	binary.Write(tiff, binary.LittleEndian, uint16(0x002A))
	binary.Write(tiff, binary.LittleEndian, uint32(8)) // IFD0 偏移

	const (
		ifd0Off = 8
		exifOff = ifd0Off + 30 // IFD0: 2 条目 = 2 + 24 + 4
		shotOff = exifOff + 18 // ExifIFD: 1 条目 = 2 + 12 + 4
		shotLen = len("2006:01:02 15:04:05") + 1
		gpsOff  = shotOff + shotLen
		ratOff  = gpsOff + 54 // GPS IFD: 4 条目 = 2 + 48 + 4
	)
	// IFD0
	binary.Write(tiff, binary.LittleEndian, uint16(2))
	tiff.Write(entryBytes(0x8769, 4, 1, uint32(exifOff)))
	tiff.Write(entryBytes(0x8825, 4, 1, uint32(gpsOff)))
	binary.Write(tiff, binary.LittleEndian, uint32(0))
	// ExifIFD：DateTimeOriginal
	binary.Write(tiff, binary.LittleEndian, uint16(1))
	tiff.Write(entryBytes(0x9003, 2, uint32(shotLen), uint32(shotOff)))
	binary.Write(tiff, binary.LittleEndian, uint32(0))
	tiff.WriteString(shot + "\x00")
	// GPS IFD
	binary.Write(tiff, binary.LittleEndian, uint16(4))
	latRef, lonRef := byte('N'), byte('E')
	alat, alon := math.Abs(lat), math.Abs(lon)
	if lat < 0 {
		latRef = 'S'
	}
	if lon < 0 {
		lonRef = 'W'
	}
	tiff.Write(entryBytes(1, 2, 2, uint32(latRef)))
	tiff.Write(entryBytes(2, 5, 3, uint32(ratOff)))
	tiff.Write(entryBytes(3, 2, 2, uint32(lonRef)))
	tiff.Write(entryBytes(4, 5, 3, uint32(ratOff+24)))
	binary.Write(tiff, binary.LittleEndian, uint32(0))
	writeDMS(tiff, alat)
	writeDMS(tiff, alon)
	return tiff.Bytes()
}

// buildJPEG 拼一张最小 JPEG：SOI + APP1(EXIF) + EOI。
// 只有 EXIF 段是真的，后面不需要有效图像数据——解析器读到 APP1 就返回。
func buildJPEG(t *testing.T, lat, lon float64, shot string) []byte {
	t.Helper()
	body := &bytes.Buffer{}
	body.Write([]byte("Exif\x00\x00"))
	body.Write(buildTIFF(lat, lon, shot))
	out := &bytes.Buffer{}
	out.Write([]byte{0xFF, 0xD8, 0xFF, 0xE1})
	binary.Write(out, binary.BigEndian, uint16(body.Len()+2))
	out.Write(body.Bytes())
	out.Write([]byte{0xFF, 0xD9})
	return out.Bytes()
}

// mkBox 拼一个 ISO BMFF 盒子（32 位长度）。
func mkBox(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

// buildHEIC 拼一个最小 HEIC：ftyp + meta(iinf + iloc) + Exif item。
// Exif item 放在文件末尾，iloc 里填它的绝对偏移——真实文件也是这样定位的。
func buildHEIC(t *testing.T, lat, lon float64, shot string) []byte {
	t.Helper()
	tiff := buildTIFF(lat, lon, shot)
	// HEIF 的 Exif item = 4 字节 TIFF 偏移 + TIFF 本体
	item := append([]byte{0, 0, 0, 4}, tiff...)
	ftyp := mkBox("ftyp", append([]byte("heic"), []byte{0, 0, 0, 0, 'h', 'e', 'i', 'c'}...))

	// infe v2：version+flags + item_ID + protection + item_type + 空名字
	infe := mkBox("infe", append([]byte{2, 0, 0, 0, 0, 1, 0, 0}, append([]byte("Exif"), 0)...))
	iinf := mkBox("iinf", append([]byte{0, 0, 0, 0, 0, 1}, infe...))

	// iloc v0：offsetSize=4 lengthSize=4 baseSize=0，一条数据段
	ilocPayload := []byte{0, 0, 0, 0, 0x44, 0x00, 0, 1, 0, 1, 0, 0, 0, 1}
	offPos := len(ilocPayload)
	ilocPayload = append(ilocPayload, 0, 0, 0, 0) // extent_offset 待回填
	ilocPayload = append(ilocPayload,
		byte(len(item)>>24), byte(len(item)>>16), byte(len(item)>>8), byte(len(item)))
	iloc := mkBox("iloc", ilocPayload)

	meta := mkBox("meta", append([]byte{0, 0, 0, 0}, append(iinf, iloc...)...))
	// extent_offset 的绝对位置：meta 头(8) + fullbox(4) + iinf + iloc 头(8) + 段内偏移
	pos := 8 + 4 + len(iinf) + 8 + offPos
	binary.BigEndian.PutUint32(meta[pos:pos+4], uint32(len(ftyp)+len(meta)))

	out := &bytes.Buffer{}
	out.Write(ftyp)
	out.Write(meta)
	out.Write(item)
	return out.Bytes()
}

func entryBytes(tag, typ uint16, count uint32, val uint32) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint16(b[0:2], tag)
	binary.LittleEndian.PutUint16(b[2:4], typ)
	binary.LittleEndian.PutUint32(b[4:8], count)
	binary.LittleEndian.PutUint32(b[8:12], val)
	return b
}

// writeDMS 把十进制度拆成 度/分/秒 三个 rational。
func writeDMS(w *bytes.Buffer, v float64) {
	d := math.Floor(v)
	m := math.Floor((v - d) * 60)
	s := ((v-d)*60 - m) * 60
	for _, p := range [][2]uint32{{uint32(d), 1}, {uint32(m), 1}, {uint32(math.Round(s * 10000)), 10000}} {
		binary.Write(w, binary.LittleEndian, p[0])
		binary.Write(w, binary.LittleEndian, p[1])
	}
}

func TestParseGPSAndTime(t *testing.T) {
	// 大理古城附近，EXIF 存的是 WGS-84
	jpg := buildJPEG(t, 25.6890, 100.1560, "2026:08:14 12:40:33")
	info, err := Parse(bytes.NewReader(jpg))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !info.HasGPS {
		t.Fatal("没读到 GPS")
	}
	if math.Abs(info.Lat-25.6890) > 1e-4 || math.Abs(info.Lon-100.1560) > 1e-4 {
		t.Fatalf("坐标不对: %v, %v", info.Lat, info.Lon)
	}
	if !info.HasTime {
		t.Fatal("没读到拍摄时间")
	}
	want := time.Date(2026, 8, 14, 12, 40, 33, 0, time.Local)
	if !info.ShotAt.Equal(want) {
		t.Fatalf("时间不对: %v 期望 %v", info.ShotAt, want)
	}
}

func TestParseSouthWest(t *testing.T) {
	jpg := buildJPEG(t, -33.8688, -70.6693, "2025:01:02 03:04:05")
	info, err := Parse(bytes.NewReader(jpg))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if info.Lat > 0 || info.Lon > 0 {
		t.Fatalf("南纬/西经应该是负数: %v, %v", info.Lat, info.Lon)
	}
}

func TestParseRejectsNonImage(t *testing.T) {
	if _, err := Parse(bytes.NewReader([]byte("PNG\x89 not a jpeg"))); err == nil {
		t.Fatal("非图片应该报错")
	}
	bare := []byte{0xFF, 0xD8, 0xFF, 0xD9} // 合法 JPEG 但没有 EXIF 段
	if _, err := Parse(bytes.NewReader(bare)); err == nil {
		t.Fatal("没有 EXIF 应该报错")
	}
}

func TestParseHEIC(t *testing.T) {
	// 喜洲古镇附近，EXIF 存 WGS-84
	heic := buildHEIC(t, 25.8560, 100.1310, "2026:08:15 15:43:00")
	info, err := Parse(bytes.NewReader(heic))
	if err != nil {
		t.Fatalf("HEIC 解析失败: %v", err)
	}
	if !info.HasGPS || math.Abs(info.Lat-25.8560) > 1e-4 || math.Abs(info.Lon-100.1310) > 1e-4 {
		t.Fatalf("坐标不对: %v, %v", info.Lat, info.Lon)
	}
	if !info.HasTime || info.ShotAt.Format("2006-01-02 15:04:05") != "2026-08-15 15:43:00" {
		t.Fatalf("时间不对: %v", info.ShotAt)
	}
}

func TestParseHEICNoExifItem(t *testing.T) {
	// 有 meta/iinf/iloc 但没有 Exif item：应报「没有 EXIF」而不是崩
	infe := mkBox("infe", append([]byte{2, 0, 0, 0, 0, 1, 0, 0}, append([]byte("hvc1"), 0)...))
	iinf := mkBox("iinf", append([]byte{0, 0, 0, 0, 0, 1}, infe...))
	meta := mkBox("meta", append([]byte{0, 0, 0, 0}, iinf...))
	ftyp := mkBox("ftyp", append([]byte("heic"), []byte{0, 0, 0, 0, 'h', 'e', 'i', 'c'}...))
	buf := append(ftyp, meta...)
	if _, err := Parse(bytes.NewReader(buf)); err == nil {
		t.Fatal("没有 Exif item 应该报错")
	}
}

func TestParseTruncatedDoesNotPanic(t *testing.T) {
	// 截断到各种长度都不该 panic（上传中断、文件损坏在真实世界里都会出现）
	full := buildHEIC(t, 25.8560, 100.1310, "2026:08:15 15:43:00")
	for _, n := range []int{1, 5, 12, 20, len(full) / 2, len(full) - 1} {
		if n <= len(full) {
			_, _ = Parse(bytes.NewReader(full[:n]))
		}
	}
	jpg := buildJPEG(t, 25.6890, 100.1560, "2026:08:14 12:40:33")
	for _, n := range []int{1, 4, 10, len(jpg) / 2, len(jpg) - 1} {
		_, _ = Parse(bytes.NewReader(jpg[:n]))
	}
}
