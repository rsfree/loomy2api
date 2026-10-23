// Package panel 内嵌 Web 管理面板。
//
// 前端资源经 go:embed 打进二进制（无外部构建步骤），路由挂在 /panel/ 下，
// 数据接口在 /panel/api/*（与网关同一套 Bearer 鉴权，可用 api_key 脚本化调用）。
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/loomy2api/internal/account"
	"github.com/rsfree/loomy2api/internal/config"
	"github.com/rsfree/loomy2api/internal/egress"
	"github.com/rsfree/loomy2api/internal/httpauth"
	"github.com/rsfree/loomy2api/internal/pool"
	"github.com/rsfree/loomy2api/internal/server"
	"github.com/rsfree/loomy2api/internal/upstream"
	"github.com/rsfree/loomy2api/internal/usage"
)

// Panel 管理面板。
type Panel struct {
	cfg      *config.Config
	pool     *pool.Pool
	accounts *account.Client
	upstream *upstream.Client
	ledger   *usage.Ledger
	models   *server.ModelCache
	egress   *egress.Resolver

	mux *http.ServeMux

	ring *Ring

	version string

	// 账号级互斥：同一账号不允许并发跑登录/刷新等运维动作。
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex

	// 自动重登的失败节流：同一账号 renewRetry 内只尝试一次。
	renewMu    sync.Mutex
	renewTried map[string]time.Time

	// 积分流水缓存。121 个账号 × 2 页流水 = 242 次上游调用，串行要一分钟以上；
	// 面板「运行日志」页把它和日志放进同一个 Promise.all，慢一次整页就是空白
	// （2026-10-23 线上事故：日志页永远「暂无日志」）。现在并行取 + 缓存 60s。
	creditMu   sync.Mutex
	creditAt   time.Time
	creditAll  []creditEntry
	creditPart bool

	// 新手任务自动补领的最近完成时间（任务中心展示 + 判断间隔）。
	onboardMu   sync.Mutex
	onboardLast time.Time

	// 邀请码批量绑定任务（同一时刻只允许一个在跑，进度供前端轮询）。
	inviteMu  sync.Mutex
	inviteJob *inviteJob

	started time.Time
}

// New 构造面板。
func New(cfg *config.Config, p *pool.Pool, acc *account.Client, up *upstream.Client,
	ledger *usage.Ledger, models *server.ModelCache, version string) *Panel {
	pl := &Panel{
		cfg: cfg, pool: p, accounts: acc, upstream: up,
		ledger: ledger, models: models, version: version,
		mux:     http.NewServeMux(),
		ring:    NewRing(500),
		locks:   map[string]*sync.Mutex{},
		started: time.Now(),
	}
	pl.routes()
	return pl
}

// SetEgress 注入出口分配（面板展示「哪个账号走哪个出口」，也是均匀度的自证）。
func (p *Panel) SetEgress(r *egress.Resolver) { p.egress = r }

// LogWriter 暴露环的 io.Writer：main 用 log.SetOutput(io.MultiWriter(os.Stderr, pl.LogWriter()))
// 把 log 包输出（含网关请求表格行）镜像进面板「运行日志」。
func (p *Panel) LogWriter() io.Writer { return p.ring }

