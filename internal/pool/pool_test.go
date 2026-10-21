package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestAccount(name string, available int64, enabled bool) *Account {
	v := available
	a := &Account{
		Name:      name,
		Enabled:   enabled,
		Session:   "sess-" + name,
		ExpireAt:  time.Now().Add(7 * 24 * time.Hour).Unix(),
		Available: &v,
	}
	return a
}

// TestWeightPrefersMoreCredits 积分多的账号权重必须更高。
func TestWeightPrefersMoreCredits(t *testing.T) {
	rich := newTestAccount("rich", 1000, true)
	poor := newTestAccount("poor", 100, true)
	max := 1000.0
	wr := weightOf(rich, max)
	wp := weightOf(poor, max)
	if wr <= wp {
		t.Fatalf("富账号权重应更高：%.2f vs %.2f", wr, wp)
	}
}

// TestWeightIdleCompensation 刚用过的账号权重更低（闲置补偿）。
func TestWeightIdleCompensation(t *testing.T) {
	fresh := newTestAccount("fresh", 500, true)
	stale := newTestAccount("stale", 500, true)
	fresh.rt.lastUsed = nowF()       // 刚用过
	stale.rt.lastUsed = nowF() - 600 // 久未使用（超过饱和时长）
	if weightOf(fresh, 500) >= weightOf(stale, 500) {
		t.Error("闲置久的账号权重应更高")
	}
}

// TestWeightNeverZero 权重恒 > 0，避免除零与全零抽签。
func TestWeightNeverZero(t *testing.T) {
	a := newTestAccount("a", 0, true)
	a.Available = nil
	if w := weightOf(a, 0); w <= 0 {
		t.Errorf("权重应 > 0，实际 %.4f", w)
	}
}

// TestWeightSuccessRate 成功率高的账号权重更高。
func TestWeightSuccessRate(t *testing.T) {
	good := newTestAccount("good", 500, true)
	good.Requests, good.Failures = 90, 10
	bad := newTestAccount("bad", 500, true)
	bad.Requests, bad.Failures = 10, 90
	if weightOf(good, 500) <= weightOf(bad, 500) {
		t.Error("成功率高者权重应更高")
	}
}

// TestPickWeightedAlwaysReturns Picker 必须总能返回一个候选（不返回 nil）。
func TestPickWeightedAlwaysReturns(t *testing.T) {
	var cands []*Account
	for i := 0; i < 6; i++ {
		cands = append(cands, newTestAccount(string(rune('a'+i)), int64(100*(i+1)), true))
	}
	for i := 0; i < 200; i++ {
		if got := pickWeighted(cands, 5); got == nil {
			t.Fatal("pickWeighted 返回 nil")
		}
	}
	if pickWeighted(nil, 5) != nil {
		t.Error("空候选应返回 nil")
	}
}

// TestPickWeightedSpreadsLoad 加权抽签不能让单个账号吃掉全部流量。
func TestPickWeightedSpreadsLoad(t *testing.T) {
	var cands []*Account
	for i := 0; i < 3; i++ {
		cands = append(cands, newTestAccount(string(rune('a'+i)), 100, true))
	}
	counts := map[string]int{}
	for i := 0; i < 600; i++ {
		got := pickWeighted(cands, 5)
		counts[got.Name]++
	}
	for _, a := range cands {
		if counts[a.Name] == 0 {
			t.Errorf("账号 %s 一次都没被选中（流量未分散）：%v", a.Name, counts)
		}
	}
}

// TestAcquireSkipsCooling 冷却中的账号不参与选号。
func TestAcquireSkipsCooling(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 100, true))
	p.Add(newTestAccount("b", 100, true))
	p.Get("a").Cool(300, CoolSoft, "test")

	for i := 0; i < 50; i++ {
		acc, err := p.Acquire(nil, "m", "")
		if err != nil {
			t.Fatalf("应能选到 b：%v", err)
		}
		if acc.Name != "b" {
			t.Fatalf("选中了冷却中的账号 %s", acc.Name)
		}
		p.Release(acc)
	}
}

