package web

import (
	"strings"
	"testing"
	"time"
)

func mt(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

// TestMovementWindow 守住行程时间区间的推导与校验。
// 规则来自真机包实测（760 条起点 / 756 条终点全部满足）：位移不早于起点到访的
// 离开时间，也不晚于终点到访的到达时间；ZSTART_/ZEND_ 在真机里 791/791 都有值，
// 所以留空必须能推断出值，不能写 NULL。
func TestMovementWindow(t *testing.T) {
	dep := mt("2026-09-16 10:00")
	from := movementEnds{SrcPK: 1, Arrive: mt("2026-09-16 09:00"), Depart: &dep}
	to := movementEnds{SrcPK: 2, Arrive: mt("2026-09-16 11:00")}
	in := func(s, e string) movementWindowInput {
		return movementWindowInput{From: from, To: to, StartRaw: s, EndRaw: e}
	}

	// 留空：按两端补默认值，即「起点离开 → 终点到达」
	start, end, msg := movementWindow(in("", ""))
	if msg != "" {
		t.Fatalf("留空应能推断，得到错误: %s", msg)
	}
	if !start.Equal(dep) || !end.Equal(to.Arrive) {
		t.Fatalf("推断结果不对: %v ~ %v", start, end)
	}

	// 起点没有离开时间时退回到达时间
	fromNoDep := movementEnds{SrcPK: 1, Arrive: mt("2026-09-16 09:00")}
	if start, _, msg = movementWindow(movementWindowInput{From: fromNoDep, To: to}); msg != "" || !start.Equal(fromNoDep.Arrive) {
		t.Fatalf("缺离开时间应以到达时间为起点: %v %s", start, msg)
	}

	// 合法的手填区间
	if _, _, msg = movementWindow(in("2026-09-16T10:10", "2026-09-16T10:50")); msg != "" {
		t.Fatalf("区间合法却被拒: %s", msg)
	}

	for _, c := range []struct {
		name             string
		start, end       string
		wantMsgSubstring string
	}{
		{"结束早于开始", "2026-09-16T10:50", "2026-09-16T10:10", "必须晚于开始时间"},
		{"开始早于起点离开", "2026-09-16T09:30", "2026-09-16T10:50", "不能早于起点到访的离开时间"},
		{"结束晚于终点到达", "2026-09-16T10:10", "2026-09-16T11:30", "不能晚于终点到访的到达时间"},
		{"只填开始且越界", "2026-09-16T09:00", "", "不能早于起点到访的离开时间"},
		{"时间格式错", "2026/09/16 10:00", "", "格式不正确"},
	} {
		_, _, msg := movementWindow(in(c.start, c.end))
		if msg == "" {
			t.Errorf("%s：应被拒绝但通过了", c.name)
			continue
		}
		if !strings.Contains(msg, c.wantMsgSubstring) {
			t.Errorf("%s：错误信息里应包含 %q，实际为 %q", c.name, c.wantMsgSubstring, msg)
		}
	}
}

// TestKeepStoredTime 守住分钟精度的表单不该磨掉库里的小数秒（到访与行程共用）。
func TestKeepStoredTime(t *testing.T) {
	stored := mt("2026-09-13 20:03").Add(21*time.Second + 323*time.Millisecond)
	typed := mt("2026-09-13 20:03") // 表单回填/手填都只到分钟
	if got := keepStoredTime(typed, &stored); !got.Equal(stored) {
		t.Errorf("同一分钟应沿用库里的值: %v", got)
	}
	// 真改了分钟就按提交值走
	other := mt("2026-09-13 20:10")
	if got := keepStoredTime(other, &stored); !got.Equal(other) {
		t.Errorf("改了分钟应按提交值生效: %v", got)
	}
	// 没有原值（新建）时原样返回
	if got := keepStoredTime(typed, nil); !got.Equal(typed) {
		t.Errorf("无原值时应原样返回: %v", got)
	}
}

// TestMovementWindowKeepsStoredSeconds 守住「只改方式不该动时间」。
// 表单是分钟精度，回填后原样提交会被解析成不带秒的整分钟；与库里同分钟就必须
// 沿用库里原值，否则每次编辑都把真机的小数秒截掉，而且截断后早于起点到访的
// 离开时间，会被校验拒掉——「改方式提示开始时间不能早于离开时间」就是这么来的。
func TestMovementWindowKeepsStoredSeconds(t *testing.T) {
	dep := mt("2026-09-13 20:03").Add(21*time.Second + 323*time.Millisecond)
	from := movementEnds{SrcPK: 1, Arrive: mt("2026-09-13 19:00"), Depart: &dep}
	to := movementEnds{SrcPK: 2, Arrive: mt("2026-09-13 20:31").Add(9 * time.Second)}
	curStart := mt("2026-09-13 20:03").Add(21*time.Second + 323*time.Millisecond)
	curEnd := mt("2026-09-13 20:31").Add(9 * time.Second)

	in := movementWindowInput{From: from, To: to, CurStart: &curStart, CurEnd: &curEnd}
	// 表单回填的就是截到分钟的值
	in.StartRaw, in.EndRaw = "2026-09-13T20:03", "2026-09-13T20:31"
	start, end, msg := movementWindow(in)
	if msg != "" {
		t.Fatalf("只改方式的时间提交应被接受，实际: %s", msg)
	}
	if !start.Equal(curStart) || !end.Equal(curEnd) {
		t.Fatalf("时间被截断了: %v ~ %v，应为 %v ~ %v", start, end, curStart, curEnd)
	}

	// 真的改了分钟才按提交值走
	in.StartRaw, in.EndRaw = "2026-09-13T20:10", "2026-09-13T20:31"
	if start, _, msg = movementWindow(in); msg != "" || start.Equal(curStart) {
		t.Fatalf("改了分钟应按提交值生效: %v %s", start, msg)
	}

	// 手填到同一分钟之外的越界值仍要拒绝
	in.StartRaw, in.EndRaw = "2026-09-13T19:30", "2026-09-13T20:31"
	if _, _, msg = movementWindow(in); !strings.Contains(msg, "不能早于起点到访的离开时间") {
		t.Fatalf("越界的手填值应被拒，实际: %s", msg)
	}
}