// Logf 记录一行面板日志。只写 log，环由 main 装的 MultiWriter 收——
// 两边都写会让每条日志在「运行日志」里出现两次。
func (p *Panel) Logf(format string, args ...any) {
	log.Print(fmt.Sprintf(format, args...))
}

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/theme.js", p.themeScript)
	p.mux.HandleFunc("GET /panel/favicon.svg", p.favicon)

	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("POST /panel/api/accounts", p.withAuth(p.addAccount))
	p.mux.HandleFunc("POST /panel/api/accounts/{name}/login", p.withAuth(p.loginAccount))
	p.mux.HandleFunc("POST /panel/api/accounts/{name}/checkin", p.withAuth(p.checkinAccount))
	p.mux.HandleFunc("POST /panel/api/accounts/{name}/balance", p.withAuth(p.refreshAccount))
	p.mux.HandleFunc("POST /panel/api/accounts/{name}/refresh", p.withAuth(p.refreshAccount))
	p.mux.HandleFunc("POST /panel/api/accounts/{name}/pause", p.withAuth(p.pauseAccount))
	p.mux.HandleFunc("POST /panel/api/accounts/{name}/resume", p.withAuth(p.resumeAccount))
	p.mux.HandleFunc("POST /panel/api/accounts/{name}/disable", p.withAuth(p.disableAccount))
	p.mux.HandleFunc("POST /panel/api/accounts/{name}/revive", p.withAuth(p.reviveAccount))
	p.mux.HandleFunc("POST /panel/api/accounts/{name}/remove", p.withAuth(p.removeAccount))
	p.mux.HandleFunc("POST /panel/api/accounts/{name}/remark", p.withAuth(p.setRemark))

	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.refreshAll))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{name}/onboarding", p.withAuth(p.onboardingAccount))
	p.mux.HandleFunc("POST /panel/api/onboarding_all", p.withAuth(p.onboardingAll))
	p.mux.HandleFunc("GET /panel/api/onboarding", p.withAuth(p.onboardingMatrix))
	p.mux.HandleFunc("GET /panel/api/invites", p.withAuth(p.invitesHandler))
	p.mux.HandleFunc("POST /panel/api/invites/bind", p.withAuth(p.inviteBindHandler))
	p.mux.HandleFunc("GET /panel/api/invites/progress", p.withAuth(p.inviteProgressHandler))
	p.mux.HandleFunc("GET /panel/api/dead_models", p.withAuth(p.deadModelsHandler))
	p.mux.HandleFunc("POST /panel/api/dead_models/{model}/clear", p.withAuth(p.clearDeadModel))

	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.modelsHandler))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usageHandler))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packagesHandler))
	p.mux.HandleFunc("GET /panel/api/credit_history", p.withAuth(p.creditHistoryHandler))
	p.mux.HandleFunc("POST /panel/api/sms/send", p.withAuth(p.smsSendHandler))
	p.mux.HandleFunc("POST /panel/api/sms/login", p.withAuth(p.smsLoginHandler))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/error_summary", p.withAuth(p.errorSummaryHandler))
	p.mux.HandleFunc("GET /panel/api/requests", p.withAuth(p.requests))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
}

// ServeHTTP 挂载到 /panel/。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// withAuth 鉴权包装。面板与 /v1/* 用同一把 api_key：参考面板只有一个 key 输入框，
// 不另设面板密码（曾自创过 panel_password，结果用户贴 key 被拒）。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.Verify(r, p.cfg.APIKeys) {
			writeErr(w, http.StatusUnauthorized, "invalid key")
			return
		}
		next(w, r)
	}
}

// ------------------------------------------------------------------ 视图

func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	ov := p.pool.Overview()
	ov["version"] = p.version
	ov["api_key_set"] = len(p.cfg.APIKeys) > 0
	ov["listen"] = p.cfg.Listen
	ov["upstream"] = p.cfg.Upstream
	ov["default_model"] = p.cfg.DefaultModel
	// 蓝本对齐字段：粘性会话计数 / 运行时长 / 模型锁池 / 账号列表（含每号 24h 用量）。
	ov["sticky_sessions"] = p.pool.Sticky().Snapshot()["entries"]
	ov["uptime_sec"] = int(time.Since(p.started).Seconds())
	ov["model_locks"] = p.pool.ModelLockView()
	ov["dead_models"] = p.pool.DeadModels()
	ov["prefer_daily_quota"] = p.cfg.PreferDaily
	ov["accounts"] = p.accountRows()
	ov["egress"] = p.egressSummary()

	windows := map[string]int64{"24h": 86400, "today": todaySeconds(), "3d": 3 * 86400, "7d": 7 * 86400, "30d": 30 * 86400}
	sum := p.ledger.Summary(windows)
	ov["usage"] = sum
	ov["hourly"] = p.ledger.Hourly(24)
	writeJSON(w, http.StatusOK, ov)
}

// egressSummary 出口分配概览：几个 IP、每个 IP 背多少账号、分布范围。
// 「尽量均匀」是这一版的产品要求，所以把 min/max 直接回给面板自证。
func (p *Panel) egressSummary() map[string]any {
	if p.egress == nil || !p.egress.Enabled() {
		return map[string]any{"enabled": false}
	}
	stats := p.egress.Stats()
	lo, hi, first := 0, 0, true
	for _, n := range stats {
		if first || n < lo {
			lo = n
		}
		if first || n > hi {
			hi = n
		}
		first = false
	}
	return map[string]any{
		"enabled":    true,
		"mode":       p.egress.Mode(),
		"ips":        len(p.egress.LocalIPs()),
		"assigned":   p.egress.Assigned(),
		"min_per_ip": lo,
		"max_per_ip": hi,
		"stats":      stats,
	}
}

