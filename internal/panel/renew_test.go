package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rsfree/loomy2api/internal/account"
	"github.com/rsfree/loomy2api/internal/pool"
)

// TestImportExpiry 卡密时间戳按秒/毫秒归一，并以真实签发时间算 14 天。
func TestImportExpiry(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.Local)

	// 毫秒 updatedAt（卡密平台常见形态）：剩余不足 14 天，不能按导入时间重算。
	got, exp := importExpiry(now, 0, 1791893917242, 0)
	if got != 1791893917 {
		t.Errorf("obtainedAt = %d，期望 1791893917", got)
	}
	if exp != 1791893917+account.SessionExpireSeconds {
		t.Errorf("expireAt = %d，期望 +14 天", exp)
	}

	// 秒级 obtainedAt 优先于 updatedAt。
	got, _ = importExpiry(now, 1791000000, 1791893917242, 0)
	if got != 1791000000 {
		t.Errorf("obtainedAt 优先失败：%d", got)
	}

	// 显式 expireAt（毫秒）最优先。
	_, exp = importExpiry(now, 1791000000, 0, 1792000000000)
	if exp != 1792000000 {
		t.Errorf("expireAt 未生效：%d", exp)
	}

	// 都没有时按导入时刻算。
	got, exp = importExpiry(now, 0, 0, 0)
	if got != now.Unix() || exp != now.Unix()+account.SessionExpireSeconds {
		t.Errorf("默认值不对：%d / %d", got, exp)
	}
}

// TestAddAccountAcceptsCardJSON 卡密原样投喂：nickname/phone 兜底命名，
// updatedAt 用来校正剩余有效期（否则 9 天前的卡会被当成刚签发）。
func TestAddAccountAcceptsCardJSON(t *testing.T) {
	pl, p := newTestPanel(t)
	body := `{"platform":"loomy","session":"sess-card","userid":"261013181346158254",
		"phone":"16743289804","nickname":"NK3289804","source":"register-machine",
		"updatedAt":1791893917242}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts", strings.NewReader(body))
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}

	acc := p.Get("NK3289804")
	if acc == nil {
		t.Fatal("账号未加入池（nickname 未用作 name）")
	}
	if acc.LoginID != "16743289804" {
		t.Errorf("phone 未映射成 loginid：%q", acc.LoginID)
	}
	if acc.UserID != "261013181346158254" {
		t.Errorf("userid 未保存：%q", acc.UserID)
	}
	if acc.ObtainedAt != 1791893917 {
		t.Errorf("obtainedAt = %d，期望按 updatedAt 归一", acc.ObtainedAt)
	}
	if want := int64(1791893917 + account.SessionExpireSeconds); acc.ExpireAt != want {
		t.Errorf("expireAt = %d，期望 %d", acc.ExpireAt, want)
	}
}

// TestAutoRenewSkips 自动重登只碰「有密码 + 快到期」的账号。
func TestAutoRenewSkips(t *testing.T) {
	pl, p := newTestPanel(t) // 默认账号 acc1：有密码，剩余 7 天（> 阈值 3）
	soon := time.Now().Add(24 * time.Hour).Unix()
	noPwd := &pool.Account{Name: "session-only", Enabled: true, Session: "s", ExpireAt: soon}
	p.Add(noPwd)

	renewed, failed := pl.AutoRenew(time.Now())
	if renewed != 0 || failed != 0 {
		t.Errorf("不该有续期动作：renewed=%d failed=%d", renewed, failed)
	}

	pl.cfg.AutoRelogin = false
	renewed, failed = pl.AutoRenew(time.Now())
	if renewed != 0 || failed != 0 {
		t.Errorf("关闭后不该续期：renewed=%d failed=%d", renewed, failed)
	}
}

// TestRenewAllowedThrottle 同一账号失败后 6 小时内不重复尝试。
func TestRenewAllowedThrottle(t *testing.T) {
	pl, _ := newTestPanel(t)
	now := time.Now()
	if !pl.renewAllowed("a", now) {
		t.Fatal("首次应允许")
	}
	if pl.renewAllowed("a", now.Add(time.Hour)) {
		t.Error("1 小时内不该重复")
	}
	if !pl.renewAllowed("a", now.Add(7*time.Hour)) {
		t.Error("超过 6 小时应重新允许")
	}
}
