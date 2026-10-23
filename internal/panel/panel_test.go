package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsfree/loomy2api/internal/account"
	"github.com/rsfree/loomy2api/internal/config"
	"github.com/rsfree/loomy2api/internal/pool"
	"github.com/rsfree/loomy2api/internal/server"
	"github.com/rsfree/loomy2api/internal/upstream"
	"github.com/rsfree/loomy2api/internal/usage"
)

func newTestPanel(t *testing.T) (*Panel, *pool.Pool) {
	return newTestPanelFull(t, "https://upstream.invalid", "https://account.invalid")
}

func newTestPanelUpstream(t *testing.T, baseURL string) (*Panel, *pool.Pool) {
	return newTestPanelFull(t, baseURL, "https://account.invalid")
}

// newTestPanelFull 同时注入上游与账号服务的假地址（短信登录类测试用）。
func newTestPanelFull(t *testing.T, upstreamURL, accountURL string) (*Panel, *pool.Pool) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.AuthDir = filepath.Join(dir, "auths")
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.LogRequests = false

	p := pool.New("")
	expire := time.Now().Add(7 * 24 * time.Hour).Unix()
	bal := int64(500)
	p.Add(&pool.Account{
		Name: "acc1", LoginID: "13800000000", Password: "pw",
		Enabled: true, Session: "sess-1", ExpireAt: expire, Available: &bal,
	})

	acc := account.NewClient(accountURL, "APP", "ak", "sk", "")
	up := upstream.NewClient(upstreamURL, "0.9.38", "", 5*time.Second)
	ledger := usage.New("", 100)
	models := server.NewModelCache(up)

	pl := New(cfg, p, acc, up, ledger, models, "test-1.0")
	return pl, p
}

// TestIndexServesHTML 面板首页返回 HTML 且带安全头。
func TestIndexServesHTML(t *testing.T) {
	pl, _ := newTestPanel(t)
	req := httptest.NewRequest(http.MethodGet, "/panel/", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Loomy2API") {
		t.Error("页面缺少标题")
	}
	if !strings.Contains(body, `src="app.js"`) {
		t.Error("页面应通过同源脚本加载 app.js（CSP script-src 'self'）")
	}
	// 页面不能有内联脚本或内联事件处理器（否则 CSP 会拦掉）
	if strings.Contains(body, "<script>") {
		t.Error("不应有内联脚本")
	}
	if strings.Contains(body, "onclick=") || strings.Contains(body, "onsubmit=") {
		t.Error("不应有内联事件处理器（CSP 会拦）")
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// TestSecurityHeaders 安全响应头齐全。
func TestSecurityHeaders(t *testing.T) {
	pl, _ := newTestPanel(t)
	req := httptest.NewRequest(http.MethodGet, "/panel/", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	checks := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, want := range checks {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q，期望 %q", k, got, want)
		}
	}
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP 缺少 %q：%s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-eval") {
		t.Error("CSP 不应允许 unsafe-eval")
	}
}

// TestAppScriptServed app.js 与 theme.js 可访问且 MIME 正确。
func TestAppScriptServed(t *testing.T) {
	pl, _ := newTestPanel(t)

	req := httptest.NewRequest(http.MethodGet, "/panel/app.js", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("app.js 状态码 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("app.js Content-Type = %q", ct)
	}
	if len(w.Body.Bytes()) < 1000 {
		t.Errorf("app.js 内容过短: %d 字节", len(w.Body.Bytes()))
	}

	req = httptest.NewRequest(http.MethodGet, "/panel/theme.js", nil)
	w = httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("theme.js 状态码 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("theme.js Content-Type = %q", ct)
	}

	// favicon 也要能取到：缺了它浏览器每次开面板都 404，控制台多一条红字。
	req = httptest.NewRequest(http.MethodGet, "/panel/favicon.svg", nil)
	w = httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("favicon.svg 状态码 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
		t.Errorf("favicon.svg Content-Type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), "<svg") {
		t.Error("favicon.svg 内容不是 SVG")
	}
}

// TestPanelAuthEnforced 配了 api_key 后数据接口必须鉴权。
func TestPanelAuthEnforced(t *testing.T) {
	pl, _ := newTestPanel(t)
	pl.cfg.APIKeys = []string{"sk-panel"}

	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("缺密钥时状态码 %d，期望 401", w.Code)
	}

	// 带上正确密钥应放行
	req = httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer sk-panel")
	w = httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("正确密钥时状态码 %d，期望 200", w.Code)
	}
}

// TestPanelAuthUsesAPIKey 面板与 /v1/* 共用 api_key（参考面板只有一个 key 输入框）。
// 曾自创过独立的 panel_password，导致用户贴 key 被 401。
func TestPanelAuthUsesAPIKey(t *testing.T) {
	pl, _ := newTestPanel(t)
	pl.cfg.APIKeys = []string{"sk-api"}

	for _, tc := range []struct {
		name string
		key  string
		want int
	}{
		{"api_key 放行", "sk-api", http.StatusOK},
		{"错误密钥拒绝", "sk-nope", http.StatusUnauthorized},
		{"空密钥拒绝", "", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
		if tc.key != "" {
			req.Header.Set("Authorization", "Bearer "+tc.key)
		}
		w := httptest.NewRecorder()
		pl.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%s：状态码 %d，期望 %d", tc.name, w.Code, tc.want)
		}
	}
}

// TestOverviewPayload overview 返回账号汇总与用量窗口。
func TestOverviewPayload(t *testing.T) {
	pl, _ := newTestPanel(t)
	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d", w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	if out["version"] != "test-1.0" {
		t.Errorf("version = %v", out["version"])
	}
	if out["total"] != float64(1) {
		t.Errorf("total = %v，期望 1", out["total"])
	}
	if out["usage"] == nil {
		t.Error("缺少 usage 窗口数据")
	}
	if out["hourly"] == nil {
		t.Error("缺少 hourly 趋势数据")
	}
}

