// pool.go 账号池：选号、会话粘性、在途租约、状态持久化。
package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options 池的可调参数（来自 config，热更新时整体替换）。
type Options struct {
	Strategy         string
	PickTopN         int
	MaxInFlight      int
	BalanceFloor     int64
	SoftRateBase     float64
	SoftRateMax      float64
	NotFoundCool     float64
	CooldownFallback float64
	BreakerThreshold int
	BreakerBase      float64
	BreakerMax       float64
	StickyTTL        float64
	StickyMaxEntries int
	// PreferDaily 选号时把「今日赠送额度 > 0」的账号放前面（见 pickTier）。
	PreferDaily bool
	// DeadModelTTL 404 模型失效记录的存活时长（0 = 用 DefaultDeadModelTTL）。
	DeadModelTTL time.Duration
}

// DefaultOptions 返回一份默认参数。
func DefaultOptions() Options {
	return Options{
		Strategy:         "weighted",
		PickTopN:         5,
		BalanceFloor:     0,
		SoftRateBase:     600,
		SoftRateMax:      7200,
		NotFoundCool:     60,
		CooldownFallback: 300,
		BreakerThreshold: 5,
		BreakerBase:      60,
		BreakerMax:       1800,
		StickyTTL:        1800,
		StickyMaxEntries: 2000,
	}
}

// Pool 账号池。
type Pool struct {
	mu     sync.RWMutex
	byName map[string]*Account
	order  []string // 稳定顺序（面板展示 + round_robin）

	opts atomic.Value // Options

	sticky *Sticky

	stateFp   string
	dirty     atomic.Bool
	stopCh    chan struct{}
	closeOnce sync.Once

	// pendingState 从 state.json 读到的账号计数，待 ApplyState 合并到账号上。
	pendingState map[string]map[string]any

	pickSeq uint64

	// dead 模型级「已下架」台账（见 deadmodel.go）：404 罚模型、不罚账号。
	deadMu sync.Mutex
	dead   map[string]*deadEntry

	// onSave 落盘钩子（测试可替换）；nil 用默认文件写。
	onSave func(map[string]any) error
}

