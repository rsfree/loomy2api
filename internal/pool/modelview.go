// modelview.go 模型锁池视图：哪些模型不能用、锁了几个号、还要锁多久。
//
// 与账号池视图互补：账号池回答「哪些号不能用」，这里回答「哪个模型在集体受限」。
// 数据源是各账号的 modelCool（模型级限流），零新增存储；无锁时返回 nil。
// 语义对齐 workbuddy2api-panel 的 ModelLockView（单域版：Loomy 没有 cn/global 分域）。
package pool

import (
	"sort"
)

// ModelLockRow 一个被锁模型的全池聚合。
type ModelLockRow struct {
	Model    string `json:"model"`
	State    string `json:"state"` // partial 部分限流 / locked 全池锁定 / starved 此刻无号
	Total    int    `json:"total"` // 参与选号的账号数（非禁用、非暂停、有登录态）
	Locked   int    `json:"locked"`
	Servable int    `json:"servable"`  // 此刻还能服务该模型的账号数
	UnlockAt int64  `json:"unlock_at"` // 最早解锁（unix 秒）
	FullyAt  int64  `json:"fully_unlock_at"`
	Reason   string `json:"reason,omitempty"`
}

// ModelLockView 汇总当前所有「有未过期模型级限流」的模型，按严重度排序。
func (p *Pool) ModelLockView() []ModelLockRow {
	all := p.All()
	n := nowF()

	type agg struct {
		locked   int
		untilMin float64
		untilMax float64
		reason   string
		model    string
	}
	rows := map[string]*agg{}
	routable := 0 // 参与选号的账号总数

	for _, a := range all {
		if !a.Enabled || a.Paused || !a.SessionValid() {
			continue
		}
		routable++
		a.rt.mu.Lock()
		for model, mc := range a.rt.modelCool {
			if mc.Until <= n {
				continue
			}
			r := rows[model]
			if r == nil {
				r = &agg{model: model, untilMin: mc.Until, untilMax: mc.Until}
				rows[model] = r
			}
			r.locked++
			if mc.Until < r.untilMin {
				r.untilMin = mc.Until
				r.reason = mc.Reason
			}
			if mc.Until > r.untilMax {
				r.untilMax = mc.Until
			}
		}
		a.rt.mu.Unlock()
	}
	if len(rows) == 0 {
		return nil
	}

	out := make([]ModelLockRow, 0, len(rows))
	for _, r := range rows {
		row := ModelLockRow{
			Model:    r.model,
			Total:    routable,
			Locked:   r.locked,
			UnlockAt: int64(r.untilMin),
			FullyAt:  int64(r.untilMax),
			Reason:   r.reason,
		}
		for _, a := range all {
			if !a.Enabled || a.Paused || !a.SessionValid() {
				continue
			}
			if a.HealthyForModel(r.model) {
				row.Servable++
			}
		}
		switch {
		case row.Servable > 0:
			row.State = "partial"
		case row.Total == 0:
			row.State = "starved"
		default:
			row.State = "locked"
		}
		out = append(out, row)
	}
	// 全池锁定 → 无号 → 部分限流；同级按最早解锁排。
	order := map[string]int{"locked": 0, "starved": 1, "partial": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if order[out[i].State] != order[out[j].State] {
			return order[out[i].State] < order[out[j].State]
		}
		return out[i].UnlockAt < out[j].UnlockAt
	})
	return out
}