// TestListAccountsNoSecrets 账号列表绝不能回显密码或完整 session。
func TestListAccountsNoSecrets(t *testing.T) {
	pl, _ := newTestPanel(t)
	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, `"pw"`) || strings.Contains(body, "sess-1\"") {
		t.Errorf("响应泄露了凭证明文: %s", body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	accounts := out["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("账号数 %d", len(accounts))
	}
	first := accounts[0].(map[string]any)
	if first["has_password"] != true {
		t.Error("has_password 应为 true（只报存在性）")
	}
}

// TestAddAccountRequiresCredential 缺凭证必须报错而不是建个空账号。
func TestAddAccountRequiresCredential(t *testing.T) {
	pl, p := newTestPanel(t)
	body := strings.NewReader(`{"name":"x"}`)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts", body)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["error"] == nil {
		t.Errorf("缺凭证应报错: %s", w.Body.String())
	}
	if p.Get("x") != nil {
		t.Error("不应创建空账号")
	}
}

// TestAddAccountWithSession 直接给 session 也能建账号（不联网）。
func TestAddAccountWithSession(t *testing.T) {
	pl, p := newTestPanel(t)
	body := strings.NewReader(`{"name":"manual","session":"my-session-token"}`)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts", body)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["ok"] != true {
		t.Fatalf("添加失败: %s", w.Body.String())
	}
	acc := p.Get("manual")
	if acc == nil {
		t.Fatal("账号未加入池")
	}
	if acc.Session != "my-session-token" {
		t.Errorf("session 未设置: %q", acc.Session)
	}
	if !acc.SessionValid() {
		t.Error("应设置 14 天有效期")
	}
	// 凭证应落盘
	if _, err := pool.LoadAccounts(pl.cfg.Resolve(pl.cfg.AuthDir)); err != nil {
		t.Errorf("账号目录读取失败: %v", err)
	}
}

// TestDisableEnableAccount 启用/禁用开关。
// TestDisableThenRevive 参考面板的账号行只有「禁用」，重新放回池里走「解冻」
// （revive 同时会置 enabled=true）——这里锁死这条恢复链路。
func TestDisableThenRevive(t *testing.T) {
	pl, p := newTestPanel(t)

	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/acc1/disable", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if p.Get("acc1").Enabled {
		t.Error("禁用未生效")
	}

	req = httptest.NewRequest(http.MethodPost, "/panel/api/accounts/acc1/revive", nil)
	w = httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if !p.Get("acc1").Enabled {
		t.Error("解冻应重新启用账号")
	}
}