// New 构造账号池（stateFp 为空则不落盘）。
func New(stateFp string) *Pool {
	p := &Pool{
		byName:  map[string]*Account{},
		stateFp: stateFp,
	}
	p.opts.Store(DefaultOptions())
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// SetOptions 注入参数（非正/空值保留默认）。
func (p *Pool) SetOptions(o Options) {
	cur := p.Options()
	if o.Strategy == "" {
		o.Strategy = cur.Strategy
	}
	if o.PickTopN <= 0 {
		o.PickTopN = cur.PickTopN
	}
	if o.SoftRateBase <= 0 {
		o.SoftRateBase = cur.SoftRateBase
	}
	if o.SoftRateMax <= 0 {
		o.SoftRateMax = cur.SoftRateMax
	}
	if o.NotFoundCool <= 0 {
		o.NotFoundCool = cur.NotFoundCool
	}
	if o.CooldownFallback <= 0 {
		o.CooldownFallback = cur.CooldownFallback
	}
	if o.BreakerThreshold <= 0 {
		o.BreakerThreshold = cur.BreakerThreshold
	}
	if o.BreakerBase <= 0 {
		o.BreakerBase = cur.BreakerBase
	}
	if o.BreakerMax <= 0 {
		o.BreakerMax = cur.BreakerMax
	}
	if o.StickyTTL < 0 {
		o.StickyTTL = cur.StickyTTL
	}
	if o.StickyMaxEntries <= 0 {
		o.StickyMaxEntries = cur.StickyMaxEntries
	}
	if o.MaxInFlight < 0 {
		o.MaxInFlight = cur.MaxInFlight
	}
	if o.BalanceFloor < 0 {
		o.BalanceFloor = cur.BalanceFloor
	}
	p.opts.Store(o)
	p.mu.Lock()
	if p.sticky == nil {
		p.sticky = NewSticky(o.StickyTTL, o.StickyMaxEntries)
	} else {
		p.sticky.Reconfigure(o.StickyTTL, o.StickyMaxEntries)
	}
	p.mu.Unlock()
}

// Options 当前参数。
func (p *Pool) Options() Options {
	if v, ok := p.opts.Load().(Options); ok {
		return v
	}
	return DefaultOptions()
}

// Sticky 返回会话粘性表。
func (p *Pool) Sticky() *Sticky {
	p.mu.RLock()
	s := p.sticky
	p.mu.RUnlock()
	if s == nil {
		p.mu.Lock()
		if p.sticky == nil {
			o := p.Options()
			p.sticky = NewSticky(o.StickyTTL, o.StickyMaxEntries)
		}
		s = p.sticky
		p.mu.Unlock()
	}
	return s
}

// Close 停止后台落盘并做最后一次 Flush（幂等）。
func (p *Pool) Close() {
	if p.stateFp == "" {
		return
	}
	p.closeOnce.Do(func() { close(p.stopCh) })
	p.Flush()
}

// Add 加入或更新一个账号（同名覆盖长期凭证，保留运行态）。
func (p *Pool) Add(a *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.Name == "" {
		a.Name = a.LoginID
	}
	if old, ok := p.byName[a.Name]; ok {
		// 保留运行态与计数，更新凭证与身份（runtime 含互斥锁，逐字段搬运而非赋值）。
		a.rt.absorb(&old.rt)
		if a.Requests == 0 {
			a.Requests = old.Requests
		}
		if a.PointsUsed == 0 {
			a.PointsUsed = old.PointsUsed
		}
	} else {
		p.order = append(p.order, a.Name)
	}
	p.byName[a.Name] = a
	p.dirty.Store(true)
}

// Remove 移除账号并删除凭证文件。
func (p *Pool) Remove(name string) {
	p.mu.Lock()
	delete(p.byName, name)
	for i, n := range p.order {
		if n == name {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	p.dirty.Store(true)
	p.Flush()
}

// Get 按名字取账号。
func (p *Pool) Get(name string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byName[name]
}

// All 返回全部账号（稳定顺序）。
func (p *Pool) All() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Account, 0, len(p.order))
	for _, n := range p.order {
		if a, ok := p.byName[n]; ok {
			out = append(out, a)
		}
	}
	return out
}

// Count 账号总数。
func (p *Pool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.order)
}

// SyncToDir 与凭证目录对齐：新文件加入、已删除文件剔除（保留状态）。
func (p *Pool) SyncToDir(accounts []*Account) {
	for _, a := range accounts {
		if p.Get(a.Name) == nil {
			p.Add(a)
		}
	}
	p.dirty.Store(true)
}

// ErrNoAccount 无可用账号。
var ErrNoAccount = fmt.Errorf("没有可用账号（全部冷却 / 熔断 / 额度耗尽 / 在途占满）")

