// Package pool 账号池：账号模型、选号、冷却/熔断、会话粘性、在途租约与状态持久化。
//
// 语义对齐 workbuddy2api-panel 的账号池，按 Loomy 的凭证形态重写：
//   - Loomy 没有 refresh token，登录态 14 天到期只能重新登录；
//     因此账号以「手机号 + 密码」为长期凭证，session 是可再生的工作凭证。
//   - 上游额度是积分（points）；余额耗尽属于硬冷却，等日额度重置。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/loomy2api/internal/account"
	"github.com/rsfree/loomy2api/internal/upstream"
)

// 冷却种类。
const (
	CoolNone = ""
	CoolSoft = "soft" // 限流类，有界指数退避
	CoolHard = "hard" // 额度耗尽，等到恢复墙钟
)

// Account 一个 Loomy 账号及其运行态。
type Account struct {
	Name    string `json:"name"`
	LoginID string `json:"loginid,omitempty"`
	// Remark 账号备注：纯展示，不参与选号 / 计费 / 鉴权。
	// 早期版本把它放在浏览器 localStorage，换浏览器、换入口（127.0.0.1 与局域网 IP
	// 是两个 origin）、清站点数据都会丢；落进 auths/<账号>.json 后与账号一起备份。
	Remark   string `json:"remark,omitempty"`
	Password string `json:"password,omitempty"`
	Enabled  bool   `json:"enabled"`
	Paused   bool   `json:"paused,omitempty"` // 暂停选号：不参与转发，但照常签到/刷额度（保号）
	Proxy    string `json:"proxy,omitempty"`

	Session    string `json:"session,omitempty"`
	UserID     string `json:"userid,omitempty"`
	ExpireAt   int64  `json:"expireAt,omitempty"`
	ObtainedAt int64  `json:"obtainedAt,omitempty"`

	Identity account.Identity `json:"identity,omitempty"`

	// 邀请码状态（缓存自上游，落盘）。
	//
	// 上游一个账号**只能绑一次**别人的码，绑完永久 activated、再绑不再给分，
	// 所以这几个字段是「这张一次性车票用没用过」的本地账本：面板据此判断
	// 哪些号还能参与互绑，批量执行时也靠它做预筛（失败仍以现场 GET 为准）。
	InviteCode    string `json:"invite_code,omitempty"`    // 自持的邀请码（被别人绑的那个）
	InviteApplied string `json:"invite_applied,omitempty"` // 自己绑过的别人的码
	InviteReward  int64  `json:"invite_reward,omitempty"`  // 从邀请码累计拿到的积分
	InviteBoundAt int64  `json:"invite_bound_at,omitempty"`
	// InviteUsed 自己这个码已经被几个人用过（本地记账；collectInvites 时用上游
	// usedCount 校正）。上游 maxUses 恒为 1000 不可信，真实上限由配置
	// invite_max_uses 决定（默认 5）。
	InviteUsed int `json:"invite_used,omitempty"`
	// InviteExhausted 上游明确拒绝过这个码（用完 / 失效）。一旦置位就不再尝试，
	// 换池里下一个还有余量的码——这比猜上限更可靠。
	InviteExhausted bool `json:"invite_exhausted,omitempty"`

	// 额度缓存（来自 /points/records + /points/first-login）
	Balance        *int64 `json:"balance,omitempty"`
	DailyBalance   *int64 `json:"daily_balance,omitempty"`
	Available      *int64 `json:"available,omitempty"`
	Permanent      *int64 `json:"permanent,omitempty"`
	DailyQuota     *int64 `json:"daily_quota,omitempty"`
	DailyUsed      *int64 `json:"daily_used,omitempty"`
	DailyCycleDate string `json:"daily_cycle_date,omitempty"`
	QuotaAt        int64  `json:"quota_updated_at,omitempty"`

	// 累计计数（落盘，重启不清零——用户对账要用）
	Requests   int64 `json:"requests"`
	PointsUsed int64 `json:"pointsUsed"`
	Failures   int64 `json:"failures"`

	// 运行态（不落盘的部分由 Runtime 承载）
	rt runtime
}