// accountRows 账号行 + 台账统计。参考面板的「用量」是累计口径（不是某个窗口），
// 所以这里取全历史窗口；「最近成功」也直接来自同一个桶，避免再多扫一遍台账。
func (p *Panel) accountRows() []map[string]any {
	all := p.ledger.Summary(map[string]int64{"all": 0})["all"]
	rows := make([]map[string]any, 0, p.pool.Count())
	for _, a := range p.pool.All() {
		pub := a.Public()
		if b := all.ByAccount[a.Name]; b != nil {
			tokens := b.Prompt + b.Completion
			tps := float64(0)
			if b.AvgLatencyMs > 0 {
				tps = float64(tokens) / (float64(b.AvgLatencyMs) / 1000)
			}
			pub["token_usage"] = map[string]any{
				"request_count":          b.Requests,
				"total_tokens":           tokens,
				"last_latency_ms":        b.AvgLatencyMs,
				"last_tokens_per_second": math.Round(tps*10) / 10,
			}
			pub["success_count"] = b.OK
			pub["err_total"] = b.Fail
			pub["last_success"] = b.LastOK
		}
		if p.egress != nil {
			pub["egress"] = p.egress.IPString(a.Name)
		}
		rows = append(rows, pub)
	}
	return rows
}

func (p *Panel) modelsHandler(w http.ResponseWriter, r *http.Request) {
	list := p.models.Get(r.Context(), p.anySession())
	// 已判定下架的模型不进列表：它们在面板里点不出可用请求，只会在「不可用」区块出现。
	kept := list[:0]
	for _, m := range list {
		if !p.pool.ModelDead(m.ID) {
			kept = append(kept, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": kept, "dead_models": p.pool.DeadModels()})
}

func (p *Panel) usageHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := parseRange(q.Get("from"), q.Get("to"))
	hoursReq := 0
	hoursSet := false
	if v := q.Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 24*30 {
			hoursReq, hoursSet = n, true
		}
	}
	if to == 0 {
		to = time.Now().Unix()
	}
	// 「全部历史」（hours=0）：窗口与时序都走显式区间 [0,now]——若落到默认分支
	// 会静默退回 7d/24h（蓝本 issue #121 的同族坑：空查询≠全部）。
	allHistory := hoursSet && hoursReq == 0
	var win *usage.Window
	switch {
	case from > 0 || allHistory:
		win = p.ledger.Range(from, to)
	default:
		win = p.ledger.Summary(map[string]int64{"7d": 7 * 86400})["7d"]
	}
	// 时序桶：显式区间走 HourlyRange（柱子按用户选的起点排列，「今天」从 00:00
	// 开始）；其余滚动窗口走 Hourly（服务端整点对齐，与旧行为逐位一致）。
	var series []map[string]any
	switch {
	case from > 0 || allHistory:
		series = p.ledger.HourlyRange(from, to)
	default:
		hours := 24
		if hoursSet && hoursReq > 0 {
			hours = hoursReq
		}
		series = p.ledger.Hourly(hours)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window": win,
		"hourly": series,
		"models": p.ledger.Models(),
	})
}

func (p *Panel) requests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	from, to := parseRange(q.Get("from"), q.Get("to"))
	filter := usage.RecordFilter{
		From: from, To: to,
		Keyword: q.Get("q"),
		Result:  q.Get("result"),
		Model:   q.Get("model"),
		Account: q.Get("account"),
		// 来源 / 出口两个方向都能筛：排查「某个出口上的请求全挂了」时
		// ?egress=154.40.45.167 比翻日志快。
		ClientIP: q.Get("client_ip"),
		Egress:   q.Get("egress"),
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": p.ledger.Recent(limit, filter)})
}

func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.ring.Snapshot()})
}

// errorSummaryHandler GET /panel/api/error_summary（默认窗口 24h）。
func (p *Panel) errorSummaryHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, _ := parseRange(q.Get("from"), q.Get("to"))
	if from == 0 {
		from = time.Now().Unix() - 86400
	}
	writeJSON(w, http.StatusOK, map[string]any{"errors": p.errorSummary(from)})
}

