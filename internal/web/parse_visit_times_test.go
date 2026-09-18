package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func visitReq(form string) *http.Request {
	vals := url.Values{}
	for _, kv := range strings.Split(form, "&") {
		p := strings.SplitN(kv, "=", 2)
		vals.Set(p[0], p[1])
	}
	r, _ := http.NewRequest("POST", "/admin/edit/visit", strings.NewReader(vals.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestParseVisitTimes(t *testing.T) {
	// 只记到达：离开留空应该合法
	arr, dep, ok, msg := parseVisitTimes(visitReq("arrival=2026-09-16T10:00&departure="))
	if !ok {
		t.Fatalf("只记到达应合法，得到: %s", msg)
	}
	if dep != nil {
		t.Fatalf("离开应为 nil，得到 %v", dep)
	}
	if arr.IsZero() {
		t.Fatal("到达时间不应为零值")
	}
	// 两个时间都有
	_, dep, ok, _ = parseVisitTimes(visitReq("arrival=2026-09-16T10:00&departure=2026-09-16T11:30"))
	if !ok || dep == nil {
		t.Fatalf("双时间应合法且 departure 非空, ok=%v dep=%v", ok, dep)
	}
	// 缺到达
	if _, _, ok, _ = parseVisitTimes(visitReq("arrival=&departure=2026-09-16T11:30")); ok {
		t.Fatal("缺到达应不合法")
	}
	// 离开早于到达
	if _, _, ok, _ = parseVisitTimes(visitReq("arrival=2026-09-16T10:00&departure=2026-09-16T09:00")); ok {
		t.Fatal("离开早于到达应不合法")
	}
}