// runtime 非持久化的运行态。
type runtime struct {
	mu sync.Mutex

	cooldownUntil float64
	coolKind      string
	softStreak    int

	breakerUntil   float64
	breakerFails   int
	breakerRetries int

	modelCool map[string]modelCooldown

	lastUsed float64
	inflight int

	lastError     string
	dailyConsumed int64
}

type modelCooldown struct {
	Until  float64 `json:"until"`
	Reason string  `json:"reason"`
	Hits   int     `json:"hits"`
}

// absorb 把另一份运行态搬过来（runtime 内含互斥锁，只能逐字段复制）。
// 调用方须持有目标与源的锁：这里只在池的写锁内、对刚构造的空 runtime 调用。
func (r *runtime) absorb(src *runtime) {
	src.mu.Lock()
	defer src.mu.Unlock()
	r.cooldownUntil = src.cooldownUntil
	r.coolKind = src.coolKind
	r.softStreak = src.softStreak
	r.breakerUntil = src.breakerUntil
	r.breakerFails = src.breakerFails
	r.breakerRetries = src.breakerRetries
	if len(src.modelCool) > 0 {
		r.modelCool = make(map[string]modelCooldown, len(src.modelCool))
		for k, v := range src.modelCool {
			r.modelCool[k] = v
		}
	}
	r.lastUsed = src.lastUsed
	r.inflight = src.inflight
	r.lastError = src.lastError
	r.dailyConsumed = src.dailyConsumed
}

// SessionValid 登录态是否仍未过期且有值。
func (a *Account) SessionValid() bool {
	return a.Session != "" && (a.ExpireAt == 0 || a.ExpireAt > time.Now().Unix())
}

// DaysLeft 登录态剩余天数（未设置过期时间返回 -1）。
func (a *Account) DaysLeft() float64 {
	if a.ExpireAt == 0 {
		return -1
	}
	return float64(a.ExpireAt-time.Now().Unix()) / 86400
}

// InBreaker 是否处于熔断。
func (a *Account) InBreaker() bool {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.breakerUntil > nowF()
}

// InCooldown 是否处于冷却（含熔断）。
func (a *Account) InCooldown() bool {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.cooldownUntil > nowF() || a.rt.breakerUntil > nowF()
}

// Healthy 能否参与选号（不看额度）。
func (a *Account) Healthy() bool {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	n := nowF()
	return a.Enabled && !a.Paused && a.SessionValid() && a.rt.cooldownUntil <= n && a.rt.breakerUntil <= n
}

// HealthyForModel 模型感知的健康判定：被该模型限流的账号换模型仍可用。
func (a *Account) HealthyForModel(model string) bool {
	if !a.Healthy() {
		return false
	}
	return !a.ModelCooled(model)
}

// ModelCooled 该模型是否仍在冷却中。
func (a *Account) ModelCooled(model string) bool {
	if model == "" {
		return false
	}
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	item, ok := a.rt.modelCool[model]
	if !ok {
		return false
	}
	if item.Until <= nowF() {
		delete(a.rt.modelCool, model)
		return false
	}
	return true
}

// Cool 冷却到 now+seconds（固定时长，不叠加）。
func (a *Account) Cool(seconds float64, kind, reason string) {
	if seconds < 1 {
		seconds = 1
	}
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.cooldownUntil = nowF() + seconds
	a.rt.coolKind = kind
	if reason != "" {
		a.rt.lastError = reason
	}
}

// CoolUntil 冷却到指定墙钟（上游给了权威重置时间时用它）。
func (a *Account) CoolUntil(until float64, kind, reason string) {
	if until <= nowF() {
		until = nowF() + 0.001
	}
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.cooldownUntil = until
	a.rt.coolKind = kind
	if reason != "" {
		a.rt.lastError = reason
	}
}

