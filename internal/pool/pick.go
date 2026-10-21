// pick.go 选号策略：三因子加权 + Top-N 短名单抽签，另附 round_robin / balance / lru。
package pool

import (
	"math/rand"
	"sort"
)

// 三因子权重（对齐 workbuddy2api-panel 的 ×10 / ×1 / ×3）。
const (
	WCredits = 10.0
	WIdle    = 1.0
	WSuccess = 3.0

	// IdleSaturateSeconds 闲置补偿的饱和时长。超过它闲置收益不再增长，
	// 否则久置账号会长期霸榜。
	IdleSaturateSeconds = 300.0
)

// weightOf 算一个账号的选号权重（恒 > 0，避免除零/全零抽签）。
//
//	weight = 余额占比 ×10 + 闲置补偿 ×1 + 成功率 ×3
//
// 余额多者权重高（优先扛流量），刚用过的短暂靠后（避免单号被打爆）。
func weightOf(a *Account, maxAvailable float64) float64 {
	if maxAvailable > 0 {
		if v := a.AvailablePoints(); v != nil {
			ratio := float64(*v) / maxAvailable
			if ratio < 0 {
				ratio = 0
			}
			if ratio > 1 {
				ratio = 1
			}
			return ratio*WCredits + idleWeight(a) + successWeight(a)
		}
	}
	return 1.0*WCredits + idleWeight(a) + successWeight(a)
}

func idleWeight(a *Account) float64 {
	idle := nowF() - a.LastUsed()
	if idle < 0 {
		idle = 0
	}
	r := idle / IdleSaturateSeconds
	if r > 1 {
		r = 1
	}
	return r * WIdle
}

func successWeight(a *Account) float64 {
	total := a.Requests + a.Failures
	if total <= 0 {
		return 0.5 * WSuccess
	}
	return float64(a.Requests) / float64(total) * WSuccess
}

// pickTier 账号的积分档位（越小越优先）。0 = 今日赠送额度可用；
// 1 = 只剩永久积分；2 = 其余（余额未知 / 查询失败 / 两头都空）。
//
// ⚠️ 余额未知**必须垫底**而不是排最前：宁可先用能确认余额的号，也不要把流量
// 押在一个连额度都没查到的号上（dsh-providers-hub 的同类选择器把「查询失败」
// 显式归最后一档，理由一致）。
func pickTier(a *Account) int {
	if a.DailyBalance != nil && *a.DailyBalance > 0 {
		return 0
	}
	perm := a.Permanent
	if perm == nil {
		perm = a.Balance
	}
	if perm != nil && *perm > 0 {
		return 1
	}
	return 2
}

// preferDailyTier 只保留最优档位的候选（档内顺序不变，交给后面的加权抽签）。
//
// 上游每日赠送按自然日清零、永久积分不过期，所以「先花今天会作废的」是严格的
// 经济上占优；上游又不会因赠送额度耗尽而报错（静默改扣永久积分并照常 200），
// 这件事只能在这里做。
func preferDailyTier(cands []*Account) []*Account {
	if len(cands) <= 1 {
		return cands
	}
	best := 2
	for _, a := range cands {
		if t := pickTier(a); t < best {
			best = t
		}
	}
	if best == 2 {
		return cands // 全员未知/无余额：不分层，保持原行为
	}
	out := make([]*Account, 0, len(cands))
	for _, a := range cands {
		if pickTier(a) == best {
			out = append(out, a)
		}
	}
	return out
}

// pickWeighted Top-N 短名单 + 加权随机抽签。
//
// 为何先截断再抽签：直接对全体加权随机，低权重账号仍有概率被抽中；
// 只按权重取最大值又会让一个账号被反复打。Top-N + 名单内加权随机兼顾两者。
func pickWeighted(cands []*Account, topN int) *Account {
	if len(cands) == 0 {
		return nil
	}
	if topN < 1 {
		topN = 1
	}
	maxAvail := 0.0
	for _, a := range cands {
		if v := a.AvailablePoints(); v != nil && float64(*v) > maxAvail {
			maxAvail = float64(*v)
		}
	}
	type scored struct {
		acc *Account
		w   float64
		idx int
	}
	items := make([]scored, len(cands))
	for i, a := range cands {
		items[i] = scored{acc: a, w: weightOf(a, maxAvail), idx: i}
	}
	// 等权重先洗牌，避免字典序靠后的账号永远进不了短名单。
	seen := map[float64]struct{}{}
	dup := false
	for _, it := range items {
		if _, ok := seen[it.w]; ok {
			dup = true
			break
		}
		seen[it.w] = struct{}{}
	}
	if dup {
		rand.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].w > items[j].w })
	short := items
	if len(short) > topN {
		short = short[:topN]
	}
	total := 0.0
	for _, it := range short {
		total += it.w
	}
	if total <= 0 {
		return short[0].acc
	}
	roll := rand.Float64() * total
	acc := 0.0
	for _, it := range short {
		acc += it.w
		if roll <= acc {
			return it.acc
		}
	}
	return short[len(short)-1].acc
}

func maxBy(cands []*Account, fn func(*Account) float64) *Account {
	if len(cands) == 0 {
		return nil
	}
	best := cands[0]
	bv := fn(best)
	for _, a := range cands[1:] {
		if v := fn(a); v > bv {
			best, bv = a, v
		}
	}
	return best
}

func minBy(cands []*Account, fn func(*Account) float64) *Account {
	if len(cands) == 0 {
		return nil
	}
	best := cands[0]
	bv := fn(best)
	for _, a := range cands[1:] {
		if v := fn(a); v < bv {
			best, bv = a, v
		}
	}
	return best
}

func shuffleCopy(cands []*Account) []*Account {
	out := make([]*Account, len(cands))
	copy(out, cands)
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}
