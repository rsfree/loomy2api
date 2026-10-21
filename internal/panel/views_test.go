package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsfree/loomy2api/internal/config"
	"github.com/rsfree/loomy2api/internal/usage"
)

// fakeUpstream 起一个复刻 Loomy 抓包形态的假上游：records / first-login / models。
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.Contains(p, "points/records"):
			// createdAt 是数字 unix 秒（抓包样本 ledgerId pl_…）。
			_, _ = io.WriteString(w, `{"code":"000000","data":{"balance":5000,"dailyBalance":5000,`+
				`"availableBalance":10000,"pageNo":1,"pageSize":100,"list":[`+
				`{"ledgerId":"pl_1","direction":"credit","description":"注册奖励",`+
				`"createdAt":1792389369,"pointsActual":5000,"consumeSource":"",`+
				`"balanceBefore":5000,"balanceAfter":10000,"dailyCycleDate":"2026-10-19"}]}}`)
		case strings.Contains(p, "first-login"):
			_, _ = io.WriteString(w, `{"code":"000000","data":{"alreadyProcessed":false,`+
				`"currentBalance":10000,"permanentBalance":5000,"dailyBalance":5000,`+
				`"dailyQuota":5000,"dailyConsumed":0,"dailyCycleDate":"2026-10-21",`+
				`"invitationCodes":[{"inviteCode":"FWF6D5","usedCount":0,"maxUses":1000,"status":"active"}]}}`)
		default:
			_, _ = io.WriteString(w, `{"code":"000000","data":{}}`)
		}
	}))
}