// TestReviveClearsCooldown 解冻清掉冷却与熔断。
func TestReviveClearsCooldown(t *testing.T) {
	pl, p := newTestPanel(t)
	acc := p.Get("acc1")
	acc.Cool(600, pool.CoolSoft, "限流")
	acc.CoolModel("m", 600, "模型限流")
	if !acc.InCooldown() {
		t.Fatal("应处于冷却")
	}

	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/acc1/revive", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	if p.Get("acc1").InCooldown() {
		t.Error("解冻后不应还在冷却")
	}
	if p.Get("acc1").ModelCooled("m") {
		t.Error("模型冷却也应被清除")
	}
}

// TestRemoveAccount 移除账号并删除凭证文件。
func TestRemoveAccount(t *testing.T) {
	pl, p := newTestPanel(t)
	// 先落盘一份凭证
	_ = pool.SaveAccount(pl.cfg.Resolve(pl.cfg.AuthDir), p.Get("acc1"))
	before, _ := pool.LoadAccounts(pl.cfg.Resolve(pl.cfg.AuthDir))
	if len(before) != 1 {
		t.Fatalf("凭证文件数 %d", len(before))
	}

	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/acc1/remove", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	if p.Get("acc1") != nil {
		t.Error("账号未从池中移除")
	}
	after, _ := pool.LoadAccounts(pl.cfg.Resolve(pl.cfg.AuthDir))
	if len(after) != 0 {
		t.Errorf("凭证文件未删除: %d", len(after))
	}
}

// TestConfigGetAndSave 配置读写往返（api_keys 明文下发，与参考面板一致）。
func TestConfigGetAndSave(t *testing.T) {
	pl, _ := newTestPanel(t)
	// 让 Save 有地方写（config.Save 写的是 Load 时的路径）
	path := filepath.Join(t.TempDir(), "config.json")
	if err := configSaveSeed(pl.cfg, path); err != nil {
		t.Fatal(err)
	}
	pl.cfg.APIKeys = []string{"sk-abcdefghij"}

	req := httptest.NewRequest(http.MethodGet, "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer sk-abcdefghij")
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	body := w.Body.String()
	// api_keys 明文下发：面板有「显示」按钮、保存要整表回传（参考面板同款）。
	if !strings.Contains(body, "sk-abcdefghij") {
		t.Errorf("配置视图应含明文 api_key: %s", body)
	}

	req = httptest.NewRequest(http.MethodPost, "/panel/api/config",
		strings.NewReader(`{"pick_top_n":9,"strategy":"lru"}`))
	req.Header.Set("Authorization", "Bearer sk-abcdefghij")
	w = httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["error"] != nil {
		t.Fatalf("保存失败: %v", out["error"])
	}
	if pl.cfg.PickTopN != 9 {
		t.Errorf("热生效失败，pick_top_n = %d", pl.cfg.PickTopN)
	}
	// 池参数也应同步热更新
	if pl.pool.Options().PickTopN != 9 {
		t.Errorf("池参数未同步: %d", pl.pool.Options().PickTopN)
	}
}

// configSaveSeed 让配置指向可写文件（模拟真实首启：Load 到具体路径）。
func configSaveSeed(cfg *config.Config, path string) error {
	if err := config.EnsureAt(path); err != nil {
		return err
	}
	loaded, err := config.Load(path)
	if err != nil {
		return err
	}
	*cfg = *loaded
	return nil
}