// TestAcquireAllCoolingFallsBack 全部冷却时仍挑一个顶班（不死锁 503）。
func TestAcquireAllCoolingFallsBack(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 100, true))
	p.Get("a").Cool(300, CoolSoft, "test")
	acc, err := p.Acquire(nil, "m", "")
	if err != nil {
		t.Fatalf("全冷却时应能顶班选号：%v", err)
	}
	if acc == nil || acc.Name != "a" {
		t.Fatalf("顶班账号不对: %v", acc)
	}
	p.Release(acc)
}

// TestAcquireNoAccountWhenEmpty 没有账号时必须报错而非 panic。
func TestAcquireNoAccountWhenEmpty(t *testing.T) {
	p := New("")
	if _, err := p.Acquire(nil, "m", ""); err == nil {
		t.Fatal("空池应返回错误")
	}
}

// TestAcquireRespectsExclude 已失败过的账号不再被选中。
func TestAcquireRespectsExclude(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 100, true))
	p.Add(newTestAccount("b", 100, true))
	for i := 0; i < 50; i++ {
		acc, err := p.Acquire([]string{"a"}, "m", "")
		if err != nil {
			t.Fatalf("应能选到 b：%v", err)
		}
		if acc.Name == "a" {
			t.Fatal("排除了 a 却选中 a")
		}
		p.Release(acc)
	}
}

// TestAcquireDisabledNotChosen 禁用账号不参与选号。
func TestAcquireDisabledNotChosen(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("off", 100, false))
	if _, err := p.Acquire(nil, "m", ""); err == nil {
		t.Fatal("只有禁用账号时应报无可用账号")
	}
}

// TestInflightLimit 单账号在途上限生效。
func TestInflightLimit(t *testing.T) {
	p := New("")
	p.SetOptions(Options{MaxInFlight: 1, PickTopN: 5, Strategy: "weighted",
		SoftRateBase: 600, SoftRateMax: 7200, NotFoundCool: 60, CooldownFallback: 300,
		BreakerThreshold: 5, BreakerBase: 60, BreakerMax: 1800,
		StickyTTL: 1800, StickyMaxEntries: 100})
	p.Add(newTestAccount("a", 100, true))

	first, err := p.Acquire(nil, "m", "")
	if err != nil {
		t.Fatalf("第一次应能选到：%v", err)
	}
	if _, err := p.Acquire(nil, "m", ""); err == nil {
		t.Fatal("在途已满时应报错")
	}
	p.Release(first)
	if _, err := p.Acquire(nil, "m", ""); err != nil {
		t.Fatalf("释放后应能再选到：%v", err)
	}
}

// TestStickyBindsNextAcquire 粘性命中的账号优先被复用。
func TestStickyBindsNextAcquire(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 100, true))
	p.Add(newTestAccount("b", 100, true))
	key := "c-conv-1"
	p.Sticky().Bind(key, "b")

	for i := 0; i < 30; i++ {
		acc, err := p.Acquire(nil, "m", key)
		if err != nil {
			t.Fatalf("选号失败：%v", err)
		}
		if acc.Name != "b" {
			t.Fatalf("粘性命中却选中 %s", acc.Name)
		}
		p.Release(acc)
	}
}

// TestStickyUnbindOnFailure 失败后解绑，下次重新分配。
func TestStickyUnbindOnFailure(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 500, true))
	key := "c-conv-2"
	p.Sticky().Bind(key, "a")
	p.ReportFailure(p.Get("a"), 500, "boom", key, "m")
	if got := p.Sticky().Get(key); got != "" {
		t.Errorf("失败后应解绑，仍绑到 %q", got)
	}
}

