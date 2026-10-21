// deadmodel_test.go 模型级「已下架」台账：404 罚模型不罚账号。
package pool

import (
	"strings"
	"testing"
	"time"
)

func newTestPool() *Pool {
	p := New("")
	p.SetOptions(Options{
		Strategy: "weighted", PickTopN: 5,
		SoftRateBase: 600, SoftRateMax: 7200, NotFoundCool: 60, CooldownFallback: 300,
		BreakerThreshold: 5, BreakerBase: 60, BreakerMax: 1800,
		StickyTTL: 1800, StickyMaxEntries: 100,
	})
	return p
}

// TestNotFoundDoesNotPunishAccount 404 是模型的问题：账号不进冷却、Failures 不涨，
// 模型进台账。旧实现把 404 当账号故障，一个打错模型名的请求能冻掉唯一可用号。
func TestNotFoundDoesNotPunishAccount(t *testing.T) {
	p := newTestPool()
	bal := int64(1000)
	p.Add(&Account{Name: "acc1", Enabled: true, Session: "s1",
		ExpireAt: time.Now().Add(72 * time.Hour).Unix(), Available: &bal})

	acc := p.Get("acc1")
	body := `{"error":{"code":404,"message":"该模型暂未开放，请切换其他模型"}}`
	p.ReportFailure(acc, 404, body, "", "no-such-model")

	if acc.InCooldown() {
		t.Error("404 不该让账号进冷却")
	}
	if acc.Failures != 0 {
		t.Errorf("404 不该计入账号失败数（实际 %d）", acc.Failures)
	}
	if !p.ModelDead("no-such-model") {
		t.Error("模型应进「已下架」台账")
	}
	// 该号仍可被选号（没被牵连）
	if _, err := p.Acquire(nil, "spark-x", ""); err != nil {
		t.Errorf("404 之后账号仍应可被选号: %v", err)
	}
	// 其它模型不受影响
	if p.ModelDead("spark-x") {
		t.Error("未 404 的模型不该被记成失效")
	}
}

// TestDeadModelExpires TTL 到期后自动解除：上游重新上架时最多等一个 TTL 就能再试。
func TestDeadModelExpires(t *testing.T) {
	p := newTestPool()
	p.MarkModelDead("m1", "gone")
	if !p.ModelDead("m1") {
		t.Fatal("刚记的模型应处于失效状态")
	}
	// 手动把到期时间拨到过去（等价于 TTL 耗尽）
	p.deadMu.Lock()
	p.dead["m1"].Until = nowF() - 1
	p.deadMu.Unlock()

	if p.ModelDead("m1") {
		t.Error("过期条目应被即时清掉")
	}
	if list := p.DeadModels(); len(list) != 0 {
		t.Errorf("过期条目不该出现在清单里: %v", list)
	}
}

// TestDeadModelClearAndTTLZero ttl<=0 时不落台账（永久失效无法自愈，只允许显式解封场景）；
// ClearDeadModel 能手动解封。
func TestDeadModelClear(t *testing.T) {
	p := newTestPool()
	p.MarkModelDead("m1", "gone")
	if !p.ClearDeadModel("m1") {
		t.Error("ClearDeadModel 应返回 true")
	}
	if p.ModelDead("m1") {
		t.Error("解封后不该再判失效")
	}
	if p.ClearDeadModel("m1") {
		t.Error("重复解封应返回 false")
	}
}

// TestDeadModelsPersist 台账要落盘：重启后不该把已知失效的模型再列出来。
func TestDeadModelsPersist(t *testing.T) {
	p := newTestPool()
	p.MarkModelDead("gone-model", "该模型暂未开放")
	snap := p.stateSnapshot()
	raw, ok := snap["dead_models"].(map[string]any)
	if !ok || raw["gone-model"] == nil {
		t.Fatalf("stateSnapshot 未带 dead_models: %v", snap["dead_models"])
	}

	p2 := newTestPool()
	p2.restoreDead(map[string]map[string]any{
		"gone-model": {"until": nowF() + 3600, "reason": "x", "hits": float64(2), "at": nowF()},
	})
	if !p2.ModelDead("gone-model") {
		t.Error("恢复后应仍判失效")
	}
	// 过期记录在恢复时直接丢弃
	p3 := newTestPool()
	p3.restoreDead(map[string]map[string]any{
		"old": {"until": nowF() - 1},
	})
	if p3.ModelDead("old") {
		t.Error("过期记录不该恢复")
	}
}