// TestLogsEndpoint 日志接口返回数组。
func TestLogsEndpoint(t *testing.T) {
	pl, _ := newTestPanel(t)
	// 与 main 同款接线：log 包输出镜像进面板环（Logf 只写 log，不重复入环）
	log.SetOutput(io.MultiWriter(os.Stderr, pl.LogWriter()))
	defer log.SetOutput(os.Stderr)
	pl.Logf("账号 acc1 已解冻（冷却与熔断清零）")
	pl.Logf("| #001 | 20:00:00 | glm |  10 | acc | 100ms | ok |") // 请求表格行 → chat

	req := httptest.NewRequest(http.MethodGet, "/panel/api/logs", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("日志为空")
	}
	first, _ := entries[0].(map[string]any)
	if !strings.HasPrefix(first["text"].(string), "账号 acc1") {
		t.Errorf("text = %v", first["text"])
	}
	// 频道归类：面板动作行 → task，请求表格行 `| #` → chat
	if first["ch"] != ChTask {
		t.Errorf("频道 = %v，期望 %s", first["ch"], ChTask)
	}
	// 按内容找那行请求，而不是假定它在末尾：log 输出是进程级的，其它测试
	// 遗留的 goroutine（全量刷额度 / 签到并发 4 路）可能在这之后往同一个环里
	// 补几行「账号 … 失败」，末尾断言会偶发失败（CI 上真实踩到过）。
	var chatEntry map[string]any
	for _, e := range entries {
		m, _ := e.(map[string]any)
		if txt, _ := m["text"].(string); strings.HasPrefix(txt, "| #001") {
			chatEntry = m
		}
	}
	if chatEntry == nil {
		t.Fatal("没找到请求表格行")
	}
	if chatEntry["ch"] != ChChat {
		t.Errorf("请求行频道 = %v，期望 %s", chatEntry["ch"], ChChat)
	}
}