// TestModelCooldownScopedToModel 模型级限流只锁该模型，换模型仍可用。
func TestModelCooldownScopedToModel(t *testing.T) {
	a := newTestAccount("a", 100, true)
	a.CoolModel("model-x", 300, "限流")
	if !a.ModelCooled("model-x") {
		t.Error("model-x 应处于冷却")
	}
	if a.ModelCooled("model-y") {
		t.Error("model-y 不应受影响")
	}
	if !a.Healthy() {
		t.Error("模型级冷却不应让账号整体不健康")
	}
	if a.HealthyForModel("model-x") {
		t.Error("model-x 上应判定为不可用")
	}
}

// TestSoftCooldownExponential 429 软冷却逐次加倍并封顶。
func TestSoftCooldownExponential(t *testing.T) {
	a := newTestAccount("a", 100, true)
	base, max := 100.0, 1000.0

	first := a.CoolSoft(base, max, "429", 0) - nowF()
	if first < base*0.9 || first > base*1.5 {
		t.Errorf("首次软冷却约 %v 秒，实际 %.1f", base, first)
	}
	a.NoteSuccess() // 清退避
	second := a.CoolSoft(base, max, "429", 0) - nowF()
	if second < base*0.9 {
		t.Errorf("退避不应低于基数：%.1f", second)
	}

	// 连续命中不得无限增长
	for i := 0; i < 20; i++ {
		a.NoteSuccess()
		got := a.CoolSoft(base, max, "429", 0) - nowF()
		if got > max*1.1 {
			t.Errorf("第 %d 次冷冷却 %.1f 超过封顶 %v", i, got, max)
		}
	}
}

// TestSoftCooldownHonorsResetAt 上游给了重置墙钟时对齐到该时间（截断到封顶）。
func TestSoftCooldownHonorsResetAt(t *testing.T) {
	a := newTestAccount("a", 100, true)
	reset := nowF() + 500
	got := a.CoolSoft(100, 1000, "429", reset)
	if diff := got - reset; diff > 1 || diff < -1 {
		t.Errorf("冷却截止 %.1f，期望对齐到 %.1f", got, reset)
	}

	// 重置时间远大于封顶 → 截断
	a2 := newTestAccount("b", 100, true)
	far := nowF() + 99999
	got2 := a2.CoolSoft(100, 1000, "429", far)
	if got2-nowF() > 1001 {
		t.Errorf("应截断到封顶 1000s，实际 %.1f", got2-nowF())
	}
}

// TestCoolSoftDoubleHitDoesNotExtend 冷却中再次命中不延长（防越重试越冷）。
func TestCoolSoftDoubleHitDoesNotExtend(t *testing.T) {
	a := newTestAccount("a", 100, true)
	first := a.CoolSoft(100, 1000, "429", 0)
	second := a.CoolSoft(100, 1000, "429", 0)
	if second != first {
		t.Errorf("冷却中重复命中不应延长：%.1f vs %.1f", first, second)
	}
}

// TestBreakerTripsAfterThreshold 连续失败达阈值后熔断。
func TestBreakerTripsAfterThreshold(t *testing.T) {
	a := newTestAccount("a", 100, true)
	tripped := false
	for i := 0; i < 3; i++ {
		if a.NoteError(3, 60, 600) {
			tripped = true
		}
	}
	if !tripped {
		t.Fatal("连败 3 次应熔断")
	}
	if !a.InBreaker() {
		t.Error("熔断状态应生效")
	}
	if a.Healthy() {
		t.Error("熔断中的账号不应健康")
	}
	a.NoteSuccess()
	if a.InBreaker() {
		t.Error("成功后应解除熔断")
	}
}

// TestFloorBlocksLowBalance 积分保底生效。
func TestFloorBlocksLowBalance(t *testing.T) {
	p := New("")
	p.SetOptions(Options{BalanceFloor: 100, PickTopN: 5, Strategy: "weighted",
		SoftRateBase: 600, SoftRateMax: 7200, NotFoundCool: 60, CooldownFallback: 300,
		BreakerThreshold: 5, BreakerBase: 60, BreakerMax: 1800,
		StickyTTL: 1800, StickyMaxEntries: 100})
	p.Add(newTestAccount("low", 50, true))
	if _, err := p.Acquire(nil, "m", ""); err == nil {
		t.Fatal("低于保底的账号不应接单")
	}
	p.Add(newTestAccount("rich", 500, true))
	acc, err := p.Acquire(nil, "m", "")
	if err != nil {
		t.Fatalf("高余额账号应能接单：%v", err)
	}
	if acc.Name != "rich" {
		t.Errorf("选中 %s，期望 rich", acc.Name)
	}
}