// CoolSoft 429/限流类软冷却，返回本次截止时间。
//
//   - 上游给了重置墙钟 → 对齐到该墙钟（截断到 max），不做指数放大；
//   - 没给 → 有界指数退避 base × 2^(streak-1)，封顶 max；
//   - 已在软冷却中再次命中 → 不推进 streak、不延长（防「越重试越冷」）。
func (a *Account) CoolSoft(baseSeconds, maxSeconds float64, reason string, resetAt float64) float64 {
	n := nowF()
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if resetAt > 0 {
		until := resetAt
		if lim := n + maxSeconds; until > lim {
			until = lim
		}
		if until <= n {
			until = n + 0.001
		}
		a.rt.cooldownUntil = until
		a.rt.coolKind = CoolSoft
		if reason != "" {
			a.rt.lastError = reason
		}
		return until
	}
	if a.rt.coolKind == CoolSoft && n < a.rt.cooldownUntil {
		return a.rt.cooldownUntil
	}
	a.rt.softStreak++
	shift := a.rt.softStreak - 1
	if shift > 6 {
		shift = 6
	}
	d := baseSeconds * float64(int64(1)<<uint(shift))
	if d < 1 {
		d = 1
	}
	if d > maxSeconds {
		d = maxSeconds
	}
	a.rt.cooldownUntil = n + d
	a.rt.coolKind = CoolSoft
	if reason != "" {
		a.rt.lastError = reason
	}
	return a.rt.cooldownUntil
}

// CoolHard 硬冷却（额度耗尽）。
func (a *Account) CoolHard(until float64, reason string) {
	a.CoolUntil(until, CoolHard, reason)
}

// CoolModel 记录一次模型级限流（换模型即可继续用）。
func (a *Account) CoolModel(model string, seconds float64, reason string) {
	if model == "" {
		return
	}
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if a.rt.modelCool == nil {
		a.rt.modelCool = map[string]modelCooldown{}
	}
	item := a.rt.modelCool[model]
	item.Until = nowF() + seconds
	item.Reason = reason
	item.Hits++
	a.rt.modelCool[model] = item
}

// NoteError 记一次「账号自身」的失败，达阈值则熔断（逐次加倍，封顶 cap）。
func (a *Account) NoteError(threshold int, base, cap float64) bool {
	if threshold < 1 {
		threshold = 1
	}
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.breakerFails++
	if a.rt.breakerFails < threshold {
		return false
	}
	d := base * float64(int64(1)<<uint(minInt(a.rt.breakerRetries, 6)))
	if d > cap {
		d = cap
	}
	a.rt.breakerFails = 0
	a.rt.breakerRetries++
	a.rt.breakerUntil = nowF() + d
	return true
}

// NoteSuccess 成功一次：清熔断 + 清软冷却退避。
func (a *Account) NoteSuccess() {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.breakerFails = 0
	a.rt.breakerRetries = 0
	a.rt.breakerUntil = 0
	a.rt.softStreak = 0
	if a.rt.coolKind == CoolSoft {
		a.rt.cooldownUntil = 0
		a.rt.coolKind = ""
	}
}

// ResetCooldowns 人工解冻：清掉全部冷却与熔断（不动 disabled）。
func (a *Account) ResetCooldowns() {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.cooldownUntil = 0
	a.rt.coolKind = ""
	a.rt.softStreak = 0
	a.rt.breakerUntil = 0
	a.rt.breakerFails = 0
	a.rt.breakerRetries = 0
	a.rt.modelCool = nil
}

// Touch 记录一次使用。
func (a *Account) Touch() {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.lastUsed = nowF()
}

// AcquireInflight 尝试占一个在途名额；limit<=0 表示不限。
func (a *Account) AcquireInflight(limit int) bool {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if limit <= 0 {
		a.rt.inflight++
		return true
	}
	if a.rt.inflight >= limit {
		return false
	}
	a.rt.inflight++
	return true
}

// ReleaseInflight 释放在途名额。
func (a *Account) ReleaseInflight() {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if a.rt.inflight > 0 {
		a.rt.inflight--
	}
}

// Inflight 当前在途数。
func (a *Account) Inflight() int {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.inflight
}

