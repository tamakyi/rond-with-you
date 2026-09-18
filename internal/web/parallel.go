package web

import (
	"context"
	"log"
	"sync"
	"time"
)

// parallelLimit 是页面内并行查询的并发上限。
// 站点常跑在 1C1G 的小机器上，并发太高会把 PostgreSQL 和 CPU 一起挤住，
// 反而比串行更慢（同一份磁盘在排队）。
const parallelLimit = 6

// slowJobLog 是打慢查询日志的阈值（debug 模式下）。
const slowJobLog = 30 * time.Millisecond

// pageJob 是一次页面内的独立查询。name 只用于日志，方便定位是哪个查询慢。
type pageJob struct {
	name string
	run  func(context.Context) error
}

// runJobs 并发执行互不依赖的查询，返回第一个（按传入顺序）非 nil 错误。
//
// 统计页这类页面有十几个互不依赖的查询，串行执行时页面耗时≈它们之和（实测 ~400ms），
// 并发之后≈最慢的那一个。**任务之间不能写同一份数据**：这里只保证并发安全，
// 不管业务上的写入冲突。
func (s *Server) runJobs(ctx context.Context, jobs ...pageJob) error {
	if len(jobs) <= 1 {
		for _, j := range jobs {
			if err := j.run(ctx); err != nil {
				return err
			}
		}
		return nil
	}
	sem := make(chan struct{}, parallelLimit)
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			start := time.Now()
			errs[i] = jobs[i].run(ctx)
			if d := time.Since(start); d > slowJobLog && s.cfg.Debug {
				log.Printf("慢查询 %s: %v", jobs[i].name, d.Round(time.Millisecond))
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