// TestOverviewCounts 汇总计数正确。
func TestOverviewCounts(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 100, true))
	p.Add(newTestAccount("b", 200, true))
	p.Add(newTestAccount("c", 50, false))
	p.Get("b").Cool(300, CoolSoft, "test")

	ov := p.Overview()
	if ov["total"] != 3 {
		t.Errorf("total = %v，期望 3", ov["total"])
	}
	if ov["healthy"] != 1 {
		t.Errorf("healthy = %v，期望 1", ov["healthy"])
	}
	if ov["cooling"] != 1 {
		t.Errorf("cooling = %v，期望 1", ov["cooling"])
	}
	if ov["disabled"] != 1 {
		t.Errorf("disabled = %v，期望 1", ov["disabled"])
	}
}

// TestClassifyQuota 额度类错误识别为硬冷却。
func TestClassifyQuota(t *testing.T) {
	cases := []struct {
		status int
		body   string
	}{
		{402, `{"error":"payment required"}`},
		{403, `{"error":"积分不足，无法继续"}`},
		{429, `{"error":"insufficient balance"}`},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != FaultQuota {
			t.Errorf("Classify(%d, %q) = %v，期望 FaultQuota", c.status, c.body, got)
		}
	}
}

// TestClassifySession 登录态失效识别。
func TestClassifySession(t *testing.T) {
	if got := Classify(401, `{"error":"unauthorized"}`); got != FaultSession {
		t.Errorf("401 应为 FaultSession，实际 %v", got)
	}
	if got := Classify(400, `{"error":"session not found"}`); got != FaultSession {
		t.Errorf("session not found 应为 FaultSession，实际 %v", got)
	}
}

// TestClassifyModelLimit 模型级限流与账号级频控要能区分。
func TestClassifyModelLimit(t *testing.T) {
	if got := Classify(429, `{"error":"model rate limit exceeded"}`); got != FaultModelLimit {
		t.Errorf("模型级限流应识别为 FaultModelLimit，实际 %v", got)
	}
	if got := Classify(429, `{"error":"too many requests"}`); got != FaultRateLimit {
		t.Errorf("账号级频控应为 FaultRateLimit，实际 %v", got)
	}
}

// TestClassifyServerAndClient 5xx 喂熔断、4xx 不罚账号。
func TestClassifyServerAndClient(t *testing.T) {
	if got := Classify(502, "bad gateway"); got != FaultServer {
		t.Errorf("502 应为 FaultServer，实际 %v", got)
	}
	if got := Classify(400, `{"error":"messages 不能为空"}`); got != FaultClient {
		t.Errorf("400 应为 FaultClient，实际 %v", got)
	}
	if !FatalRequest(FaultClient) {
		t.Error("客户端错误不应换号重试")
	}
	if FatalRequest(FaultQuota) {
		t.Error("额度错误应该换号重试")
	}
}

// TestNextDailyResetInFuture 硬冷却截止时间必须在未来。
func TestNextDailyResetInFuture(t *testing.T) {
	got := NextDailyReset()
	if got <= nowF() {
		t.Errorf("下次重置 %.0f 不在未来（now %.0f）", got, nowF())
	}
	// 不超过 25 小时
	if got-nowF() > 25*3600 {
		t.Errorf("重置时间过远：%.1f 小时后", (got-nowF())/3600)
	}
}

