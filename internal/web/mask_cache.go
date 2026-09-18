package web

import (
	"sync"
	"time"

	"rond-with-you/internal/fog"
)

// maskTTL 是遮罩列表的缓存时长。保存/删除遮罩时会主动失效，这里的 TTL 只是兜底
// （比如多实例部署时另一个进程改了库）。所以可以取长一点。
const maskTTL = 30 * time.Second

// maskCache 缓存迷雾遮罩列表。
//
// 为什么需要：遮罩不在 fogIndex 的内存索引里，每个迷雾瓦片请求都会查一次库，
// 而地图一屏就是几十张瓦片——同一份数据被反复查几十遍。指纹也顺带缓存，
// 它被瓦片 URL 与缓存键用到。
type maskCache struct {
	mu    sync.Mutex
	at    time.Time
	list  []fog.Mask
	fp    string
	valid bool
}

// get 返回缓存的遮罩列表与指纹；ok 为 false 表示需要查库。
// 「站点一块遮罩都没有」也是有效缓存（list 为 nil），所以不能拿 list 判空。
func (c *maskCache) get() (list []fog.Mask, fp string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && time.Since(c.at) < maskTTL {
		return c.list, c.fp, true
	}
	return nil, "", false
}

// put 写入缓存（由调用方查库后调用）。
func (c *maskCache) put(list []fog.Mask) ([]fog.Mask, string) {
	fp := fog.MaskFingerprint(list)
	c.mu.Lock()
	c.list, c.fp, c.at, c.valid = list, fp, time.Now(), true
	c.mu.Unlock()
	return list, fp
}

// invalidate 在遮罩被保存 / 删除后立刻失效，保证改完立即对所有访客生效。
func (c *maskCache) invalidate() {
	c.mu.Lock()
	c.valid = false
	c.mu.Unlock()
}

// masksFor 取遮罩列表（带缓存）。查库失败时返回 nil（等于不遮），与原先的行为一致。
func (s *Server) masksFor() []fog.Mask {
	if list, _, ok := s.masks.get(); ok {
		return list
	}
	list, err := fog.LoadMasks(s.db)
	if err != nil {
		return nil
	}
	out, _ := s.masks.put(list)
	return out
}

// maskSetFor 取遮罩判定集合（带缓存）；没有遮罩时返回 nil。
//
// admin 为 true 时不返回遮罩——站长永远看完整迷雾，否则没法核对遮哪里。
func (s *Server) maskSetFor(admin bool) *fog.MaskSet {
	if admin {
		return nil
	}
	return fog.NewMaskSet(s.masksFor())
}

// maskFP 取遮罩指纹（带缓存），供瓦片 URL 与缓存键使用。
func (s *Server) maskFP() string {
	if _, fp, ok := s.masks.get(); ok {
		return fp
	}
	list := s.masksFor()
	return fog.MaskFingerprint(list)
}