// LastUsed 最近使用时间（unix 秒，含小数）。
func (a *Account) LastUsed() float64 {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.lastUsed
}

// LastError 最近一次错误描述。
func (a *Account) LastError() string {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.lastError
}

// SetLastError 记录最近错误。
func (a *Account) SetLastError(msg string) {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.lastError = msg
}

// AddPointsUsed 累加已耗积分。
func (a *Account) AddPointsUsed(n int64) {
	a.rt.mu.Lock()
	a.PointsUsed += n
	a.rt.dailyConsumed += n
	a.rt.mu.Unlock()
}

// DailyConsumed 当日本地累计消耗。
func (a *Account) DailyConsumed() int64 {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.dailyConsumed
}

// CheckinDone 今日是否已完成签到（额度快照的周期日 == 今天）。
// 上游口径：每日登录后 first-login 自动刷新，dailyCycleDate 即为已签到凭证。
func (a *Account) CheckinDone() bool {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if a.DailyCycleDate == "" {
		return false
	}
	return a.DailyCycleDate == time.Now().Format("2006-01-02")
}

// Public 面板/接口视图（无秘密）。
func (a *Account) Public() map[string]any {
	a.rt.mu.Lock()
	n := nowF()
	coolLeft := a.rt.cooldownUntil - n
	breakerLeft := a.rt.breakerUntil - n
	kind := a.rt.coolKind
	softStreak := a.rt.softStreak
	inflight := a.rt.inflight
	lastErr := a.rt.lastError
	dailyConsumed := a.rt.dailyConsumed
	modelCool := map[string]any{}
	for k, v := range a.rt.modelCool {
		if v.Until > n {
			modelCool[k] = map[string]any{
				"seconds_left": round1(v.Until - n),
				"reason":       v.Reason,
			}
		}
	}
	breakerFails := a.rt.breakerFails
	a.rt.mu.Unlock()

	sess := ""
	if a.Session != "" {
		// 不管长短一律只露前 8 位：短会话原样返回等于把凭证直接吐给浏览器。
		if len(a.Session) > 8 {
			sess = a.Session[:8] + "…"
		} else {
			sess = strings.Repeat("•", len(a.Session))
		}
	}
	out := map[string]any{
		"name":                  a.Name,
		"uid":                   a.Name, // 面板统一身份键（蓝本 workbuddy 用 uid，Loomy 用账号名）
		"nickname":              a.Name,
		"remark":                a.Remark,
		"loginid":               a.LoginID,
		"userid":                a.UserID,
		"enabled":               a.Enabled,
		"paused":                a.Paused,
		"disabled":              !a.Enabled,
		"has_password":          a.Password != "",
		"proxy":                 a.Proxy,
		"session":               sess,
		"session_valid":         a.SessionValid(),
		"session_days_left":     nil,
		"expire_at":             a.ExpireAt,
		"checkin_done":          a.CheckinDone(),
		"balance":               nil,
		"daily_balance":         nil,
		"available":             nil,
		"permanent":             nil,
		"daily_quota":           nil,
		"daily_used":            nil,
		"daily_cycle_date":      "",
		"quota_updated_at":      a.QuotaAt,
		"requests":              a.Requests,
		"points_used":           a.PointsUsed,
		"daily_consumed":        dailyConsumed,
		"failures":              a.Failures,
		"in_cooldown":           coolLeft > 0 || breakerLeft > 0,
		"cooldown_seconds_left": maxF(round1(coolLeft), 0),
		"cooldown_kind":         kind,
		"soft_streak":           softStreak,
		"in_breaker":            breakerLeft > 0,
		"breaker_seconds_left":  maxF(round1(breakerLeft), 0),
		"breaker_fails":         breakerFails,
		"model_cooldowns":       modelCool,
		"inflight":              inflight,
		"last_error":            lastErr,
		"identity": map[string]any{
			"devid":            a.Identity.DevID,
			"ua":               a.Identity.UA,
			"modelid":          a.Identity.ModelID,
			"version":          a.Identity.Version,
			"campus_device_id": clip(a.Identity.CampusDev, 32),
			"bound":            a.Identity.DevID != "",
		},
	}
	if d := a.DaysLeft(); d >= 0 {
		out["session_days_left"] = round2(d)
	}
	if a.Balance != nil {
		out["balance"] = *a.Balance
	}
	if a.DailyBalance != nil {
		out["daily_balance"] = *a.DailyBalance
	}
	if a.Available != nil {
		out["available"] = *a.Available
	}
	if a.Permanent != nil {
		out["permanent"] = *a.Permanent
	}
	if a.DailyQuota != nil {
		out["daily_quota"] = *a.DailyQuota
	}
	if a.DailyUsed != nil {
		out["daily_used"] = *a.DailyUsed
	}
	out["daily_cycle_date"] = a.DailyCycleDate
	return out
}

