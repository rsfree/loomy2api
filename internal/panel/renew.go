package panel

import (
	"time"

	"github.com/rsfree/loomy2api/internal/account"
)

// renewRetry 自动重登失败后的重试间隔：失败常见于密码被改 / 上游风控，
// 每 5 分钟重试只会刷日志，6 小时足够且不会错过恢复。
const renewRetry = 6 * time.Hour

// importExpiry 决定导入账号的登录态起止时间。
//
// 卡密里的时间字段不统一：`updatedAt` 是毫秒时间戳，也有平台给秒；
// `expireAt` 可能直接给到期时间。这里统一归一成秒：
//
//	obtainedAt > updatedAt > now 作为签发时间，默认 +14 天；
//	显式 expireAt 优先于推算值。
//
// 不按卡密时间校正的话，一张 9 天前签发的卡会被当成「刚签发、还有 14 天」，
// 实际剩余不到 5 天却没有任何告警。
func importExpiry(now time.Time, obtainedAt, updatedAt, expireAt int64) (int64, int64) {
	base := now.Unix()
	if v := normEpoch(obtainedAt); v > 0 {
		base = v
	} else if v := normEpoch(updatedAt); v > 0 {
		base = v
	}
	exp := base + account.SessionExpireSeconds
	if v := normEpoch(expireAt); v > 0 {
		exp = v
	}
	return base, exp
}

// normEpoch 把秒/毫秒时间戳统一成秒；非正数返回 0。
// 阈值按当前量级取 1e11：秒级纪元 ~1.7e9，毫秒级 ~1.7e12。
func normEpoch(v int64) int64 {
	if v <= 0 {
		return 0
	}
	if v > 1e11 {
		return v / 1000
	}
	return v
}

// AutoRenew 给「有密码且登录态快到期」的账号自动重登，把 14 天有效期滚动续上。
//
// 只处理同时有手机号和密码的账号：上游没有 refresh token，纯 session 的卡密号
// 到期只能换卡，没有任何接口能续期。返回成功与失败数量。
func (p *Panel) AutoRenew(now time.Time) (renewed, failed int) {
	if p == nil || p.cfg == nil || !p.cfg.AutoRelogin || p.pool == nil {
		return 0, 0
	}
	days := p.cfg.SessionRenewDays
	if days <= 0 {
		days = 3
	}
	for _, a := range p.pool.All() {
		if !a.Enabled || a.Paused || a.LoginID == "" || a.Password == "" {
			continue
		}
		// DaysLeft 为负表示已过期，也要尝试续期。
		if left := a.DaysLeft(); left >= float64(days) {
			continue
		}
		if !p.renewAllowed(a.Name, now) {
			continue
		}
		if !p.tryLock(a.Name) {
			continue // 该账号正在被其它运维动作使用，下一轮再说
		}
		left := a.DaysLeft()
		err := p.doLogin(a)
		if err != nil {
			a.SetLastError("自动重登失败: " + err.Error())
			p.Logf("账号 %s 自动重登失败（剩余 %.1f 天）：%v；%s 后重试",
				a.Name, left, err, renewRetry)
			failed++
		} else {
			a.Enabled = true
			a.ResetCooldowns()
			p.Logf("账号 %s 登录态已自动续期（剩余 %.1f 天 → 14 天）", a.Name, left)
			renewed++
		}
		p.persist(a)
		p.unlock(a.Name)
		if err == nil {
			go p.checkinAndRefresh(a)
		}
	}
	return renewed, failed
}

// renewAllowed 同一账号在 renewRetry 内只尝试一次，避免反复失败刷日志。
func (p *Panel) renewAllowed(name string, now time.Time) bool {
	p.renewMu.Lock()
	defer p.renewMu.Unlock()
	if p.renewTried == nil {
		p.renewTried = map[string]time.Time{}
	}
	if last, ok := p.renewTried[name]; ok && now.Sub(last) < renewRetry {
		return false
	}
	p.renewTried[name] = now
	return true
}
