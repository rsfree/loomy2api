// deadmodel.go 模型级「已下架」台账。
//
// 上游对「模型不在目录里 / 已下架」回 404 `该模型暂未开放，请切换其他模型`，
// 这个 404 与「账号有问题」完全无关：换号、等冷却都不会好。旧实现把它按
// FaultNotFound 罚了账号 60 秒 —— 一个打错模型名的客户端请求就能冻掉唯一可用号。
//
// 这里改成**模型维度**的台账：
//   - 记下 model + 到期时间，命中时直接回 404 给客户端，不再打上游；
//   - 不罚账号（连 Failures 都不加：404 不是账号的错，不该污染成功率权重）；
//   - /v1/models 与面板的模型视图隐藏它，客户端不会再选到；
//   - 带 TTL（默认 30 天）自动回收：模型被下架后用户选不到、不可能靠「再成功一次」
//     自愈，只能靠过期重新试探；上游也可能把模型重新上架。
//
// 判据刻意只有「阳性证据」——**请求真的 404 了**才算，不从目录反推：
// Loomy 的 /models 是全局目录而非按账号权限裁剪，用它做差集会误删
// 「你有权用、只是这份目录没列」的模型（dsh-providers-hub 的 dead-model-store
// 记着同类教训：buddy 那家就是目录残缺）。
package pool

import (
	"sort"
	"time"
)

// DefaultDeadModelTTL 失效模型记录的默认存活时长（30 天）。
const DefaultDeadModelTTL = 30 * 24 * time.Hour

// deadEntry 一条失效模型记录。
type deadEntry struct {
	Until  float64 // unix 秒
	Reason string
	Hits   int // 累计 404 次数（面板展示用）
	At     float64
}

// MarkModelDead 记一条「模型已下架」并落盘（幂等：同模型只延长到期时间）。
// model 为空直接忽略（没拿到模型名的 404 无法归类）。
func (p *Pool) MarkModelDead(model, reason string) {
	if model == "" {
		return
	}
	ttl := p.deadTTL()
	if ttl <= 0 {
		return // TTL 0 = 永久；永久记录无法自愈，故只在显式配置允许时生效
	}
	now := nowF()
	p.deadMu.Lock()
	if p.dead == nil {
		p.dead = map[string]*deadEntry{}
	}
	e := p.dead[model]
	if e == nil {
		e = &deadEntry{At: now, Reason: reason}
		p.dead[model] = e
	}
	e.Hits++
	e.Until = now + ttl.Seconds()
	if reason != "" {
		e.Reason = reason
	}
	p.deadMu.Unlock()
	p.dirty.Store(true)
}

// ModelDead 该模型当前是否处于「已下架」台账内（过期条目顺手清掉）。
func (p *Pool) ModelDead(model string) bool {
	if model == "" {
		return false
	}
	now := nowF()
	p.deadMu.Lock()
	defer p.deadMu.Unlock()
	e := p.dead[model]
	if e == nil {
		return false
	}
	if e.Until <= now {
		delete(p.dead, model)
		return false
	}
	return true
}

// ClearDeadModel 手动解封一个模型（面板「解除」用），返回是否真的删掉了。
func (p *Pool) ClearDeadModel(model string) bool {
	p.deadMu.Lock()
	_, ok := p.dead[model]
	delete(p.dead, model)
	p.deadMu.Unlock()
	if ok {
		p.dirty.Store(true)
	}
	return ok
}

// DeadModels 当前有效的失效模型清单（按最近到期排序）。
func (p *Pool) DeadModels() []map[string]any {
	now := nowF()
	p.deadMu.Lock()
	type row struct {
		model string
		e     deadEntry
	}
	rows := make([]row, 0, len(p.dead))
	for m, e := range p.dead {
		if e.Until <= now {
			delete(p.dead, m)
			continue
		}
		rows = append(rows, row{model: m, e: *e})
	}
	p.deadMu.Unlock()
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].e.Until < rows[j].e.Until })
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"model":              r.model,
			"reason":             r.e.Reason,
			"hits":               r.e.Hits,
			"seen_at":            int64(r.e.At),
			"until":              int64(r.e.Until),
			"seconds_left":       round1(r.e.Until - now),
			"hidden_from_models": true,
		})
	}
	return out
}

// deadSnapshot 落盘用（state.json 的 dead_models 键）。
func (p *Pool) deadSnapshot() map[string]any {
	now := nowF()
	p.deadMu.Lock()
	defer p.deadMu.Unlock()
	out := map[string]any{}
	for m, e := range p.dead {
		if e.Until <= now {
			delete(p.dead, m)
			continue
		}
		out[m] = map[string]any{"until": e.Until, "reason": e.Reason, "hits": e.Hits, "at": e.At}
	}
	return out
}

// restoreDead 从 state.json 恢复台账（过期条目直接丢弃）。
func (p *Pool) restoreDead(raw map[string]map[string]any) {
	if len(raw) == 0 {
		return
	}
	now := nowF()
	p.deadMu.Lock()
	defer p.deadMu.Unlock()
	if p.dead == nil {
		p.dead = map[string]*deadEntry{}
	}
	for m, v := range raw {
		until := numOf(v["until"])
		if m == "" || until <= now {
			continue
		}
		p.dead[m] = &deadEntry{
			Until:  until,
			Reason: strOf(v["reason"]),
			Hits:   int(numOf(v["hits"])),
			At:     numOf(v["at"]),
		}
	}
}

// deadTTL 失效模型记录的存活时长（Options 未配置时用默认 30 天）。
func (p *Pool) deadTTL() time.Duration {
	opts := p.Options()
	if opts.DeadModelTTL > 0 {
		return opts.DeadModelTTL
	}
	return DefaultDeadModelTTL
}