// SetQuota 更新额度缓存（来自 upstream.Quota 的真实快照）。
func (a *Account) SetQuota(q *upstream.Quota) {
	if q == nil {
		return
	}
	a.rt.mu.Lock()
	a.Balance, a.DailyBalance, a.Available = q.Balance, q.Daily, q.Available
	a.Permanent, a.DailyQuota, a.DailyUsed = q.Permanent, q.DailyQuota, q.DailyUsed
	a.DailyCycleDate, a.QuotaAt = q.CycleDate, q.RefreshedAt
	a.rt.mu.Unlock()
}

// AvailablePoints 可用积分（未知返回 nil）。
func (a *Account) AvailablePoints() *int64 {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if a.Available != nil {
		return a.Available
	}
	// 上游没给 availableBalance 时用 balance+daily 估。
	if a.Balance == nil && a.DailyBalance == nil {
		return nil
	}
	var sum int64
	if a.Balance != nil {
		sum += *a.Balance
	}
	if a.DailyBalance != nil {
		sum += *a.DailyBalance
	}
	return &sum
}

// AccountFromMap 从持久化 JSON 还原账号。
func AccountFromMap(raw map[string]any, index int) *Account {
	name := strOf(raw["name"], raw["loginid"])
	if name == "" {
		name = "account" + itoa(index+1)
	}
	acc := &Account{
		Name:       name,
		LoginID:    strOf(raw["loginid"], raw["phone"]),
		Password:   strOf(raw["password"]),
		Proxy:      strOf(raw["proxy"]),
		Remark:     strOf(raw["remark"]),
		Session:    strOf(raw["session"]),
		UserID:     strOf(raw["userid"]),
		ExpireAt:   int64Of(raw["expireAt"], raw["expire_at"]),
		ObtainedAt: int64Of(raw["obtainedAt"], raw["obtained_at"]),
		Requests:   int64Of(raw["requests"]),
		PointsUsed: int64Of(raw["pointsUsed"], raw["points_used"]),
		Failures:   int64Of(raw["failures"]),
	}
	acc.Enabled = true
	if v, ok := raw["enabled"]; ok {
		acc.Enabled = toBool(v)
	}
	if v, ok := raw["paused"]; ok {
		acc.Paused = toBool(v)
	}
	if ident, ok := raw["identity"].(map[string]any); ok {
		acc.Identity = account.Identity{
			DevID:     strOf(ident["devid"]),
			UA:        strOf(ident["ua"]),
			ModelID:   strOf(ident["modelid"]),
			Version:   strOf(ident["version"]),
			CampusDev: strOf(ident["campus_device_id"]),
			CreatedAt: int64Of(ident["created_at"]),
		}
	}
	// 额度快照（见 persisted 注释：重启后不该归零）。
	acc.Balance = ptrFrom(raw, "balance")
	acc.DailyBalance = ptrFrom(raw, "daily_balance")
	acc.Available = ptrFrom(raw, "available")
	acc.Permanent = ptrFrom(raw, "permanent")
	acc.DailyQuota = ptrFrom(raw, "daily_quota")
	acc.DailyUsed = ptrFrom(raw, "daily_used")
	acc.DailyCycleDate = strOf(raw["daily_cycle_date"])
	acc.QuotaAt = int64Of(raw["quota_updated_at"])
	return acc
}

