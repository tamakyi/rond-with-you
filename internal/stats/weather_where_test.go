package stats

import (
	"strings"
	"testing"
	"time"
)

// TestWeatherWhereSemantics 钉住天气筛选的两条语义，它们都不是显而易见的：
//
//  1. 时间用**观测时刻 w.at**，不是所属到访的 arrival —— 筛一段时间要的是
//     「这段时间里经历的天气」，横跨边界的那次到访只算落在区间内的小时。
//  2. 只按时间筛时**不能**要求天气能关联到访。删到访时 weather.visit_src 是被置空
//     而不是删行（editVisitDelete），那些失去归属的天气行仍属于「某段时间的天气」；
//     一旦加上 EXISTS，删掉一次到访就会让它那几天的天气从统计里凭空消失。
//     而城市 / 类型 / 标签这类条件必须挂到到访上，那时才该有 EXISTS。
//
// 纯白盒：直接看生成的 SQL 形状，不需要真实数据。
func TestWeatherWhereSemantics(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	t.Run("时间按观测时刻", func(t *testing.T) {
		w := Filter{DatasetID: 9, From: &from, To: &to}.weatherWhere()
		got := w.SQL()
		if !strings.Contains(got, "w.at >= ") || !strings.Contains(got, "w.at < ") {
			t.Errorf("时间条件应落在 w.at 上，实际：%s", got)
		}
		if strings.Contains(got, "v.arrival") {
			t.Errorf("不该用所属到访的 arrival 判时间，实际：%s", got)
		}
		if strings.Contains(got, "EXISTS") {
			t.Errorf("只按时间筛选时不该要求能关联到到访（否则删过到访的天气会消失），实际：%s", got)
		}
		if len(w.args) != 3 {
			t.Errorf("参数个数应为 3（数据集 + 起 + 止），实际 %d：%v", len(w.args), w.args)
		}
	})

	t.Run("城市条件挂到到访上", func(t *testing.T) {
		w := Filter{DatasetID: 9, City: "贵阳市"}.weatherWhere()
		got := w.SQL()
		if !strings.Contains(got, "EXISTS (SELECT 1 FROM visits v WHERE v.src_pk = w.visit_src") {
			t.Errorf("城市条件应通过所属到访判定，实际：%s", got)
		}
		if !strings.Contains(got, "v.dataset_id = ") {
			t.Errorf("EXISTS 里应当带上数据集条件，实际：%s", got)
		}
		if strings.Contains(got, "v.arrival") {
			t.Errorf("时间参数已置空，不该出现 arrival 条件，实际：%s", got)
		}
	})

	t.Run("占位符编号连续", func(t *testing.T) {
		// 外层先写了 dataset + 起止时间（$1~$3），EXISTS 里的条件必须从 $4 接着编，
		// 否则参数会错位（这类错误只在有数据时炸，很难查）。
		// 城市条件本身会带一条 v.dataset_id，所以 EXISTS 里有两个占位符。
		w := Filter{DatasetID: 9, From: &from, To: &to, City: "贵阳市"}.weatherWhere()
		got := w.SQL()
		if !strings.Contains(got, "v.dataset_id = $4") {
			t.Errorf("EXISTS 的第一个占位符应当是 $4，实际：%s", got)
		}
		if !strings.Contains(got, "px.city = $5") {
			t.Errorf("城市条件应当是 $5，实际：%s", got)
		}
		if len(w.args) != 5 {
			t.Errorf("参数个数应为 5（数据集 + 起 + 止 + v.dataset_id + 城市），实际 %d：%v",
				len(w.args), w.args)
		}
	})

	t.Run("区域限制也算到访条件", func(t *testing.T) {
		w := Filter{DatasetID: 9, Regions: []string{"贵阳市"}, RegionsExclude: true}.weatherWhere()
		if !strings.Contains(w.SQL(), "EXISTS") {
			t.Errorf("区域限制应当触发 EXISTS，实际：%s", w.SQL())
		}
	})
}