// TestPackagesHandler 积分构成：额度折算成包 + 来源归并 + 邀请码 + 签到字段。
func TestPackagesHandler(t *testing.T) {
	srv := fakeUpstream(t)
	defer srv.Close()
	pl, _ := newTestPanelUpstream(t, srv.URL)

	req := httptest.NewRequest(http.MethodGet, "/panel/api/packages", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d body %s", w.Code, w.Body.String())
	}
	var out struct {
		Accounts []struct {
			UID             string           `json:"uid"`
			Remain          int64            `json:"remain"`
			Size            int64            `json:"size"`
			Packages        []map[string]any `json:"packages"`
			InvitationCodes []map[string]any `json:"invitation_codes"`
			DailyCycleDate  string           `json:"daily_cycle_date"`
			CheckinDone     bool             `json:"checkin_done"`
			Error           string           `json:"error"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("账号数 %d，期望 1", len(out.Accounts))
	}
	a := out.Accounts[0]
	if a.Error != "" {
		t.Fatalf("查询失败: %s", a.Error)
	}
	// 可用总额取 availableBalance = 10000。
	if a.Remain != 10000 {
		t.Errorf("remain=%d 期望 10000", a.Remain)
	}
	// 至少应有「每日赠送」与「长期积分」两个口径包。
	names := map[string]bool{}
	for _, p := range a.Packages {
		names[p["name"].(string)] = true
	}
	if !names["每日赠送"] || !names["长期积分"] {
		t.Errorf("包口径不全: %v", names)
	}
	if len(a.InvitationCodes) != 1 || a.InvitationCodes[0]["inviteCode"] != "FWF6D5" {
		t.Errorf("邀请码没带出来: %v", a.InvitationCodes)
	}
	if a.DailyCycleDate != "2026-10-21" {
		t.Errorf("周期日=%q", a.DailyCycleDate)
	}
	if a.CheckinDone != (time.Now().Format("2006-01-02") == "2026-10-21") {
		// checkin_done 只在周期日==今天时为真；假上游固定 2026-10-21，通常应为 false。
		t.Logf("checkin_done=%v（周期日非今天时应为 false）", a.CheckinDone)
	}
}

// TestCreditHistoryHandler 积分历史：流水账本，入账 delta 为正、含前后余额。
func TestCreditHistoryHandler(t *testing.T) {
	srv := fakeUpstream(t)
	defer srv.Close()
	pl, _ := newTestPanelUpstream(t, srv.URL)

	req := httptest.NewRequest(http.MethodGet, "/panel/api/credit_history?limit=100", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d body %s", w.Code, w.Body.String())
	}
	var out struct {
		Entries []struct {
			Time    string `json:"time"`
			Account string `json:"account"`
			Delta   int64  `json:"delta"`
			Before  int64  `json:"before"`
			After   int64  `json:"after"`
			Desc    string `json:"desc"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("不是 JSON: %v body=%s", err, w.Body.String())
	}
	if len(out.Entries) != 1 {
		t.Fatalf("流水条数 %d，期望 1", len(out.Entries))
	}
	e := out.Entries[0]
	if e.Account != "acc1" {
		t.Errorf("账号=%q", e.Account)
	}
	// 注册奖励是 credit → delta 正 5000，after=10000、before=5000。
	if e.Delta != 5000 || e.After != 10000 || e.Before != 5000 {
		t.Errorf("delta/before/after = %d/%d/%d，期望 5000/5000/10000", e.Delta, e.Before, e.After)
	}
	if e.Desc != "注册奖励" {
		t.Errorf("说明=%q", e.Desc)
	}
	if _, err := time.Parse(time.RFC3339, e.Time); err != nil {
		t.Errorf("time 不是 RFC3339: %q", e.Time)
	}
}

// TestPauseResume 暂停选号后 Healthy 变假、overview 计数进 paused 档；恢复后回可用。
func TestPauseResume(t *testing.T) {
	pl, p := newTestPanel(t)

	do := func(method, path string) map[string]any {
		req := httptest.NewRequest(method, path, nil)
		w := httptest.NewRecorder()
		pl.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s → %d body %s", method, path, w.Code, w.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}

	// 暂停前：healthy=1。
	if ov := p.Overview(); ov["healthy"] != 1 {
		t.Fatalf("初始 healthy 应为 1：%v", ov["healthy"])
	}
	do(http.MethodPost, "/panel/api/accounts/acc1/pause")
	if !p.Get("acc1").Paused {
		t.Fatal("pause 后 Paused 应为 true")
	}
	if ov := p.Overview(); ov["paused"] != 1 || ov["healthy"] != 0 {
		t.Errorf("pause 后 overview paused/healthy = %v/%v，期望 1/0", ov["paused"], ov["healthy"])
	}

	do(http.MethodPost, "/panel/api/accounts/acc1/resume")
	if p.Get("acc1").Paused {
		t.Fatal("resume 后 Paused 应为 false")
	}
	if ov := p.Overview(); ov["paused"] != 0 || ov["healthy"] != 1 {
		t.Errorf("resume 后 overview paused/healthy = %v/%v，期望 0/1", ov["paused"], ov["healthy"])
	}
}

// TestSmsLoginFlow 验证码登录：send 提取 msg_id → login 写回账号并保留身份。
func TestSmsLoginFlow(t *testing.T) {
	var sendHit, loginHit bool
	acctSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "sendMsgCode"):
			sendHit = true
			_, _ = io.WriteString(w, `{"code":"000000","data":{"msgId":"MSG-123"}}`)
		case strings.Contains(r.URL.Path, "checkCode"):
			loginHit = true
			_, _ = io.WriteString(w, `{"code":"000000","data":{"session":"sess-sms","userid":"U9"}}`)
		default:
			_, _ = io.WriteString(w, `{"code":"000000","data":{}}`)
		}
	}))
	defer acctSrv.Close()

	pl, p := newTestPanelFull(t, "https://upstream.invalid", acctSrv.URL)

	// send
	sb, _ := json.Marshal(map[string]any{"name": "", "phone": "15600007692"})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/sms/send", strings.NewReader(string(sb)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if !sendHit || w.Code != http.StatusOK {
		t.Fatalf("send 未命中或状态码错：%d %s", w.Code, w.Body.String())
	}
	var sendOut struct {
		OK    bool   `json:"ok"`
		MsgID string `json:"msg_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sendOut)
	if sendOut.MsgID != "MSG-123" {
		t.Fatalf("msg_id=%q 期望 MSG-123", sendOut.MsgID)
	}

	// login（上游 first-login 走 upstream.invalid，异步签到会失败但不阻断登录写回）
	lb, _ := json.Marshal(map[string]any{"name": "sms-01", "phone": "15600007692", "code": "123456", "msg_id": "MSG-123"})
	req = httptest.NewRequest(http.MethodPost, "/panel/api/sms/login", strings.NewReader(string(lb)))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if !loginHit || w.Code != http.StatusOK {
		t.Fatalf("login 未命中或状态码错：%d %s", w.Code, w.Body.String())
	}
	acc := p.Get("sms-01")
	if acc == nil || acc.Session != "sess-sms" || acc.UserID != "U9" {
		t.Fatalf("短信登录未写回账号: %+v", acc)
	}
	if acc.Paused || !acc.Enabled {
		t.Errorf("登录应清暂停并保持启用：paused=%v enabled=%v", acc.Paused, acc.Enabled)
	}
}

// TestOverviewDecorated /overview 带粘性计数、运行时长、模型锁池、账号行。
func TestOverviewDecorated(t *testing.T) {
	pl, _ := newTestPanel(t)
	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	var ov map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	if _, ok := ov["sticky_sessions"]; !ok {
		t.Error("缺 sticky_sessions")
	}
	if ov["uptime_sec"] == nil {
		t.Error("缺 uptime_sec")
	}
	// 无模型级限流时 model_locks 为 nil（JSON null）。
	ml, present := ov["model_locks"]
	if !present {
		t.Error("缺 model_locks 键")
	} else if ml != nil {
		t.Errorf("无锁时 model_locks 应为 null，实际 %v", ml)
	}
	accounts, _ := ov["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts 行数 %d 期望 1", len(accounts))
	}
	row, _ := accounts[0].(map[string]any)
	for _, k := range []string{"uid", "nickname", "disabled", "paused", "checkin_done"} {
		if _, ok := row[k]; !ok {
			t.Errorf("账号行缺 %q", k)
		}
	}
}

// TestUsageHoursZeroAllHistory usage 的 hours=0（全部历史）不能静默退回 24h。
func TestUsageHoursZeroAllHistory(t *testing.T) {
	pl, _ := newTestPanel(t)
	// 造一条 10 天前的记录。
	old := time.Now().Add(-10 * 24 * time.Hour).Unix()
	pl.ledger.Record(usage.Record{Time: old, Account: "acc1", Model: "m", Status: 200, Prompt: 10, Completion: 5})
	req := httptest.NewRequest(http.MethodGet, "/panel/api/usage?hours=0", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	var out struct {
		Hourly []map[string]any `json:"hourly"`
		Window map[string]any   `json:"window"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Window["total"] != float64(1) {
		t.Fatalf("全部历史窗口应含 1 条，实际 %v", out.Window["total"])
	}
}

// TestSaveConfigSetAPIKey 面板配置页是单个 API 密钥输入框（参考面板同款），
// 保存时单值覆盖 api_keys 并落盘。
func TestSaveConfigSetAPIKey(t *testing.T) {
	pl, _ := newTestPanel(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := configSaveSeed(pl.cfg, path); err != nil {
		t.Fatal(err)
	}
	pl.cfg.APIKeys = []string{"sk-existing-0001"}
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config",
		strings.NewReader(`{"api_keys":"sk-newkey123456"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-existing-0001")
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d body %s", w.Code, w.Body.String())
	}
	if len(pl.cfg.APIKeys) != 1 || pl.cfg.APIKeys[0] != "sk-newkey123456" {
		t.Errorf("api_keys 未被覆盖: %v", pl.cfg.APIKeys)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("重新加载配置失败: %v", err)
	}
	if len(reloaded.APIKeys) != 1 || reloaded.APIKeys[0] != "sk-newkey123456" {
		t.Errorf("磁盘上的 api_keys = %v", reloaded.APIKeys)
	}
}