// Acquire 选一个可用账号并占一个在途名额。exclude 为本次已失败过的账号名。
//
// 会话粘性优先：sessionKey 命中且绑定账号健康则直接用它（不参与抽签）；
// 未命中 / 绑定号不可用时走正常选号，并在调用方 reportSuccess 时写回绑定。
func (p *Pool) Acquire(exclude []string, model string, sessionKey string) (*Account, error) {
	opts := p.Options()

	skip := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		skip[n] = true
	}

	// 1) 粘性命中
	if sessionKey != "" {
		if name := p.Sticky().Get(sessionKey); name != "" && !skip[name] {
			if a := p.Get(name); a != nil && a.HealthyForModel(model) && !p.floorBlocked(a, model) {
				if a.AcquireInflight(opts.MaxInFlight) {
					a.Touch()
					p.bumpSeq(a)
					return a, nil
				}
			}
		}
	}

	// 2) 正常选号
	all := p.All()
	candidates := make([]*Account, 0, len(all))
	for _, a := range all {
		if skip[a.Name] || !a.HealthyForModel(model) || p.floorBlocked(a, model) {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		// 全冷却时从「非禁用、非暂停、非额度耗尽」的软冷却/熔断账号里选最早到期者顶班。
		fallback := make([]*Account, 0)
		for _, a := range all {
			if skip[a.Name] || !a.Enabled || a.Paused || !a.SessionValid() {
				continue
			}
			if p.floorBlocked(a, model) {
				continue
			}
			fallback = append(fallback, a)
		}
		sort.SliceStable(fallback, func(i, j int) bool {
			return p.cooldownLeft(fallback[i]) < p.cooldownLeft(fallback[j])
		})
		for _, a := range fallback {
			if a.AcquireInflight(opts.MaxInFlight) {
				a.Touch()
				p.bumpSeq(a)
				return a, nil
			}
		}
		return nil, ErrNoAccount
	}

	// 每日赠送优先：把「今日额度 > 0」的号提到最前，只在其内部比权重。
	//
	// 为什么必须分层而不是揉进权重：上游的每日赠送额度**按自然日清零**，
	// 永久积分不过期。平均加权会先烧永久积分、把每天会作废的赠送额度留到浪费。
	// 上游不会因为赠送额度耗尽报错（继续扣永久、照常 200），所以只能靠选号侧排序。
	if opts.PreferDaily {
		candidates = preferDailyTier(candidates)
	}

	// 在途未满的优先
	usable := make([]*Account, 0, len(candidates))
	for _, a := range candidates {
		usable = append(usable, a)
	}

	var chosen *Account
	switch strings.ToLower(opts.Strategy) {
	case "round_robin":
		chosen = p.pickRoundRobin(usable)
	case "balance":
		chosen = maxBy(usable, func(a *Account) float64 {
			if v := a.AvailablePoints(); v != nil {
				return float64(*v)
			}
			return -1
		})
	case "lru":
		chosen = minBy(usable, func(a *Account) float64 {
			if v := a.LastUsed(); v > 0 {
				return v
			}
			return 0 // 从未使用 → 最优先
		})
	default: // weighted
		chosen = pickWeighted(usable, opts.PickTopN)
	}
	if chosen == nil {
		return nil, ErrNoAccount
	}
	// 在途名额：选中者满了就换下一个候选（不阻塞）。
	acquired := chosen.AcquireInflight(opts.MaxInFlight)
	if !acquired {
		for _, a := range shuffleCopy(usable) {
			if a != chosen && a.HealthyForModel(model) && a.AcquireInflight(opts.MaxInFlight) {
				chosen = a
				acquired = true
				break
			}
		}
	}
	if !acquired { // 候选全部在途占满
		return nil, ErrNoAccount
	}
	chosen.Touch()
	p.bumpSeq(chosen)
	return chosen, nil
}

// Release 释放一个在途名额（必须与 Acquire 成对，defer 里调）。
func (p *Pool) Release(a *Account) {
	if a != nil {
		a.ReleaseInflight()
	}
}

// ReportSuccess 成功一次：清熔断/软冷却、写会话绑定、更新额度。
func (p *Pool) ReportSuccess(a *Account, sessionKey, model string, points int64) {
	if a == nil {
		return
	}
	a.NoteSuccess()
	a.Requests++
	if points > 0 {
		a.AddPointsUsed(points)
	}
	if sessionKey != "" {
		p.Sticky().Bind(sessionKey, a.Name)
	}
	p.dirty.Store(true)
}

