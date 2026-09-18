package fog

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"path"
	"strings"
)

// layerMaxBytes 是单个留存条目的解压上限（实测最大约 250KB）。
const layerMaxBytes = 8 << 20

// RawLayers 取出快照里「不属于已探索层」的条目（内容原样，条目名也原样）。
//
// fog of world 的快照里有三类瓦片：
//
//	Model/*  已探索层：128×128 块，每块 512 字节位图 + 3 字节元信息（本包解析的就是它）
//	Model/~  另一层位图：每块 512 字节，没有那 3 字节
//	Model/#  只有元信息：每块 3 字节
//
// 后两类我们不解析（站点只用已探索层来渲染），但**必须原样留住**——
// 否则重新导出的快照会丢掉这 55% 的内容，fog of world 那边看到的就是残缺数据。
//
// 留存范围是「快照里的全部条目，除去已探索层与被 Decode 解析掉的那些瓦片」：
// Decode 认的是「文件名能通过瓦片校验」，不认目录，所以判断也要按文件名来，
// 否则同一个文件会被解析一遍、又原样写一遍，包里出现两份。
func RawLayers(data []byte) (map[string][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		p := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		// Model/* 之下就是已探索层，由 fog_blocks 重建
		if rest := strings.TrimPrefix(p, "Model/"); rest != p {
			if i := strings.Index(rest, "/"); i > 0 && rest[:i] == "*" {
				continue
			}
		}
		// 文件名能通过瓦片校验的会被 Decode 解析成块，写回时也走 Model/*，这里不重复留存
		if _, ok := tileIDFromName(path.Base(p)); ok {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		// 留存的条目不解析、也不该被截断：超限就报错，别把半份数据悄悄带进导出包
		raw, err := io.ReadAll(io.LimitReader(rc, layerMaxBytes+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) > layerMaxBytes {
			return nil, fmt.Errorf("条目 %s 超过 %d 字节，疑似异常快照", p, layerMaxBytes)
		}
		out[p] = raw
	}
	return out, nil
}