// TestPreferDailyTier 每日赠送额度（自然日清零）优先于永久积分；余额未知垫底。
func TestPreferDailyTier(t *testing.T) {
	daily, permOnly, unknown := int64(4000), int64(0), int64(0)
	_ = daily
	big := int64(900000)

	a := &Account{Name: "daily", DailyBalance: &big, Permanent: &permOnly} // 档 0
	b := &Account{Name: "perm", DailyBalance: &permOnly, Permanent: &big}  // 档 1
	c := &Account{Name: "unknown"}                                         // 档 2
	got := preferDailyTier([]*Account{a, b, c})
	if len(got) != 1 || got[0].Name != "daily" {
		t.Fatalf("应只留档 0 的账号，实际 %v", names(got))
	}

	// 没有档 0 → 取档 1
	got = preferDailyTier([]*Account{b, c})
	if len(got) != 1 || got[0].Name != "perm" {
		t.Fatalf("应退到档 1，实际 %v", names(got))
	}

	// 全未知 → 不分层，保持原样（不能因为查不到余额就无号可用）
	got = preferDailyTier([]*Account{c, {Name: "c2"}})
	if len(got) != 2 {
		t.Fatalf("全未知时应保持原候选集，实际 %v", names(got))
	}
	_ = unknown
}

// TestAcquirePrefersDailyQuota 端到端：两个号余额相同、一个只剩永久积分，
// 请求必须落在有每日赠送额度的那个号上（否则会把每天作废的额度留到浪费）。
func TestAcquirePrefersDailyQuota(t *testing.T) {
	p := newTestPool()
	p.SetOptions(Options{Strategy: "weighted", PickTopN: 2,
		SoftRateBase: 600, SoftRateMax: 7200, NotFoundCool: 60, CooldownFallback: 300,
		BreakerThreshold: 5, BreakerBase: 60, BreakerMax: 1800,
		StickyTTL: 1800, StickyMaxEntries: 100, PreferDaily: true})

	expire := time.Now().Add(72 * time.Hour).Unix()
	daily, zero, perm := int64(5000), int64(0), int64(9000)
	p.Add(&Account{Name: "perm-only", Enabled: true, Session: "s1", ExpireAt: expire,
		DailyBalance: &zero, Permanent: &perm, Available: &perm})
	p.Add(&Account{Name: "has-daily", Enabled: true, Session: "s2", ExpireAt: expire,
		DailyBalance: &daily, Permanent: &perm, Available: &perm})

	for i := 0; i < 40; i++ {
		acc, err := p.Acquire(nil, "spark-x", "")
		if err != nil {
			t.Fatalf("第 %d 次选号失败: %v", i, err)
		}
		if acc.Name != "has-daily" {
			t.Fatalf("第 %d 次选到了 %s：应优先消耗每日赠送额度", i, acc.Name)
		}
		p.Release(acc)
	}
}

// TestPreferDailyCanBeDisabled 关掉开关后回到纯粹的加权选号（两种号都该被抽到）。
func TestPreferDailyCanBeDisabled(t *testing.T) {
	p := newTestPool() // 默认 Options 里 PreferDaily = false
	expire := time.Now().Add(72 * time.Hour).Unix()
	daily, zero, perm := int64(5000), int64(0), int64(9000)
	p.Add(&Account{Name: "perm-only", Enabled: true, Session: "s1", ExpireAt: expire,
		DailyBalance: &zero, Permanent: &perm, Available: &perm})
	p.Add(&Account{Name: "has-daily", Enabled: true, Session: "s2", ExpireAt: expire,
		DailyBalance: &daily, Permanent: &perm, Available: &perm})

	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		acc, err := p.Acquire(nil, "spark-x", "")
		if err != nil {
			t.Fatalf("选号失败: %v", err)
		}
		seen[acc.Name]++
		p.Release(acc)
	}
	if seen["perm-only"] == 0 {
		t.Errorf("关闭 PreferDaily 后两个号都应参与抽签，实际 %v", seen)
	}
}

// TestNextDailyResetIsMidnight 额度耗尽的硬冷却终点必须是次日 00:00（自然日重置），
// 不是 04:00 —— 按 04:00 会让 23:50 烧空额度的号白等 4 小时。
func TestNextDailyResetIsMidnight(t *testing.T) {
	ts := time.Unix(int64(NextDailyReset()), 0)
	if ts.Hour() != 0 || ts.Minute() != 0 {
		t.Fatalf("冷却终点 = %s，期望次日 00:00", ts.Format("2006-01-02 15:04"))
	}
	if !ts.After(time.Now()) || ts.Sub(time.Now()) > 24*time.Hour {
		t.Fatalf("终点必须落在未来 24h 内，实际 %s", ts)
	}
}

// TestFatalRequestStopsOn404 404 不该换号重试（换号也不会好）。
func TestFatalRequestStopsOn404(t *testing.T) {
	if !FatalRequest(FaultNotFound) {
		t.Error("404 应判为不可重试")
	}
	if FatalRequest(FaultRateLimit) {
		t.Error("限流应可换号重试")
	}
	if !FatalRequest(FaultClient) {
		t.Error("客户端错误应判为不可重试")
	}
	_ = strings.TrimSpace
}

func names(list []*Account) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Name)
	}
	return out
}