// TestRequestsEndpoint 请求记录接口可用。
func TestRequestsEndpoint(t *testing.T) {
	pl, _ := newTestPanel(t)
	pl.ledger.Record(usage.Record{
		Time: time.Now().Unix(), Account: "acc1", Model: "m", Status: 200,
		Prompt: 1, Completion: 2,
	})

	req := httptest.NewRequest(http.MethodGet, "/panel/api/requests?limit=10", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	records, _ := out["records"].([]any)
	if len(records) != 1 {
		t.Errorf("记录数 %d，期望 1", len(records))
	}
}

// TestUnknownPanelPath 未定义路径走 404（而非 500）。
func TestUnknownPanelPath(t *testing.T) {
	pl, _ := newTestPanel(t)
	req := httptest.NewRequest(http.MethodGet, "/panel/api/nope", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("状态码 %d，期望 404", w.Code)
	}
}

// TestCheckinAccount 单号签到：调 first-login、写回额度快照、返回口径字段。
func TestCheckinAccount(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "first-login") {
			hit = true
			_, _ = io.WriteString(w, `{"code":"000000","data":{"alreadyProcessed":false,
				"currentBalance":10000,"permanentBalance":5000,"dailyBalance":5000,
				"dailyQuota":5000,"dailyConsumed":0,"dailyCycleDate":"2026-10-21"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":"000000","data":{}}`)
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/acc1/checkin", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	if !hit {
		t.Error("checkin 必须命中 points/first-login")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d body %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	c, _ := out["checkin"].(map[string]any)
	if c == nil || c["already_processed"] != false {
		t.Errorf("checkin 字段不对: %v", out["checkin"])
	}
	if c["daily_quota"] != float64(5000) || c["current_balance"] != float64(10000) {
		t.Errorf("checkin 口径不对: %v", c)
	}
	acc := p.Get("acc1")
	if acc.DailyQuota == nil || *acc.DailyQuota != 5000 || acc.DailyCycleDate != "2026-10-21" {
		t.Errorf("额度快照未写回账号: quota=%v cycle=%q", acc.DailyQuota, acc.DailyCycleDate)
	}
	if acc.Available == nil || *acc.Available != 10000 {
		t.Errorf("可用总额未写回: %v", acc.Available)
	}
}

// TestCheckinAccountUpstreamError 上游 401 时端点应回 502 而不是伪成功。
func TestCheckinAccountUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"100002","desc":"缺少 token"}`)
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/acc1/checkin", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("状态码 %d，期望 502", w.Code)
	}
	if p.Get("acc1").LastError() == "" {
		t.Error("失败原因应记录到 last_error")
	}
}

// TestCheckinAll 全量签到：只处理启用且会话有效的账号。
func TestCheckinAll(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 新手任务全已完成：签到链路里的自动领取只发一次 GET、不领分、不再刷额度，
		// 这样 first-login 的调用次数仍能精确断言。
		if strings.Contains(r.URL.Path, "onboarding/tasks") {
			_, _ = io.WriteString(w, `{"code":"000000","data":{"tasks":{
				"first_message":true,"pick_skill":true,"generate_ppt":true,"set_schedule":true,
				"install_skill":true,"configure_remote":true,"create_soul":true,"share_soul":true},
				"earned":10000,"total":10000}}`)
			return
		}
		if strings.Contains(r.URL.Path, "first-login") {
			hits++
			_, _ = io.WriteString(w, `{"code":"000000","data":{"alreadyProcessed":true,
				"currentBalance":10000,"permanentBalance":5000,"dailyBalance":5000,
				"dailyQuota":5000,"dailyConsumed":0,"dailyCycleDate":"2026-10-21"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":"000000","data":{}}`)
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	expired := time.Now().Add(-time.Hour).Unix()
	p.Add(&pool.Account{Name: "acc2", Enabled: true, Session: "sess-2", ExpireAt: expired})
	p.Add(&pool.Account{Name: "acc3", Enabled: false, Session: "sess-3", ExpireAt: time.Now().Add(72 * time.Hour).Unix()})

	req := httptest.NewRequest(http.MethodPost, "/panel/api/checkin_all", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d body %s", w.Code, w.Body.String())
	}
	if hits != 1 {
		t.Errorf("first-login 应只打 1 次（acc1），实际 %d", hits)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	results, _ := out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("结果数 %d，期望 1（跳过过期与禁用）", len(results))
	}
	item, _ := results[0].(map[string]any)
	if item["name"] != "acc1" || item["ok"] != true || item["granted"] != false {
		t.Errorf("结果项不对: %v", item)
	}
}

// TestSetRemarkPersistsToAccountFile 备注必须落到 auths/<账号>.json 并随 overview 下发：
// 这是「换浏览器 / 换入口还看得见」的唯一保证，也是从 localStorage 迁到后端的核心诉求。
func TestSetRemarkPersistsToAccountFile(t *testing.T) {
	pl, p := newTestPanelFull(t, "http://127.0.0.1:1", "http://127.0.0.1:1")

	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/acc1/remark",
		strings.NewReader(`{"remark":"主力号"}`))
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}

	// 1) 内存里的账号对象立即生效
	if got := p.Get("acc1").Remark; got != "主力号" {
		t.Fatalf("内存备注 = %q", got)
	}
	// 2) 落盘：直接读文件，不依赖进程内状态
	path := filepath.Join(pl.cfg.Resolve(pl.cfg.AuthDir), "acc1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读账号文件失败：%v", err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("账号文件不是合法 JSON：%v", err)
	}
	if onDisk["remark"] != "主力号" {
		t.Fatalf("账号文件 remark = %v", onDisk["remark"])
	}
	// 3) 重启后仍在：按落盘内容重建一份池
	loaded, err := pool.LoadAccounts(pl.cfg.Resolve(pl.cfg.AuthDir))
	if err != nil || len(loaded) != 1 {
		t.Fatalf("重新加载失败：%v / %d 个", err, len(loaded))
	}
	if loaded[0].Remark != "主力号" {
		t.Fatalf("重新加载后备注 = %q", loaded[0].Remark)
	}
	// 4) overview 下发给前端（前端只认这个字段）
	ov := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	ow := httptest.NewRecorder()
	pl.ServeHTTP(ow, ov)
	if !strings.Contains(ow.Body.String(), `"remark":"主力号"`) {
		t.Errorf("overview 未下发 remark：%s", ow.Body.String())
	}
}

