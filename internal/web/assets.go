package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// assetVersion 是静态资源的版本号，页面链接带 ?v=它，一致才允许长期缓存。
// 重新编译或改动静态文件后版本号随之变化，旧缓存自动失效。
var assetVersion = "0"

// initAssetVersion 在启动时计算版本号：调试模式读磁盘（改样式即时生效），
// 正式模式读 embed 的文件内容。
func initAssetVersion(debug bool) {
	h := sha256.New()
	if debug {
		var paths []string
		filepath.Walk("internal/web/static", func(p string, info os.FileInfo, err error) error {
			if err == nil && info != nil && !info.IsDir() {
				paths = append(paths, p)
			}
			return nil
		})
		sort.Strings(paths)
		for _, p := range paths {
			f, err := os.Open(p)
			if err != nil {
				return
			}
			io.Copy(h, f)
			f.Close()
			io.WriteString(h, p)
		}
		assetVersion = hex.EncodeToString(h.Sum(nil))[:12]
		return
	}
	var names []string
	fs.WalkDir(staticFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && d != nil && !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names)
	for _, p := range names {
		f, err := staticFS.Open(p)
		if err != nil {
			return
		}
		io.Copy(h, f)
		f.Close()
		io.WriteString(h, p)
	}
	assetVersion = hex.EncodeToString(h.Sum(nil))[:12]
}