// ReportFailure 失败一次：按错误分类决定账号处置。
func (p *Pool) ReportFailure(a *Account, status int, body, sessionKey, model string) {
	if a == nil {
		return
	}
	opts := p.Options()
	class := Classify(status, body)

	// 404 = 模型不在上游目录（「该模型暂未开放」），是**模型**的问题，不是账号的：
	// 换号、等冷却都不会好，罚账号还会让一个打错模型名的请求冻掉唯一可用号。
	// 故记进模型级台账（/v1/models 会隐藏它）并**完全不碰账号**——连 Failures
	// 都不加，否则 404 会污染该账号在所有模型上的成功率权重。
	if class == FaultNotFound {
		p.MarkModelDead(model, FaultMessage(body))
		return
	}

	a.Failures++
	a.SetLastError(fmt.Sprintf("http %d: %s", status, truncate(body, 160)))
	p.dirty.Store(true)
	if sessionKey != "" {
		p.Sticky().Unbind(sessionKey, a.Name)
	}

	switch class {
	case FaultQuota:
		// 额度耗尽 → 硬冷却到次日 00:00（自然日重置，见 NextDailyReset）
		a.CoolHard(NextDailyReset(), "额度耗尽")
	case FaultModelLimit:
		// 模型级限流：只锁该模型，换模型立即可用
		a.CoolModel(model, opts.SoftRateBase, "模型级限流")
	case FaultRateLimit:
		a.CoolSoft(opts.SoftRateBase, opts.SoftRateMax, truncate(body, 120), ResetAtFromBody(body))
	case FaultSession:
		// 登录态失效：Loomy 没有 refresh token，只能重新登录。
		// 一次失败不判死（可能是网络抖动），连败达阈值才禁用。
		if a.NoteError(opts.BreakerThreshold, opts.BreakerBase, opts.BreakerMax) {
			a.Enabled = false
			a.SetLastError("登录态失效（连续失败达阈值，需重新登录）")
		}
	case FaultServer:
		a.NoteError(opts.BreakerThreshold, opts.BreakerBase, opts.BreakerMax)
	case FaultClient:
		// 客户端问题：不罚账号
	default:
		a.Cool(opts.CooldownFallback, CoolSoft, truncate(body, 120))
	}
}

// floorBlocked 余额保底：余额已知且低于阈值时停止接单（0 = 关闭）。
func (p *Pool) floorBlocked(a *Account, model string) bool {
	opts := p.Options()
	if opts.BalanceFloor <= 0 {
		return false
	}
	v := a.AvailablePoints()
	if v == nil {
		return false // 从未查过余额 → 不拦（否则新号永远进不来）
	}
	return *v <= opts.BalanceFloor
}

func (p *Pool) cooldownLeft(a *Account) float64 {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	n := nowF()
	left := a.rt.cooldownUntil - n
	if b := a.rt.breakerUntil - n; b > left {
		left = b
	}
	return left
}

func (p *Pool) bumpSeq(a *Account) {
	p.pickSeq++
	a.rt.mu.Lock()
	a.rt.lastUsed = nowF()
	a.rt.mu.Unlock()
}

// pickRoundRobin 选最近最少被选中者（用 lastUsed 的严格全序近似）。
func (p *Pool) pickRoundRobin(cands []*Account) *Account {
	return minBy(cands, func(a *Account) float64 {
		if v := a.LastUsed(); v > 0 {
			return v
		}
		return 0
	})
}

// Overview 汇总统计。
func (p *Pool) Overview() map[string]any {
	all := p.All()
	var healthy, cooling, disabled, paused, inflight int
	var totalAvailable int64
	var availableKnown bool
	var requests, points, failures int64
	for _, a := range all {
		if !a.Enabled {
			disabled++
		} else if a.Paused {
			paused++ // 暂停选号：单独一档（保号任务照常，但不接流量）
		} else if a.Healthy() {
			healthy++
		} else {
			cooling++
		}
		inflight += a.Inflight()
		requests += a.Requests
		points += a.PointsUsed
		failures += a.Failures
		if v := a.AvailablePoints(); v != nil {
			totalAvailable += *v
			availableKnown = true
		}
	}
	out := map[string]any{
		"total":       len(all),
		"healthy":     healthy,
		"cooling":     cooling,
		"disabled":    disabled,
		"paused":      paused,
		"inflight":    inflight,
		"requests":    requests,
		"points_used": points,
		"failures":    failures,
	}
	if availableKnown {
		out["available_points"] = totalAvailable
	}
	if s := p.Sticky(); s != nil {
		out["sticky"] = s.Snapshot()
	}
	return out
}