// ptrFrom 从落盘 JSON 里取可选整数（缺失返回 nil——「未知」与「0」必须能区分，
// 0 积分是真实状态，nil 是还没查过）。
func ptrFrom(raw map[string]any, key string) *int64 {
	v, ok := raw[key]
	if !ok {
		return nil
	}
	n, ok := v.(float64)
	if !ok {
		return nil
	}
	i := int64(n)
	return &i
}

// ------------------------------------------------------------- persistence

// persisted 落盘的账号形态（只存长期凭证与计数，不存运行态）。
func (a *Account) persisted() map[string]any {
	out := map[string]any{
		"name":       a.Name,
		"enabled":    a.Enabled,
		"requests":   a.Requests,
		"pointsUsed": a.PointsUsed,
		"failures":   a.Failures,
	}
	if a.Paused {
		out["paused"] = true
	}
	if a.LoginID != "" {
		out["loginid"] = a.LoginID
	}
	if a.Password != "" {
		out["password"] = a.Password
	}
	if a.Proxy != "" {
		out["proxy"] = a.Proxy
	}
	// 备注必须写进快照：SaveAccount 是先 MarshalIndent 快照再整体覆盖文件，
	// 漏一个字段就会被下一次额度刷新（每 30 分钟全量 persist）静默冲掉。
	if a.Remark != "" {
		out["remark"] = a.Remark
	}
	if a.Session != "" {
		out["session"] = a.Session
	}
	if a.UserID != "" {
		out["userid"] = a.UserID
	}
	if a.ExpireAt != 0 {
		out["expireAt"] = a.ExpireAt
	}
	if a.ObtainedAt != 0 {
		out["obtainedAt"] = a.ObtainedAt
	}
	if a.Identity.DevID != "" {
		out["identity"] = a.Identity
	}
	// 额度快照：面板的「积分 / 每日签到 / 周期日」全靠它，不落盘就会在重启后
	// 全部归零（面板显示 0 积分、未签到，直到下一次刷新）。上游查询很便宜但
	// 重启瞬间没有它，用户会以为额度丢了。
	a.rt.mu.Lock()
	for key, v := range map[string]*int64{
		"balance":       a.Balance,
		"daily_balance": a.DailyBalance,
		"available":     a.Available,
		"permanent":     a.Permanent,
		"daily_quota":   a.DailyQuota,
		"daily_used":    a.DailyUsed,
	} {
		if v != nil {
			out[key] = *v
		}
	}
	if a.DailyCycleDate != "" {
		out["daily_cycle_date"] = a.DailyCycleDate
	}
	if a.QuotaAt != 0 {
		out["quota_updated_at"] = a.QuotaAt
	}
	a.rt.mu.Unlock()
	return out
}

// LoadAccounts 读取账号目录（*.json，文件名即账号名兜底）。
func LoadAccounts(dir string) ([]*Account, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Account
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		// 支持单账号对象与账号数组两种形态。
		if arr, ok := m["accounts"].([]any); ok {
			for i, item := range arr {
				if am, ok := item.(map[string]any); ok {
					out = append(out, AccountFromMap(am, len(out)+i))
				}
			}
			continue
		}
		out = append(out, AccountFromMap(m, len(out)))
	}
	return out, nil
}

// SaveAccount 原子写回单个账号凭证（0600）。
func SaveAccount(dir string, a *Account) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(a.persisted(), "", "  ")
	if err != nil {
		return err
	}
	name := safeFileName(a.Name)
	if name == "" {
		name = safeFileName(a.LoginID)
	}
	if name == "" {
		name = "account"
	}
	target := filepath.Join(dir, name+".json")
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// RemoveAccountFile 删除账号凭证文件。
func RemoveAccountFile(dir string, name string) error {
	target := filepath.Join(dir, safeFileName(name)+".json")
	err := os.Remove(target)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func safeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == '@':
			b.WriteRune('_')
		}
	}
	return b.String()
}