// TestStickySessionKeyUsesLastUser 会话键取最后一条 user（避免多账号退化）。
func TestStickySessionKeyUsesLastUser(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "第一条"},
			map[string]any{"role": "assistant", "content": "回复"},
			map[string]any{"role": "user", "content": "第二条"},
		},
	}
	k1 := SessionKey(payload)
	if k1 == "" {
		t.Fatal("应派生出会话键")
	}
	// 多轮对话换了最后一条 → 键必须变（否则粘性永久锁死同一账号）
	payload["messages"] = append(payload["messages"].([]any),
		map[string]any{"role": "assistant", "content": "再回复"},
		map[string]any{"role": "user", "content": "第三条"},
	)
	k2 := SessionKey(payload)
	if k1 == k2 {
		t.Error("最后一条 user 变化后会话键应变化")
	}
	// 同一条消息重试 → 键不变（粘性只对重试生效）
	if SessionKey(payload) != k2 {
		t.Error("同内容应得到稳定键")
	}
}

// TestStickySessionKeyExplicitID 显式 conversation_id 优先。
func TestStickySessionKeyExplicitID(t *testing.T) {
	payload := map[string]any{
		"conversation_id": "abc",
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if got := SessionKey(payload); got != "c-abc" {
		t.Errorf("显式 ID 应优先，得到 %q", got)
	}
}

// TestStickyTTLExpiry TTL 到期后绑定失效。
func TestStickyTTLExpiry(t *testing.T) {
	s := NewSticky(0.05, 10)
	s.Bind("k", "acc")
	if s.Get("k") != "acc" {
		t.Fatal("刚绑定的应命中")
	}
	time.Sleep(80 * time.Millisecond)
	if s.Get("k") != "" {
		t.Error("过期后不应命中")
	}
}

// TestStickyCapacityEviction 容量满时淘汰最早到期者，新会话仍能绑上。
func TestStickyCapacityEviction(t *testing.T) {
	s := NewSticky(1800, 3)
	for i := 0; i < 10; i++ {
		s.Bind(string(rune('a'+i)), "acc")
	}
	snap := s.Snapshot()
	if snap["entries"].(int) > 3 {
		t.Errorf("条目数 %v 超过容量 3", snap["entries"])
	}
	s.Bind("newest", "acc")
	if s.Get("newest") != "acc" {
		t.Error("新键应能绑上")
	}
}

// TestStickyDisabled ttl=0 时粘性关闭。
func TestStickyDisabled(t *testing.T) {
	s := NewSticky(0, 100)
	s.Bind("k", "acc")
	if s.Get("k") != "" {
		t.Error("ttl=0 时不应命中")
	}
	if s.Enabled() {
		t.Error("ttl=0 时应判定为未启用")
	}
}

// TestPoolStatePersistence 计数与开关能落盘并恢复。
func TestPoolStatePersistence(t *testing.T) {
	dir := t.TempDir()
	stateFile := dir + "/state.json"

	p := New(stateFile)
	p.Add(newTestAccount("a", 100, true))
	p.Get("a").Requests = 42
	p.Get("a").PointsUsed = 1234
	p.Get("a").Failures = 3
	p.Get("a").Enabled = false
	p.Flush()
	p.Close()

	// 新池：先加账号再合并状态
	p2 := New(stateFile)
	defer p2.Close()
	p2.Add(newTestAccount("a", 100, true))
	p2.ApplyState()

	got := p2.Get("a")
	if got.Requests != 42 {
		t.Errorf("requests = %d，期望 42", got.Requests)
	}
	if got.PointsUsed != 1234 {
		t.Errorf("pointsUsed = %d，期望 1234", got.PointsUsed)
	}
	if got.Failures != 3 {
		t.Errorf("failures = %d，期望 3", got.Failures)
	}
	if got.Enabled {
		t.Error("enabled=false 应被恢复")
	}
}

// TestPoolRemove 移除账号后池与列表同步。
func TestPoolRemove(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 100, true))
	p.Add(newTestAccount("b", 100, true))
	p.Remove("a")
	if p.Count() != 1 {
		t.Errorf("移除后数量 %d，期望 1", p.Count())
	}
	if p.Get("a") != nil {
		t.Error("a 应已被移除")
	}
	if p.Get("b") == nil {
		t.Error("b 不应受影响")
	}
}