// TestRemarkSurvivesQuotaRefresh 回归：SaveAccount 是「快照整体覆盖」，
// 每次刷新额度都会 persist 一次。备注若没进 persisted()，会被静默冲掉。
func TestRemarkSurvivesQuotaRefresh(t *testing.T) {
	pl, p := newTestPanelFull(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	acc := p.Get("acc1")
	acc.Remark = "别丢"
	pl.persist(acc)

	// 模拟刷新额度后的再次落盘（清掉其它字段、只走 persisted 快照）。
	bal := int64(7777)
	acc.Available = &bal
	pl.persist(acc)

	dir := pl.cfg.Resolve(pl.cfg.AuthDir)
	loaded, err := pool.LoadAccounts(dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("重新加载失败：%v", err)
	}
	if loaded[0].Remark != "别丢" {
		t.Fatalf("额度刷新后备注被冲掉了：%q", loaded[0].Remark)
	}
	if loaded[0].Available == nil || *loaded[0].Available != 7777 {
		t.Errorf("额度刷新未落盘：%+v", loaded[0].Available)
	}
}

// TestSetRemarkClearsAndValidates 空串 = 清除（文件里不留空键）；超长 400 且不改动已有值。
func TestSetRemarkClearsAndValidates(t *testing.T) {
	pl, p := newTestPanelFull(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	acc := p.Get("acc1")
	acc.Remark = "旧备注"
	pl.persist(acc)

	tooLong := strings.Repeat("字", 30)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/acc1/remark",
		strings.NewReader(`{"remark":"`+tooLong+`"}`))
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("超长应 400，实际 %d", w.Code)
	}
	if acc.Remark != "旧备注" {
		t.Errorf("被拒绝的请求不该改动备注：%q", acc.Remark)
	}

	req = httptest.NewRequest(http.MethodPost, "/panel/api/accounts/acc1/remark",
		strings.NewReader(`{"remark":"  "}`))
	w = httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("清除应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	if acc.Remark != "" {
		t.Errorf("空白应清除备注，实际 %q", acc.Remark)
	}
	raw, err := os.ReadFile(filepath.Join(pl.cfg.Resolve(pl.cfg.AuthDir), "acc1.json"))
	if err != nil {
		t.Fatalf("读账号文件失败：%v", err)
	}
	if strings.Contains(string(raw), `"remark"`) {
		t.Errorf("清除后不应留下空 remark 键：%s", raw)
	}

	req = httptest.NewRequest(http.MethodPost, "/panel/api/accounts/nope/remark",
		strings.NewReader(`{"remark":"x"}`))
	w = httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("不存在的账号应 404，实际 %d", w.Code)
	}
}

// TestOnboardingEndpoint 面板单号领取：返回本次领到的任务与积分，并把额度快照刷新。
func TestOnboardingEndpoint(t *testing.T) {
	var completes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/onboarding/tasks"):
			_, _ = io.WriteString(w, `{"code":"000000","data":{"tasks":{
				"first_message":false,"pick_skill":false,"generate_ppt":false,"set_schedule":false,
				"install_skill":false,"configure_remote":false,"create_soul":false,"share_soul":false}}}`)
		case strings.HasSuffix(r.URL.Path, "/onboarding/tasks/complete"):
			completes++
			_, _ = io.WriteString(w, `{"code":"000000","data":{"alreadyCompleted":false,"balance":15000}}`)
		case strings.Contains(r.URL.Path, "points/records"):
			_, _ = io.WriteString(w, `{"code":"000000","data":{"balance":5000,"dailyBalance":5000,"availableBalance":20000}}`)
		default:
			_, _ = io.WriteString(w, `{"code":"000000","data":{}}`)
		}
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/acc1/onboarding", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	var out struct {
		Onboarding struct {
			Claimed []string `json:"claimed"`
			Points  int64    `json:"points"`
			Total   int64    `json:"total"`
		} `json:"onboarding"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Onboarding.Claimed) != 8 || out.Onboarding.Points != 10000 {
		t.Errorf("应领满 8 个任务 / 10000 积分，实际 %d / %d", len(out.Onboarding.Claimed), out.Onboarding.Points)
	}
	if out.Onboarding.Total != 10000 {
		t.Errorf("total = %d", out.Onboarding.Total)
	}
	if completes != 8 {
		t.Errorf("complete 调用 %d 次，期望 8", completes)
	}
	_ = p
}

// TestOnboardingAllSkipsInvalid 全量领取跳过禁用 / 登录态失效的账号。
func TestOnboardingAllSkipsInvalid(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/onboarding/tasks") {
			got = append(got, r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"code":"000000","data":{"tasks":{}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":"000000","data":{}}`)
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	p.Add(&pool.Account{Name: "dead", Enabled: true, Session: "s-dead",
		ExpireAt: time.Now().Add(-time.Hour).Unix()})
	p.Add(&pool.Account{Name: "off", Enabled: false, Session: "s-off",
		ExpireAt: time.Now().Add(72 * time.Hour).Unix()})

	req := httptest.NewRequest(http.MethodPost, "/panel/api/onboarding_all", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	if len(got) != 1 || !strings.Contains(got[0], "sess-1") {
		t.Errorf("只该查一次目录（acc1 的会话），实际 %v", got)
	}
}

