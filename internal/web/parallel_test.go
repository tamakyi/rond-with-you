package web

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"rond-with-you/internal/config"
)

func testServer() *Server { return &Server{cfg: &config.Config{}} }

func TestRunJobsRunsConcurrently(t *testing.T) {
	s := testServer()
	const n = 8
	const each = 60 * time.Millisecond
	var done int32
	jobs := make([]pageJob, 0, n)
	for i := 0; i < n; i++ {
		jobs = append(jobs, pageJob{"job", func(context.Context) error {
			time.Sleep(each)
			atomic.AddInt32(&done, 1)
			return nil
		}})
	}
	start := time.Now()
	if err := s.runJobs(context.Background(), jobs...); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if int(done) != n {
		t.Fatalf("只完成了 %d 个任务", done)
	}
	// 并发执行：总耗时应接近单个任务，而不是 n 倍。给足余量避免机器慢时抖动。
	if elapsed > each*n/2 {
		t.Errorf("8 个 %v 的任务耗时 %v，看起来没并发（串行约 %v）", each, elapsed, each*n)
	}
}

func TestRunJobsReturnsFirstErrorInOrder(t *testing.T) {
	s := testServer()
	errA, errB := errors.New("A 错"), errors.New("B 错")
	// 故意让靠后的任务先失败，返回值仍应是传入顺序里的第一个错误
	err := s.runJobs(context.Background(),
		pageJob{"slow-fail", func(context.Context) error {
			time.Sleep(50 * time.Millisecond)
			return errA
		}},
		pageJob{"fast-fail", func(context.Context) error { return errB }},
		pageJob{"ok", func(context.Context) error { return nil }},
	)
	if !errors.Is(err, errA) {
		t.Errorf("返回 %v，期望传入顺序里的第一个错误 %v", err, errA)
	}
}

func TestRunJobsSingleJobRunsInline(t *testing.T) {
	s := testServer()
	ran := false
	if err := s.runJobs(context.Background(), pageJob{"only", func(context.Context) error {
		ran = true
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("单个任务也应执行")
	}
}

func TestRunJobsStopsOnCancelledContext(t *testing.T) {
	s := testServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var ran int32
	jobs := make([]pageJob, 0, 4)
	for i := 0; i < 4; i++ {
		jobs = append(jobs, pageJob{"job", func(ctx context.Context) error {
			// 真实查询在 ctx 取消后就是这样返回的
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			atomic.AddInt32(&ran, 1)
			return nil
		}})
	}
	if err := s.runJobs(ctx, jobs...); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx 已取消时应返回 context.Canceled，实际 %v", err)
	}
	if n := atomic.LoadInt32(&ran); n != 0 {
		t.Errorf("ctx 一开始就已取消，不该有任务真正执行（实际 %d 个）", n)
	}
}
