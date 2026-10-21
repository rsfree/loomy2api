// errors.go 错误汇总：把失败请求按 (账号 × 模型 × 状态码) 聚合。
//
// 为什么不是再开一个日志视图：原始失败几十条同质，人眼抓不到差异（某个号报错 44 次、
// 另一个只 1 次）。聚合成十几行才读得动。
//
// 数据源就是台账里的失败记录（已带 status 与上游 error 原文），不新增存储、
// 不去正则解析日志行。
package panel

import (
	"sort"
	"time"

	"github.com/yuanjie-ai/loomy2api/internal/usage"
)

// ErrStat 一行「账号 × 模型 × 状态」聚合。
type ErrStat struct {
	Account string `json:"account"`
	Model   string `json:"model,omitempty"`
	Status  int    `json:"status"`
	Count   int    `json:"count"`
	Last    string `json:"last"`             // 最近一次 HH:MM:SS
	Sample  string `json:"sample,omitempty"` // 上游错误原文（首次样本，供下钻）
}

// errorSummary 汇总窗口内的失败（from 为 unix 秒；0 = 不设下界）。
//
// shortcut: 用 Recent 一次性取整环（20000 条上限），台账本身也是这个量级；
// 若将来台账容量上调，这里要改成按时间遍历。
func (p *Panel) errorSummary(from int64) []ErrStat {
	type key struct {
		account, model string
		status         int
	}
	const scan = 20000
	agg := map[key]*ErrStat{}
	for _, r := range p.ledger.Recent(scan, usage.RecordFilter{From: from}) {
		if r.Status >= 200 && r.Status < 300 {
			continue
		}
		k := key{r.Account, r.Model, r.Status}
		st := agg[k]
		if st == nil {
			st = &ErrStat{Account: r.Account, Model: r.Model, Status: r.Status, Sample: r.Error}
			agg[k] = st
		}
		st.Count++
		if ts := time.Unix(r.Time, 0).Format("15:04:05"); ts > st.Last {
			st.Last = ts
		}
	}
	out := make([]ErrStat, 0, len(agg))
	for _, st := range agg {
		out = append(out, *st)
	}
	// 次数降序（最吵的排前面），次数相同按账号名稳定排序
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Account < out[j].Account
	})
	return out
}