// TestDeadModelsGloballyHidden 404 之后：模型进台账、/v1/models 与面板模型视图都不再列它，
// 再请求它直接 404 且不打上游。
func TestDeadModelsGloballyHidden(t *testing.T) {
	var chatHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			_, _ = io.WriteString(w, `{"object":"list","data":[
				{"id":"gone-model","type":"chat"},{"id":"spark-x","type":"chat"}]}`)
		case strings.Contains(r.URL.Path, "chat/completions"):
			chatHits++
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"该模型暂未开放，请切换其他模型"}}`)
		default:
			_, _ = io.WriteString(w, `{"code":"000000","data":{}}`)
		}
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	if _, err := p.Acquire(nil, "gone-model", ""); err != nil {
		t.Fatal(err)
	}
	// 模拟一次上游 404（走真实 ReportFailure 路径）
	acc := p.Get("acc1")
	p.ReportFailure(acc, 404, `{"error":{"code":404,"message":"该模型暂未开放"}}`, "", "gone-model")

	if !p.ModelDead("gone-model") {
		t.Fatal("404 后模型应进台账")
	}
	if acc.InCooldown() {
		t.Error("404 不该冷却账号")
	}

	// 面板模型视图
	req := httptest.NewRequest(http.MethodGet, "/panel/api/models", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	var ml struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ml); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range ml.Models {
		ids[m.ID] = true
	}
	if ids["gone-model"] {
		t.Errorf("已下架模型不该出现在模型列表：%v", ids)
	}
	if !ids["spark-x"] {
		t.Errorf("其它模型应照常列出：%v", ids)
	}

	// 台账接口 + 手动解封
	req = httptest.NewRequest(http.MethodGet, "/panel/api/dead_models", nil)
	w = httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "gone-model") {
		t.Errorf("台账应列出该模型：%s", w.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/panel/api/dead_models/gone-model/clear", nil)
	w = httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if p.ModelDead("gone-model") {
		t.Error("手动解封后应不再判失效")
	}
	_ = chatHits
}

// TestOverviewExposesDeadModels overview 要带上台账与选号口径，面板据此渲染。
func TestOverviewExposesDeadModels(t *testing.T) {
	pl, p := newTestPanelFull(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	p.MarkModelDead("gone-model", "该模型暂未开放")

	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "gone-model") {
		t.Errorf("overview 应带 dead_models：%s", body)
	}
	if !strings.Contains(body, "prefer_daily_quota") {
		t.Errorf("overview 应带 prefer_daily_quota：%s", body)
	}
}

// TestCreditHistoryParallelAndCached 积分流水：8 个账号 × 1 页、每次上游 120ms。
// 串行要 0.96s（线上 121 个账号更是一分钟以上，曾把日志页整页拖白），
// 现在并发 12 + 5 分钟缓存：首访应在一秒内返回，二访不再打上游。
func TestCreditHistoryParallelAndCached(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		time.Sleep(120 * time.Millisecond)
		_, _ = io.WriteString(w, `{"code":"000000","data":{"list":[]}}`)
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	expire := time.Now().Add(24 * time.Hour).Unix()
	for i := 2; i <= 8; i++ {
		bal := int64(100)
		p.Add(&pool.Account{
			Name: fmt.Sprintf("acc%d", i), LoginID: fmt.Sprintf("1380000000%d", i),
			Enabled: true, Session: fmt.Sprintf("sess-%d", i), ExpireAt: expire, Available: &bal,
		})
	}

	start := time.Now()
	w := httptest.NewRecorder()
	pl.creditHistoryHandler(w, httptest.NewRequest(http.MethodGet, "/panel/api/credit_history?limit=10", nil))
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	if elapsed > 600*time.Millisecond {
		t.Errorf("并发取数耗时 %v（串行约 0.96s，并发应 <600ms）", elapsed)
	}

	mu.Lock()
	first := hits
	mu.Unlock()
	if first != 8 {
		t.Errorf("上游调用 %d 次，期望 8 账号 × 1 页 = 8", first)
	}

	start = time.Now()
	w2 := httptest.NewRecorder()
	pl.creditHistoryHandler(w2, httptest.NewRequest(http.MethodGet, "/panel/api/credit_history?limit=10", nil))
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("缓存命中应 <100ms，实际 %v", d)
	}
	mu.Lock()
	second := hits
	mu.Unlock()
	if second != first {
		t.Errorf("60s 内重复请求又打了 %d 次上游", second-first)
	}
}

// TestCreditHistoryMarksPartial 有账号取不到流水时仍返回 200，但带 partial=true，
// 让「账本不全」可见而不是安静少几行。
func TestCreditHistoryMarksPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	w := httptest.NewRecorder()
	pl.creditHistoryHandler(w, httptest.NewRequest(http.MethodGet, "/panel/api/credit_history?limit=10", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if out["partial"] != true {
		t.Errorf("上游全失败时应标 partial=true，实际 %v", out["partial"])
	}
}

// TestAutoOnboardingClaimsIncomplete 自动补领只对「缺项」的账号发 complete：
// 已领满的号只多一次 GET（幂等，不重复加分），禁用/暂停的号整轮跳过。
// 这条链路原先不存在——新手任务只在登录与手动签到时领一次，单项失败会让
// 账号永久停在 5/8、7/8（线上任务中心截图里的「部分」）。
func TestAutoOnboardingClaimsIncomplete(t *testing.T) {
	var mu sync.Mutex
	claims := map[string]int{}
	done := func(keys ...string) map[string]bool {
		out := make(map[string]bool, len(upstream.OnboardingPoints))
		for k := range upstream.OnboardingPoints {
			out[k] = false
		}
		for _, k := range keys {
			out[k] = true
		}
		return out
	}
	allDone := done("first_message", "pick_skill", "generate_ppt", "set_schedule",
		"install_skill", "configure_remote", "create_soul", "share_soul")
	state := map[string]map[string]bool{
		"sess-1":    allDone, // newTestPanelUpstream 预置的 acc1
		"sess-full": allDone,
		"sess-part": done("first_message"),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.HasSuffix(r.URL.Path, "/onboarding/tasks/complete") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			key, _ := body["key"].(string)
			mu.Lock()
			claims[sess+"|"+key]++
			mu.Unlock()
			_, _ = io.WriteString(w, `{"code":"000000","data":{"alreadyCompleted":false,"balance":10000}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/onboarding/tasks") {
			tasks := state[sess]
			if tasks == nil {
				tasks = done()
			}
			buf, _ := json.Marshal(map[string]any{"code": "000000", "data": map[string]any{
				"tasks": tasks, "earned": 0, "total": upstream.OnboardingTotal,
			}})
			_, _ = w.Write(buf)
			return
		}
		_, _ = io.WriteString(w, `{"code":"000000","data":{}}`)
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	expire := time.Now().Add(48 * time.Hour).Unix()
	p.Add(&pool.Account{Name: "acc-full", Enabled: true, Session: "sess-full", ExpireAt: expire})
	p.Add(&pool.Account{Name: "acc-part", Enabled: true, Session: "sess-part", ExpireAt: expire})
	p.Add(&pool.Account{Name: "acc-off", Enabled: false, Session: "sess-off", ExpireAt: expire})

	n, pts := pl.AutoOnboarding(context.Background())
	if n != 1 {
		t.Errorf("应只有 1 个账号到账，实际 %d", n)
	}
	want := upstream.OnboardingTotal - upstream.OnboardingPoints["first_message"]
	if pts != want {
		t.Errorf("到账积分 %d，期望 %d（7 个缺项）", pts, want)
	}
	mu.Lock()
	defer mu.Unlock()
	for key := range claims {
		if strings.HasPrefix(key, "sess-full|") {
			t.Errorf("已领满的账号不该再发 complete：%s", key)
		}
		if strings.HasPrefix(key, "sess-off|") {
			t.Errorf("禁用账号不该被领取：%s", key)
		}
	}
	if len(claims) != 7 {
		t.Errorf("应为缺的 7 项各发一次，实际 %d 次", len(claims))
	}
}