// TestPoolAddReplacesCredentialPreservingCounters 同名再添加应保留计数。
func TestPoolAddReplacesCredentialPreservingCounters(t *testing.T) {
	p := New("")
	old := newTestAccount("a", 100, true)
	old.Requests = 7
	p.Add(old)

	fresh := &Account{Name: "a", Enabled: true, Session: "new-session",
		ExpireAt: time.Now().Add(time.Hour).Unix()}
	p.Add(fresh)

	got := p.Get("a")
	if got.Session != "new-session" {
		t.Errorf("session 应被更新，实际 %q", got.Session)
	}
	if got.Requests != 7 {
		t.Errorf("计数应保留 7，实际 %d", got.Requests)
	}
}

// TestClassifySessionBodyOnly 上游给 200+错误信封（已改判 502）时，文案里的
// 「登录已失效」仍要判成 FaultSession，才能触发重登而不是当服务器故障。
func TestClassifySessionBodyOnly(t *testing.T) {
	body := `{"code":"100002","desc":"登录已失效，请重新登录","data":{}}`
	if got := Classify(502, body); got != FaultSession {
		t.Errorf("登录已失效应为 FaultSession，实际 %v", got)
	}
}

// TestQuotaSurvivesRestart 额度快照必须落盘：面板重启后积分/签到状态不能归零。
func TestQuotaSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	avail, perm, daily, quota, used := int64(10000), int64(5000), int64(5000), int64(5000), int64(0)
	a := &Account{
		Name: "acc1", LoginID: "13800000000", Enabled: true, Session: "s1",
		ExpireAt:  time.Now().Add(72 * time.Hour).Unix(),
		Available: &avail, Permanent: &perm, DailyBalance: &daily,
		DailyQuota: &quota, DailyUsed: &used, DailyCycleDate: "2026-10-21", QuotaAt: 1792390000,
	}
	if err := SaveAccount(dir, a); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	loaded, err := LoadAccounts(dir)
	if err != nil {
		t.Fatalf("LoadAccounts: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("账号数 %d", len(loaded))
	}
	got := loaded[0]
	if got.Available == nil || *got.Available != 10000 {
		t.Errorf("available 未恢复: %v", got.Available)
	}
	if got.Permanent == nil || *got.Permanent != 5000 {
		t.Errorf("permanent 未恢复: %v", got.Permanent)
	}
	if got.DailyQuota == nil || *got.DailyQuota != 5000 {
		t.Errorf("daily_quota 未恢复: %v", got.DailyQuota)
	}
	if got.DailyCycleDate != "2026-10-21" {
		t.Errorf("daily_cycle_date 未恢复: %q", got.DailyCycleDate)
	}
	if got.QuotaAt != 1792390000 {
		t.Errorf("quota_updated_at 未恢复: %d", got.QuotaAt)
	}
}

// TestRemarkSurvivesRestart 备注属于账号资料：落盘 → 重新加载必须原样回来。
// 它与额度快照是同一条约束——persisted() 是「整体覆盖」，漏字段等于静默清零。
func TestRemarkSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	a := &Account{
		Name: "acc1", Enabled: true, Session: "s1",
		Remark: "主力号（联通）",
	}
	if err := SaveAccount(dir, a); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	loaded, err := LoadAccounts(dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadAccounts: %v / %d", err, len(loaded))
	}
	if loaded[0].Remark != "主力号（联通）" {
		t.Errorf("remark 未恢复: %q", loaded[0].Remark)
	}

	// 再存一次不带备注的（模拟清除）：文件里不能留下空键。
	loaded[0].Remark = ""
	if err := SaveAccount(dir, loaded[0]); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "acc1.json"))
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	if strings.Contains(string(raw), `"remark"`) {
		t.Errorf("清除后不应留下空 remark 键: %s", raw)
	}
}