// ------------------------------------------------------------------- state

// stateSnapshot 落盘形态。
func (p *Pool) stateSnapshot() map[string]any {
	accounts := map[string]any{}
	for _, a := range p.All() {
		accounts[a.Name] = map[string]any{
			"requests":     a.Requests,
			"pointsUsed":   a.PointsUsed,
			"failures":     a.Failures,
			"enabled":      a.Enabled,
			"balance":      a.Balance,
			"dailyBalance": a.DailyBalance,
			"available":    a.Available,
			"permanent":    a.Permanent,
			"dailyQuota":   a.DailyQuota,
			"dailyUsed":    a.DailyUsed,
			"dailyCycle":   a.DailyCycleDate,
			"quotaAt":      a.QuotaAt,
		}
	}
	return map[string]any{
		"saved_at":    time.Now().Unix(),
		"accounts":    accounts,
		"dead_models": p.deadSnapshot(),
	}
}

// Flush 立即落盘（幂等；无变化时不写）。
func (p *Pool) Flush() {
	if p.stateFp == "" {
		return
	}
	snap := p.stateSnapshot()
	if p.onSave != nil {
		_ = p.onSave(snap)
		p.dirty.Store(false)
		return
	}
	buf, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.stateFp), 0o755); err != nil {
		return
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, p.stateFp); err == nil {
		p.dirty.Store(false)
	}
}

// SetSaver 替换落盘实现（测试注入）。
func (p *Pool) SetSaver(fn func(map[string]any) error) { p.onSave = fn }

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var snap struct {
		Accounts   map[string]map[string]any `json:"accounts"`
		DeadModels map[string]map[string]any `json:"dead_models"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return
	}
	p.pendingState = snap.Accounts
	p.restoreDead(snap.DeadModels)
}

// pendingState 账号文件先加载，再由 ApplyState 合并计数（避免相互覆盖）。
func (p *Pool) ApplyState() {
	p.mu.Lock()
	pending := p.pendingState
	p.pendingState = nil
	p.mu.Unlock()
	if pending == nil {
		return
	}
	for name, st := range pending {
		a := p.Get(name)
		if a == nil {
			continue
		}
		a.Requests = int64Of(st["requests"])
		a.PointsUsed = int64Of(st["pointsUsed"])
		a.Failures = int64Of(st["failures"])
		if v, ok := st["enabled"]; ok {
			a.Enabled = toBool(v)
		}
		if st["balance"] != nil || st["dailyBalance"] != nil || st["available"] != nil {
			b := ptrInt64(st["balance"])
			d := ptrInt64(st["dailyBalance"])
			av := ptrInt64(st["available"])
			a.Balance, a.DailyBalance, a.Available = b, d, av
			a.Permanent = ptrInt64(st["permanent"])
			a.DailyQuota = ptrInt64(st["dailyQuota"])
			a.DailyUsed = ptrInt64(st["dailyUsed"])
			if v, ok := st["dailyCycle"].(string); ok {
				a.DailyCycleDate = v
			}
			a.QuotaAt = int64Of(st["quotaAt"])
		}
	}
}

func ptrInt64(v any) *int64 {
	if v == nil {
		return nil
	}
	n := int64Of(v)
	return &n
}

func (p *Pool) startFlusher() {
	p.stopCh = make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case <-t.C:
				if p.dirty.Load() {
					p.Flush()
				}
			}
		}
	}()
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}
