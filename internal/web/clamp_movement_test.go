package web

import (
	"database/sql"
	"testing"
	"time"
)

// 改到访时间后把引用它的行程夹回窗口 —— 规则本身的回归。
//
// 真机不变量：位移不早于起点到访的离开时间、不晚于终点到访的到达时间。
// 线上出过的事：把某次到访的到达时间改早之后，原先合法的行程就变成越界
// （实测 2 条超出 26~47 秒，导出的包里带着这个违例）。
func TestClampMovementWindow(t *testing.T) {
	base := time.Date(2026, 9, 13, 3, 30, 0, 0, time.Local)
	at := func(d time.Duration) time.Time { return base.Add(d) }
	valid := func(tt time.Time) sql.NullTime { return sql.NullTime{Time: tt, Valid: true} }

	cases := []struct {
		name       string
		start, end time.Time
		lo, hi     sql.NullTime
		wantStart  time.Time
		wantEnd    time.Time
	}{
		{
			name:  "都在窗口内：不动",
			start: at(10 * time.Minute), end: at(30 * time.Minute),
			lo: valid(at(5 * time.Minute)), hi: valid(at(40 * time.Minute)),
			wantStart: at(10 * time.Minute), wantEnd: at(30 * time.Minute),
		},
		{
			name:  "起点被改晚导致开始时间早于离开：开始推到离开",
			start: at(1 * time.Minute), end: at(30 * time.Minute),
			lo: valid(at(10 * time.Minute)), hi: valid(at(40 * time.Minute)),
			wantStart: at(10 * time.Minute), wantEnd: at(30 * time.Minute),
		},
		{
			name:  "终点被改早导致结束晚于到达：结束拉回到达",
			start: at(10 * time.Minute), end: at(50 * time.Minute),
			lo: valid(at(5 * time.Minute)), hi: valid(at(20 * time.Minute)),
			wantStart: at(10 * time.Minute), wantEnd: at(20 * time.Minute),
		},
		{
			name:  "两端都越界：一起夹",
			start: at(1 * time.Minute), end: at(50 * time.Minute),
			lo: valid(at(10 * time.Minute)), hi: valid(at(20 * time.Minute)),
			wantStart: at(10 * time.Minute), wantEnd: at(20 * time.Minute),
		},
		{
			name:  "窗口被压没了：结束推到开始之后 1 分钟（不能写 NULL）",
			start: at(10 * time.Minute), end: at(50 * time.Minute),
			lo: valid(at(30 * time.Minute)), hi: valid(at(20 * time.Minute)),
			wantStart: at(30 * time.Minute), wantEnd: at(31 * time.Minute),
		},
		{
			name:  "两端到访都没有时间：不夹",
			start: at(10 * time.Minute), end: at(30 * time.Minute),
			lo: sql.NullTime{}, hi: sql.NullTime{},
			wantStart: at(10 * time.Minute), wantEnd: at(30 * time.Minute),
		},
		{
			name:  "终点到访缺失：只夹起点侧",
			start: at(1 * time.Minute), end: at(30 * time.Minute),
			lo: valid(at(10 * time.Minute)), hi: sql.NullTime{},
			wantStart: at(10 * time.Minute), wantEnd: at(30 * time.Minute),
		},
	}
	for _, c := range cases {
		gs, ge := clampMovementWindow(c.start, c.end, c.lo, c.hi)
		if !gs.Equal(c.wantStart) {
			t.Errorf("%s: start = %v，期望 %v", c.name, gs, c.wantStart)
		}
		if !ge.Equal(c.wantEnd) {
			t.Errorf("%s: end = %v，期望 %v", c.name, ge, c.wantEnd)
		}
		if !ge.After(gs) {
			t.Errorf("%s: 夹完 end(%v) 必须晚于 start(%v)", c.name, ge, gs)
		}
	}
}