func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"config": p.cfg.Public(),
		"path":   p.cfg.Path(),
	})
}

func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体必须是对象: "+err.Error())
		return
	}
	// api_keys 与参考面板同款：面板拿到的是明文列表，改完整体回传即可。
	if v, ok := patch["api_keys"].(string); ok {
		// 单值输入框：空 = 不修改，非空 = 覆盖为这一把。
		if s := strings.TrimSpace(v); s != "" {
			patch["api_keys"] = []string{s}
		} else {
			delete(patch, "api_keys")
		}
	}
	restart, err := p.cfg.Save(patch)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 池参数热生效
	p.pool.SetOptions(poolOptions(p.cfg))
	p.Logf("配置已保存（%d 项）", len(patch))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "restart_required": restart,
		"message": restartMsg(restart),
	})
}

// ------------------------------------------------------------------ 账号运维

func (p *Panel) addAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		LoginID  string `json:"loginid"`
		Password string `json:"password"`
		Session  string `json:"session"`
		Proxy    string `json:"proxy"`
		Login    bool   `json:"login"`
		// 以下字段兼容卡密原样投喂：平台常给 phone / nickname / userid 与
		// updatedAt（毫秒），没有 name/loginid 时自动兜底，时间用来校正剩余有效期。
		Phone      string `json:"phone"`
		Nickname   string `json:"nickname"`
		UserID     string `json:"userid"`
		ObtainedAt int64  `json:"obtainedAt"`
		UpdatedAt  int64  `json:"updatedAt"`
		ExpireAt   int64  `json:"expireAt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	body.LoginID = strings.TrimSpace(body.LoginID)
	if body.LoginID == "" {
		body.LoginID = strings.TrimSpace(body.Phone)
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		body.Name = strings.TrimSpace(body.Nickname)
	}
	if body.Name == "" {
		body.Name = body.LoginID
	}
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name / loginid 至少填一个")
		return
	}
	if body.LoginID == "" && body.Session == "" {
		writeErr(w, http.StatusBadRequest, "需要 loginid + password，或直接给 session")
		return
	}

	acc := &pool.Account{
		Name:     body.Name,
		LoginID:  body.LoginID,
		UserID:   strings.TrimSpace(body.UserID),
		Password: body.Password,
		Proxy:    body.Proxy,
		Enabled:  true,
		Identity: account.NewIdentity(),
	}
	if body.Session != "" {
		acc.Session = body.Session
		acc.ObtainedAt, acc.ExpireAt = importExpiry(time.Now(), body.ObtainedAt, body.UpdatedAt, body.ExpireAt)
		if acc.ExpireAt <= time.Now().Unix() {
			p.Logf("账号 %s 导入的登录态已过期（到期 %s），请尽快换卡或改用密码登录",
				acc.Name, time.Unix(acc.ExpireAt, 0).Format("2006-01-02 15:04"))
		}
	}
	p.pool.Add(acc)

	// 有手机号 + 密码就立刻登录一次，验证凭证并拿 session。
	if body.Login && body.LoginID != "" && body.Password != "" {
		if err := p.doLogin(acc); err != nil {
			p.Logf("账号 %s 加入，但登录失败%s：%v", acc.Name, p.egressTag(acc.Name), err)
			p.persist(acc)
			writeErr(w, http.StatusBadGateway, "账号已加入，但登录失败: "+err.Error())
			return
		}
	}
	p.persist(acc)
	p.Logf("新增账号 %s（登录态 %v）", acc.Name, acc.SessionValid())
	go p.checkinAndRefresh(acc)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Public()})
}

func (p *Panel) loginAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc := p.pool.Get(name)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name)
		return
	}
	if !p.tryLock(name) {
		writeErr(w, http.StatusConflict, "该账号正在执行其它运维动作")
		return
	}
	defer p.unlock(name)

	if err := p.doLogin(acc); err != nil {
		acc.SetLastError("登录失败: " + err.Error())
		p.Logf("账号 %s 登录失败%s：%v", name, p.egressTag(name), err)
		p.persist(acc)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	acc.Enabled = true
	acc.ResetCooldowns()
	p.persist(acc)
	p.Logf("账号 %s 登录成功（14 天有效）", name)
	go p.checkinAndRefresh(acc)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Public()})
}

// checkinAccount 单号签到/领取每日积分（官方客户端同款 first-login，幂等）。
func (p *Panel) checkinAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc := p.pool.Get(name)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := p.doCheckinCtx(ctx, acc)
	if err != nil {
		p.Logf("账号 %s 签到失败%s：%v", name, p.egressTag(name), err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	p.persist(acc)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "account": acc.Public(),
		"checkin": map[string]any{
			"already_processed": res.AlreadyProcessed,
			"daily_quota":       res.DailyQuota,
			"daily_consumed":    res.DailyConsumed,
			"daily_balance":     res.DailyBalance,
			"permanent_balance": res.PermanentBalance,
			"current_balance":   res.CurrentBalance,
			"daily_cycle_date":  res.CycleDate,
			"invitation_codes":  res.InvitationCodes,
		},
	})
}

// checkinAll 全池签到：对每个会话有效的账号调一次 first-login。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	accounts := p.pool.All()
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	type result struct {
		Name    string `json:"name"`
		OK      bool   `json:"ok"`
		Granted bool   `json:"granted"` // 本次真正领到了新周期额度
		Error   string `json:"error,omitempty"`
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var mu sync.Mutex
	var results []result

	for _, a := range accounts {
		if !a.Enabled || !a.SessionValid() {
			continue
		}
		wg.Add(1)
		go func(a *pool.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := p.doCheckinCtx(ctx, a)
			mu.Lock()
			defer mu.Unlock()
			item := result{Name: a.Name, OK: err == nil}
			if err != nil {
				item.Error = err.Error()
			} else {
				item.Granted = res != nil && !res.AlreadyProcessed
				p.persist(a)
			}
			results = append(results, item)
		}(a)
	}
	wg.Wait()
	okCount := 0
	for _, res := range results {
		if res.OK {
			okCount++
		}
	}
	p.Logf("全量签到：成功 %d / %d", okCount, len(results))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results, "overview": p.pool.Overview()})
}

// doCheckinCtx 领取每日赠送积分（幂等），成功后把口径快照写回账号。
func (p *Panel) doCheckinCtx(ctx context.Context, acc *pool.Account) (*upstream.CheckinResult, error) {
	if !acc.SessionValid() {
		return nil, fmt.Errorf("登录态已失效，需要重新登录")
	}
	res, err := p.upstream.Checkin(ctx, acc.Session)
	if err != nil {
		acc.SetLastError("签到失败: " + err.Error())
		return nil, err
	}
	acc.SetQuota(res.Quota)
	verb := "已领取新额度"
	if res.AlreadyProcessed {
		verb = "本周期已领过"
	}
	p.Logf("账号 %s 签到：%s（每日 %d/%d，长期 %d，可用 %d）", acc.Name,
		verb, res.DailyQuota-res.DailyConsumed, res.DailyQuota, res.PermanentBalance, res.CurrentBalance)
	// 顺带领新手任务（合计 10000 积分，零 token 消耗）：服务端不校验前置行为，
	// 已领过的号只多一次 GET（ClaimOnboarding 先查状态再补发）。登录 / 签到 /
	// 定时刷额度都会走到这里，新号因此不需要手动点一次「领新手任务」。
	if _, pts, oerr := p.claimOnboardingCtx(ctx, acc); oerr == nil && pts > 0 {
		p.Logf("账号 %s 新手任务到账 %d 积分", acc.Name, pts)
	}
	return res, nil
}

// ---------------------------------------------------------------- 新手任务

// onboardingAccount POST /panel/api/accounts/{name}/onboarding：领该号的新手任务。
func (p *Panel) onboardingAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc := p.pool.Get(name)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	claimed, points, err := p.claimOnboardingCtx(ctx, acc)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "account": acc.Public(),
		"onboarding": map[string]any{
			"claimed": claimed, "points": points,
			"total": upstream.OnboardingTotal,
		},
	})
}

// onboardingAll POST /panel/api/onboarding_all：全池领新手任务。
func (p *Panel) onboardingAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()

	type result struct {
		Name    string   `json:"name"`
		OK      bool     `json:"ok"`
		Claimed []string `json:"claimed,omitempty"`
		Points  int64    `json:"points"`
		Error   string   `json:"error,omitempty"`
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var mu sync.Mutex
	var results []result

	for _, a := range p.pool.All() {
		if !a.Enabled || !a.SessionValid() {
			continue
		}
		wg.Add(1)
		go func(a *pool.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			claimed, points, err := p.claimOnboardingCtx(ctx, a)
			mu.Lock()
			defer mu.Unlock()
			item := result{Name: a.Name, OK: err == nil, Claimed: claimed, Points: points}
			if err != nil {
				item.Error = err.Error()
			}
			results = append(results, item)
		}(a)
	}
	wg.Wait()
	var sum int64
	for _, res := range results {
		sum += res.Points
	}
	p.Logf("全量领新手任务：%d 个账号，共 %d 积分", len(results), sum)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "results": results, "points": sum, "overview": p.pool.Overview(),
	})
}

// claimOnboardingCtx 领该号所有未完成的新手任务，领到积分后刷新额度快照。
//
// 服务端不校验前置行为（实测不真的发对话/生成 PPT 也能拿满 10000），所以这是
// 纯 API 直领、零 token 消耗；幂等判据是响应体的 alreadyCompleted。
func (p *Panel) claimOnboardingCtx(ctx context.Context, acc *pool.Account) ([]string, int64, error) {
	if !acc.SessionValid() {
		return nil, 0, fmt.Errorf("登录态已失效，需要重新登录")
	}
	claimed, points, err := p.upstream.ClaimOnboarding(ctx, acc.Session)
	if err != nil {
		return nil, 0, err
	}
	if points > 0 {
		p.Logf("账号 %s 领新手任务：%d 个（%s），共 %d 积分",
			acc.Name, len(claimed), strings.Join(claimed, ","), points)
		// 积分变了：刷一次额度快照，面板立刻能看出 +10000（失败不影响领取结果）。
		if qerr := p.refreshQuotaCtx(ctx, acc); qerr == nil {
			p.persist(acc)
		}
	}
	return claimed, points, nil
}

// ---------------------------------------------------------------- 已下架模型

// deadModelsHandler GET /panel/api/dead_models：404 台账（模型级，与账号无关）。
func (p *Panel) deadModelsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"models":   p.pool.DeadModels(),
		"ttl_days": p.cfg.DeadModelTTLDays,
	})
}

// clearDeadModel POST /panel/api/dead_models/{model}/clear：手动解封一个模型。
func (p *Panel) clearDeadModel(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	if p.pool.ClearDeadModel(model) {
		p.Logf("模型 %s 已从「不可用」台账移除", model)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": p.pool.DeadModels()})
}

func (p *Panel) refreshAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc := p.pool.Get(name)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := p.refreshQuotaCtx(ctx, acc); err != nil {
		// 原先这里只把错误回给前端，运行日志里一个字都没有：用户点「余额」看到
		// 502，排障时无从下手。带上出口 IP 才能区分「账号问题」与「这个出口不通」。
		p.Logf("账号 %s 额度刷新失败%s：%v", name, p.egressTag(name), err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	p.persist(acc)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Public()})
}

func (p *Panel) disableAccount(w http.ResponseWriter, r *http.Request) {
	p.toggleAccount(w, r, false)
}

func (p *Panel) toggleAccount(w http.ResponseWriter, r *http.Request, enabled bool) {
	name := r.PathValue("name")
	acc := p.pool.Get(name)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name)
		return
	}
	acc.Enabled = enabled
	p.persist(acc)
	if enabled {
		p.Logf("账号 %s 已启用", name)
	} else {
		p.Logf("账号 %s 已禁用（不参与选号，定时任务仍可手动跑）", name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Public()})
}

func (p *Panel) reviveAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc := p.pool.Get(name)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name)
		return
	}
	acc.ResetCooldowns()
	acc.Enabled = true
	acc.SetLastError("")
	p.persist(acc)
	p.Logf("账号 %s 已解冻（冷却与熔断清零）", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Public()})
}

func (p *Panel) removeAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc := p.pool.Get(name)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name)
		return
	}
	p.pool.Remove(name)
	if err := pool.RemoveAccountFile(p.cfg.Resolve(p.cfg.AuthDir), name); err != nil {
		p.Logf("账号 %s 已移出池，但删除凭证文件失败：%v", name, err)
	}
	p.Logf("账号 %s 已移除", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) refreshAll(w http.ResponseWriter, r *http.Request) {
	accounts := p.pool.All()
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var mu sync.Mutex
	results := make([]map[string]any, 0, len(accounts))
	failByEgress := map[string]int{}
	for _, a := range accounts {
		if !a.Enabled || !a.SessionValid() {
			continue
		}
		wg.Add(1)
		go func(a *pool.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := p.refreshQuotaCtx(ctx, a)
			mu.Lock()
			defer mu.Unlock()
			item := map[string]any{"name": a.Name, "ok": err == nil}
			if err != nil {
				item["error"] = err.Error()
				// 失败按出口聚合：某个 IP 被上游拉黑时，这里会立刻聚成一堆，
				// 一眼分得出「号坏了」还是「这条出口坏了」。
				failByEgress[p.egressOf(a.Name)]++
			}
			results = append(results, item)
		}(a)
	}
	wg.Wait()
	okCount := len(results) - countValues(failByEgress)
	p.Logf("全量刷新额度：%d 个账号，成功 %d，失败 %d%s",
		len(results), okCount, countValues(failByEgress), egressFailNote(failByEgress))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results, "overview": p.pool.Overview()})
}

// countValues 汇总 map 里的计数。
func countValues(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// egressFailNote 把「失败出口 → 次数」拼成一行，按次数降序、同次数按标签升序
// （排序只为日志稳定可 diff）。空 map 返回空串，未启用出口隔离时不污染日志。
func egressFailNote(byEgress map[string]int) string {
	if len(byEgress) == 0 {
		return ""
	}
	type row struct {
		label string
		n     int
	}
	rows := make([]row, 0, len(byEgress))
	for ip, n := range byEgress {
		label := ip
		if label == "" {
			label = "direct" // 没绑定账号的调用：走默认出口
		}
		rows = append(rows, row{label: label, n: n})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].label < rows[j].label
	})
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = fmt.Sprintf("%s×%d", r.label, r.n)
	}
	return "（失败出口 " + strings.Join(parts, "、") + "）"
}

// ------------------------------------------------------------------ 内部

func (p *Panel) doLogin(acc *pool.Account) error {
	if acc.LoginID == "" || acc.Password == "" {
		return fmt.Errorf("缺少手机号或密码，无法自动登录")
	}
	if acc.Identity.DevID == "" {
		acc.Identity = account.NewIdentity()
	}
	// 用账号名（而不是 loginid）作为出口身份：出口分配按账号名算，两边必须一致。
	res, err := p.accounts.LoginByPasswordAs(acc.Name, acc.LoginID, acc.Password, acc.Identity)
	if err != nil {
		return err
	}
	acc.Session = res.Session
	acc.UserID = res.UserID
	acc.ObtainedAt = time.Now().Unix()
	acc.ExpireAt = acc.ObtainedAt + account.SessionExpireSeconds
	return nil
}

func (p *Panel) refreshQuota(acc *pool.Account) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.refreshQuotaCtx(ctx, acc); err != nil {
		p.Logf("账号 %s 额度刷新失败%s：%v", acc.Name, p.egressTag(acc.Name), err)
		return
	}
	p.persist(acc)
}

// checkinAndRefresh 复刻真实客户端的登录后动作链：先调 first-login 领每日积分
// （幂等，失败不阻断），再拉一次完整额度快照落盘。
func (p *Panel) checkinAndRefresh(acc *pool.Account) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := p.doCheckinCtx(ctx, acc); err != nil {
		p.Logf("账号 %s 登录后签到失败%s：%v", acc.Name, p.egressTag(acc.Name), err)
	} else {
		p.persist(acc)
	}
	// 签到成功时 SetQuota 已写入 first-login 口径；这里补 records 侧余额。
	if err := p.refreshQuotaCtx(ctx, acc); err != nil {
		p.Logf("账号 %s 额度刷新失败%s：%v", acc.Name, p.egressTag(acc.Name), err)
		return
	}
	p.persist(acc)
}

func (p *Panel) refreshQuotaCtx(ctx context.Context, acc *pool.Account) error {
	if !acc.SessionValid() {
		return fmt.Errorf("登录态已失效，需要重新登录")
	}
	balance, err := p.upstream.Quota(ctx, acc.Session)
	if err != nil {
		return err
	}
	acc.SetQuota(balance)
	return nil
}

// egressOf 该账号的出口 IP（未启用出口隔离时为空）。
//
// 运维日志里带上它：同一条「连不上上游 / 超时」的报错，可能只在某个出口上复现，
// 不写出来就只能靠猜（2026-10-23 的 .34 被讯飞丢包就是这样查了一轮）。
func (p *Panel) egressOf(name string) string {
	if p.egress == nil || !p.egress.Enabled() {
		return ""
	}
	return p.egress.IPString(name)
}

// egressTag 日志行尾的出口标记（未启用时为空串，保持旧行格式）。
func (p *Panel) egressTag(name string) string {
	if e := p.egressOf(name); e != "" {
		return "（出口 " + e + "）"
	}
	return ""
}

func (p *Panel) persist(acc *pool.Account) {
	if err := pool.SaveAccount(p.cfg.Resolve(p.cfg.AuthDir), acc); err != nil {
		p.Logf("账号 %s 凭证落盘失败：%v", acc.Name, err)
	}
}

func (p *Panel) anySession() string {
	for _, a := range p.pool.All() {
		if a.Enabled && a.SessionValid() {
			return a.Session
		}
	}
	return ""
}

func (p *Panel) tryLock(name string) bool {
	p.locksMu.Lock()
	m := p.locks[name]
	if m == nil {
		m = &sync.Mutex{}
		p.locks[name] = m
	}
	p.locksMu.Unlock()
	return m.TryLock()
}

func (p *Panel) unlock(name string) {
	p.locksMu.Lock()
	m := p.locks[name]
	p.locksMu.Unlock()
	if m != nil {
		m.Unlock()
	}
}

// poolOptions 把配置映射成池参数。
func poolOptions(cfg *config.Config) pool.Options {
	return pool.Options{
		Strategy:         cfg.Strategy,
		PickTopN:         cfg.PickTopN,
		MaxInFlight:      cfg.MaxInFlight,
		BalanceFloor:     int64(cfg.BalanceFloor),
		SoftRateBase:     float64(cfg.SoftRateBaseSeconds),
		SoftRateMax:      float64(cfg.SoftRateMaxSeconds),
		NotFoundCool:     float64(cfg.NotFoundCooldownSec),
		CooldownFallback: float64(cfg.CooldownSeconds),
		BreakerThreshold: cfg.BreakerThreshold,
		BreakerBase:      float64(cfg.BreakerBaseSeconds),
		BreakerMax:       float64(cfg.BreakerMaxSeconds),
		StickyTTL:        float64(cfg.StickyTTLSeconds),
		StickyMaxEntries: cfg.StickyMaxEntries,
		PreferDaily:      cfg.PreferDaily,
		DeadModelTTL:     time.Duration(cfg.DeadModelTTLDays) * 24 * time.Hour,
	}
}

// PoolOptions 供 main 复用同一份映射。
func PoolOptions(cfg *config.Config) pool.Options { return poolOptions(cfg) }

func todaySeconds() int64 {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return int64(now.Sub(start).Seconds())
}

// parseRange 解析 from/to（支持 unix 秒与 YYYY-MM-DD[THH:MM] 两种形态）。
func parseRange(fromStr, toStr string) (int64, int64) {
	var from, to int64
	if v := strings.TrimSpace(fromStr); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			from = n
		} else if t, err := parseTime(v); err == nil {
			from = t
		}
	}
	if v := strings.TrimSpace(toStr); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			to = n
		} else if t, err := parseTime(v); err == nil {
			to = t
		}
	}
	if from > 0 && to == 0 {
		to = time.Now().Unix()
	}
	return from, to
}

func parseTime(s string) (int64, error) {
	layouts := []string{
		"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02",
		time.RFC3339,
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("无法解析时间: %s", s)
}

func restartMsg(keys []string) string {
	if len(keys) == 0 {
		return "已保存，全部热生效"
	}
	sort.Strings(keys)
	return "已保存；以下字段需重启进程生效: " + strings.Join(keys, ", ")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeErr 与蓝本同款错误信封：业务错误一律带真实 HTTP 状态，
// 前端 api() 靠 res.ok 判成败，不必再读 body 里的 error 字段。
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
