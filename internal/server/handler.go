// Package server HTTP 网关：入站协议解析、账号轮转、SSE 转发与本地聚合。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rsfree/loomy2api/internal/bridge"
	"github.com/rsfree/loomy2api/internal/config"
	"github.com/rsfree/loomy2api/internal/httpauth"
	"github.com/rsfree/loomy2api/internal/pool"
	"github.com/rsfree/loomy2api/internal/upstream"
	"github.com/rsfree/loomy2api/internal/usage"
)

// Handler 网关主 handler。
type Handler struct {
	Cfg      *config.Config
	Pool     *pool.Pool
	Upstream *upstream.Client
	Ledger   *usage.Ledger
	Models   *ModelCache

	reqSeq  atomic.Int64
	started time.Time
}

// New 构造 handler。
func New(cfg *config.Config, p *pool.Pool, up *upstream.Client, ledger *usage.Ledger) *Handler {
	return &Handler{
		Cfg: cfg, Pool: p, Upstream: up, Ledger: ledger,
		Models:  NewModelCache(up),
		started: time.Now(),
	}
}

// Routes 返回路由表。
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", h.healthz)
	mux.HandleFunc("/status", h.withAuth(h.status))
	mux.HandleFunc("/metrics", h.withAuth(h.metrics))

	mux.HandleFunc("/v1/chat/completions", h.withAuth(h.chatCompletions))
	mux.HandleFunc("/v1/responses", h.withAuth(h.responses))
	mux.HandleFunc("/chat/completions", h.withAuth(h.chatCompletions))
	mux.HandleFunc("/v1/messages", h.withAuth(h.anthropicMessages))
	mux.HandleFunc("/messages", h.withAuth(h.anthropicMessages))
	mux.HandleFunc("/v1/embeddings", h.withAuth(h.passthrough("embeddings", "embed")))
	mux.HandleFunc("/v1/images/generations", h.withAuth(h.passthrough("images/generations", "image")))
	// edits 是 generations 的 multipart 前门（见 imagefront.go）。
	mux.HandleFunc("/v1/images/edits", h.withAuth(h.imageEdits))
	mux.HandleFunc("/v1/search", h.withAuth(h.search))
	mux.HandleFunc("/v1/models", h.withAuth(h.listModels))
	mux.HandleFunc("/v1/points", h.withAuth(h.points))
	mux.HandleFunc("/v1/", h.withAuth(h.notFound))

	return mux
}

// withAuth 统一鉴权包装。
func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !httpauth.Verify(r, h.Cfg.APIKeys) {
			writeErr(w, http.StatusUnauthorized, "invalid api key", "authentication_error")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 256<<20)
		next(w, r)
	}
}

// cors 允许浏览器端直接调用（同源之外的自托管前端）。
func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotFound, "未知路径 "+r.URL.Path, "invalid_request_error")
}

// ------------------------------------------------------------- health / 状态

// healthz 健康检查：有可用账号返回 200，否则 503。
//
// 响应带 service 字段与 X-Service 头作身份标识：同端口上可能残留别的服务，
// 对方即使返回 2xx 也不会带该字段，宿主探活据此避免「假成功」。
func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	ov := h.Pool.Overview()
	healthy, _ := ov["healthy"].(int)
	total, _ := ov["total"].(int)
	w.Header().Set("X-Service", "loomy2api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	code := http.StatusOK
	if healthy == 0 {
		code = http.StatusServiceUnavailable
	}
	w.WriteHeader(code)
	writeJSON(w, map[string]any{
		"healthy": healthy,
		"total":   total,
		"service": "loomy2api",
		"uptime":  int64(time.Since(h.started).Seconds()),
	})
}

// status 账号状态汇总 + 每账号详情。
func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	ov := h.Pool.Overview()
	accounts := make([]map[string]any, 0, h.Pool.Count())
	for _, a := range h.Pool.All() {
		accounts = append(accounts, a.Public())
	}
	ov["accounts"] = accounts
	ov["version"] = Version
	writeJSON(w, ov)
}

// metrics Prometheus 文本。
func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	ov := h.Pool.Overview()
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP loomy2api_accounts_total 账号总数\n")
	fmt.Fprintf(&b, "# TYPE loomy2api_accounts_total gauge\n")
	fmt.Fprintf(&b, "loomy2api_accounts_total %v\n", ov["total"])
	fmt.Fprintf(&b, "loomy2api_accounts_healthy %v\n", ov["healthy"])
	fmt.Fprintf(&b, "loomy2api_accounts_cooling %v\n", ov["cooling"])
	fmt.Fprintf(&b, "loomy2api_requests_total %v\n", ov["requests"])
	fmt.Fprintf(&b, "loomy2api_points_used_total %v\n", ov["points_used"])

	if w24 := h.Ledger.Summary(map[string]int64{"24h": 86400})["24h"]; w24 != nil {
		fmt.Fprintf(&b, "loomy2api_requests_24h %d\n", w24.Total)
		fmt.Fprintf(&b, "loomy2api_prompt_tokens_24h %d\n", w24.Prompt)
		fmt.Fprintf(&b, "loomy2api_completion_tokens_24h %d\n", w24.Completion)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(&b, "loomy2api_mem_alloc_bytes %d\n", ms.Alloc)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// ---------------------------------------------------------------- 模型目录

// ModelCache 模型目录缓存（1h，失败 5min 负缓存）。
type ModelCache struct {
	up      *upstream.Client
	mu      sync.Mutex
	models  []config.Model
	fetched time.Time
	failed  time.Time
}

// NewModelCache 构造缓存。
func NewModelCache(up *upstream.Client) *ModelCache {
	return &ModelCache{up: up}
}

// Get 取模型目录（优先缓存，其次上游，最后兜底表）。
func (m *ModelCache) Get(ctx context.Context, session string) []config.Model {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if len(m.models) > 0 && now.Sub(m.fetched) < time.Hour {
		return m.models
	}
	if !m.failed.IsZero() && now.Sub(m.failed) < 5*time.Minute {
		return fallbackOr(m.models)
	}
	if session == "" {
		return fallbackOr(m.models)
	}
	list, err := m.up.Models(ctx, session)
	if err != nil || len(list) == 0 {
		m.failed = now
		log.Printf("[models] 上游目录拉取失败：%v（回落到缓存/兜底表）", err)
		return fallbackOr(m.models)
	}
	m.models = list
	m.fetched = now
	m.failed = time.Time{}
	return list
}

// Cached 只返回**已从上游拉到的**目录（未拉过返回 nil，不回落到编译期兜底表）。
//
// 严格模型校验必须用这个口径：兜底表是编译期快照，拿它判"模型不存在"会把上游
// 新上的模型、以及测试里的假模型一起 404 掉。缓存为空时调用方应放行。
func (m *ModelCache) Cached() []config.Model {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.models) == 0 {
		return nil
	}
	return m.models
}

// Invalidate 强制下次重新拉取。
func (m *ModelCache) Invalidate() {
	m.mu.Lock()
	m.fetched = time.Time{}
	m.mu.Unlock()
}

func fallbackOr(cached []config.Model) []config.Model {
	if len(cached) > 0 {
		return cached
	}
	return config.FallbackModels()
}

// anySession 取任一健康账号的登录态（用于只读查询）。
func (h *Handler) anySession() string {
	for _, a := range h.Pool.All() {
		if a.Enabled && a.SessionValid() {
			return a.Session
		}
	}
	return ""
}

// listModels GET /v1/models。
func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	models := h.Models.Get(r.Context(), h.anySession())
	data := make([]map[string]any, 0, len(models))
	now := time.Now().Unix()
	for _, m := range models {
		// 已判定下架的模型不再对外播报：列出来只会让客户端选中一个必然 404 的条目。
		if h.Pool != nil && h.Pool.ModelDead(m.ID) {
			continue
		}
		item := map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  now,
			"owned_by": "iflytek",
		}
		if m.ContextLength > 0 {
			item["context_length"] = m.ContextLength
		}
		if m.MaxOutput > 0 {
			item["max_output_tokens"] = m.MaxOutput
		}
		if m.Multiplier > 0 {
			item["multiplier"] = m.Multiplier
		}
		if m.Name != "" {
			item["name"] = m.Name
		}
		if len(m.Capabilities) > 0 {
			item["capabilities"] = m.Capabilities
		}
		if len(m.ReasoningTiers) > 0 {
			item["reasoning_efforts"] = m.ReasoningTiers
		}
		if m.Type != "" {
			item["type"] = m.Type
		}
		if m.IsFree {
			// 限时免费（倍率 0 / 名称标注）：对外也标出来，客户端不必再解析展示名。
			item["is_free"] = true
			item["multiplier"] = 0
		}
		if m.IsImage() {
			// 图像模型的规范端点是 generations；edits 是 multipart 前门，
			// chat / responses 也能用（会折成生图请求，见 imagefront.go）。
			item["endpoint"] = "/v1/images/generations"
			item["edits_endpoint"] = "/v1/images/edits"
			item["supports_chat"] = true
		}
		if m.IsDefault {
			item["is_default_model"] = true
		}
		data = append(data, item)
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

// points GET /v1/points 聚合全池额度。
func (h *Handler) points(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0)
	for _, a := range h.Pool.All() {
		item := map[string]any{
			"name":       a.Name,
			"enabled":    a.Enabled,
			"balance":    a.Balance,
			"daily":      a.DailyBalance,
			"available":  a.AvailablePoints(),
			"updated_at": a.QuotaAt,
		}
		out = append(out, item)
	}
	writeJSON(w, map[string]any{"accounts": out})
}

// ---------------------------------------------------------------- 会话处理

// chatCompletions POST /v1/chat/completions。
func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	req, err := readJSON(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), "invalid_request_error")
		return
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		// 空 messages 直接 400 快速失败：不打上游、不占账号、不冷却。
		writeErr(w, http.StatusBadRequest, "messages 不能为空", "invalid_request_error")
		return
	}

	model := h.resolveModel(r.Context(), req)
	if h.imageOnlyModel(r.Context(), model) {
		// 图像模型走对话端点 = image-adapter 的 chat 前门：把 messages 折成
		// prompt + image，内部调 images/generations，再把结果封成 chat.completion。
		h.imageChat(w, r, req, model)
		return
	}
	if h.rejectUnknownModel(w, model) {
		return
	}
	if h.rejectDeadModel(w, model) {
		return
	}
	stream := truthy(req["stream"])
	skey := pool.SessionKey(req)
	extra := headerExtras(r, req)

	if !stream {
		// 非流式长任务：上游对久无响应的非流式请求约 60s 就回 504，
		// 而流式同模型能跑几分钟。故内部转流式取数再聚合，客户端无感。
		if minTok, _ := intOf(req["max_tokens"]); minTok >= h.Cfg.KeepaliveMinTokens {
			h.chatAggregated(w, r, req, model, skey, extra)
			return
		}
	}

	if stream {
		h.chatStream(w, r, req, model, skey, extra)
		return
	}
	h.chatOnce(w, r, req, model, skey, extra)
}

// chatOnce 非流式短请求：直接调上游。
func (h *Handler) chatOnce(w http.ResponseWriter, r *http.Request, req map[string]any, model, skey string, extra map[string]string) {
	started := time.Now()
	var tried []string
	var lastAcc *pool.Account
	var lastStatus int
	var lastBody string

	for attempt := 0; attempt <= h.Cfg.MaxRetries; attempt++ {
		acc, err := h.Pool.Acquire(tried, model, skey)
		if err != nil {
			if len(tried) == 0 {
				h.fail(w, r, "chat", model, nil, http.StatusServiceUnavailable, err.Error(), started)
				return
			}
			break
		}
		tried = append(tried, acc.Name)
		lastAcc = acc
		opts := h.Pool.Options()
		resp, err := h.Upstream.Do(r.Context(), http.MethodPost, "chat/completions", acc.Session, req, extra)
		if err != nil {
			h.Pool.Release(acc)
			h.Pool.ReportFailure(acc, 0, err.Error(), skey, model)
			lastStatus, lastBody = http.StatusBadGateway, err.Error()
			h.logCall(acc, model, 0, time.Since(started), "sync 连接异常: "+err.Error(), h.requestIP(r))
			continue
		}
		body := resp.BodyText()
		h.Pool.Release(acc)
		if resp.Status == http.StatusOK {
			var obj map[string]any
			usageObj := map[string]any{}
			if err := json.Unmarshal([]byte(body), &obj); err == nil {
				usageObj, _ = obj["usage"].(map[string]any)
				// 上游静默回落到默认模型时，响应里的 model 会变成别的名字：这次请求
				// 算"成功"但执行的不是客户端要的模型，且按默认模型扣分。strict 校验
				// 之外再留一条证据，免得 rogue 模型名悄悄烧钱。
				if got := str(obj["model"]); got != "" && !strings.EqualFold(got, model) &&
					!strings.EqualFold(strings.TrimPrefix(got, "imodel/"), strings.TrimPrefix(model, "imodel/")) {
					log.Printf("[model] ⚠️ 上游回落：请求 %s，实际执行 %s（客户端会拿到错答案且账单按后者算）", model, got)
				}
			}
			pts, prompt, completion, reasoning := usageParts(usageObj)
			h.Pool.ReportSuccess(acc, skey, model, pts)
			h.record(usage.Record{
				Account: acc.Name, Model: model, Kind: "chat", Status: 200,
				Prompt: prompt, Completion: completion, Reasoning: reasoning,
				Points: pts, LatencyMs: time.Since(started).Milliseconds(),
				ClientIP: clientIP(r), UserAgent: ua(r), RequestID: rid(resp.Header),
			})
			h.logCall(acc, model, 200, time.Since(started), usageSummary(prompt, completion, pts, reasoning), h.requestIP(r))
			w.Header().Set("X-Loomy-Account", acc.Name)
			if id := rid(resp.Header); id != "" {
				w.Header().Set("X-Request-Id", id)
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, body)
			_ = opts
			return
		}

		lastStatus, lastBody = resp.Status, body
		fault := pool.Classify(resp.Status, body)
		h.Pool.ReportFailure(acc, resp.Status, body, skey, model)
		h.logCall(acc, model, resp.Status, time.Since(started), "上游返回，换号重试", h.requestIP(r))
		if pool.FatalRequest(fault) {
			break
		}
	}

	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	if lastBody == "" {
		lastBody = "所有账号都失败了"
	}
	h.fail(w, r, "chat", model, lastAcc, lastStatus, lastBody, started)
}

// chatAggregated 非流式长任务：内部走流式取数再聚合。
func (h *Handler) chatAggregated(w http.ResponseWriter, r *http.Request, req map[string]any, model, skey string, extra map[string]string) {
	started := time.Now()
	sreq := cloneMap(req)
	sreq["stream"] = true

	// 先发 200 + chunked，并周期性写空白保活，绕开反代/CF 的 60~100s 504。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	stopKeepalive := make(chan struct{})
	done := make(chan struct{})
	if flusher != nil {
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stopKeepalive:
					return
				case <-done:
					return
				case <-t.C:
					// JSON 合法前导空白
					_, _ = io.WriteString(w, strings.Repeat(" ", 8))
					flusher.Flush()
				}
			}
		}()
	}
	defer func() {
		close(stopKeepalive)
		close(done)
	}()

	obj, err := h.collectStream(r.Context(), sreq, model, skey, extra)
	if err != nil {
		h.noteFail(r, "chat", model, nil, http.StatusBadGateway, err.Error(), started)
		writeJSON(w, map[string]any{"error": map[string]any{
			"message": err.Error(), "type": "upstream_error", "code": 502,
		}})
		return
	}
	h.logCall(nil, model, 200, time.Since(started), "流式聚合（长任务）", h.requestIP(r))
	buf, _ := json.Marshal(obj)
	_, _ = w.Write(buf)
}

// collectStream 用流式调上游并把 SSE 聚合成一条非流式响应。
//
// 内部强制 stream=true：调用方（/v1/messages、/v1/responses、长任务）传进来的
// 请求体可能是 stream:false，而这里按 SSE 解析响应——不覆盖的话上游返回普通 JSON，
// 解析器读到 0 帧，客户端会拿到一个空回答。
func (h *Handler) collectStream(ctx context.Context, req map[string]any, model, skey string, extra map[string]string) (map[string]any, error) {
	upReq := cloneMap(req)
	upReq["stream"] = true

	var tried []string
	var lastErr error

	for attempt := 0; attempt <= h.Cfg.MaxRetries; attempt++ {
		acc, err := h.Pool.Acquire(tried, model, skey)
		if err != nil {
			if len(tried) == 0 {
				return nil, err
			}
			break
		}
		tried = append(tried, acc.Name)
		started := time.Now()

		resp, err := h.Upstream.Do(ctx, http.MethodPost, "chat/completions", acc.Session, upReq, extra)
		if err != nil {
			h.Pool.Release(acc)
			h.Pool.ReportFailure(acc, 0, err.Error(), skey, model)
			lastErr = err
			continue
		}
		if resp.Status != http.StatusOK {
			body := resp.BodyText()
			h.Pool.Release(acc)
			h.Pool.ReportFailure(acc, resp.Status, body, skey, model)
			lastErr = fmt.Errorf("上游 HTTP %d: %s", resp.Status, truncate(body, 200))
			continue
		}

		agg := newAggregator()
		sc := newSSEStream(resp.Body)
		for {
			obj, ok, serr := sc.Next()
			if serr != nil {
				lastErr = serr
				break
			}
			if !ok {
				break
			}
			agg.Feed(obj)
		}
		_ = resp.Body.Close()
		h.Pool.Release(acc)

		out := agg.Result(model)
		pts, prompt, completion, reasoning := usageParts(agg.Usage())
		h.Pool.ReportSuccess(acc, skey, model, pts)
		h.record(usage.Record{
			Account: acc.Name, Model: model, Kind: "chat", Stream: false, Status: 200,
			Prompt: prompt, Completion: completion, Reasoning: reasoning, Points: pts,
			LatencyMs: time.Since(started).Milliseconds(),
		})
		return out, nil
	}
	if lastErr == nil {
		lastErr = errors.New("所有账号都失败了")
	}
	return nil, lastErr
}

// chatStream 流式请求：把上游 SSE 原样转发给客户端。
func (h *Handler) chatStream(w http.ResponseWriter, r *http.Request, req map[string]any, model, skey string, extra map[string]string) {
	started := time.Now()
	var tried []string
	var lastStatus int
	var lastBody string
	var lastResp *upstream.Response
	var lastAcc *pool.Account

	for attempt := 0; attempt <= h.Cfg.MaxRetries; attempt++ {
		acc, err := h.Pool.Acquire(tried, model, skey)
		if err != nil {
			if len(tried) == 0 {
				h.fail(w, r, "chat", model, nil, http.StatusServiceUnavailable, err.Error(), started)
				return
			}
			break
		}
		tried = append(tried, acc.Name)
		lastAcc = acc
		resp, err := h.Upstream.Do(r.Context(), http.MethodPost, "chat/completions", acc.Session, req, extra)
		if err != nil {
			h.Pool.Release(acc)
			h.Pool.ReportFailure(acc, 0, err.Error(), skey, model)
			lastStatus, lastBody = http.StatusBadGateway, err.Error()
			continue
		}
		if resp.Status == http.StatusOK {
			lastResp, lastAcc = resp, acc
			break
		}
		body := resp.BodyText()
		h.Pool.Release(acc)
		lastStatus, lastBody = resp.Status, body
		h.Pool.ReportFailure(acc, resp.Status, body, skey, model)
		if pool.FatalRequest(pool.Classify(resp.Status, body)) {
			break
		}
	}

	if lastResp == nil {
		if lastStatus == 0 {
			lastStatus = http.StatusBadGateway
		}
		h.fail(w, r, "chat", model, lastAcc, lastStatus, lastBody, started)
		return
	}
	defer h.Pool.Release(lastAcc)
	defer lastResp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Loomy-Account", lastAcc.Name)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var firstAt time.Time
	agg := newAggregator()

	sc := newSSEStream(lastResp.Body)
	for {
		obj, ok, err := sc.Next()
		if err != nil {
			// 上游断流：保留已拿到的部分，收尾由下面的统一兜底负责。
			break
		}
		if !ok {
			break
		}
		if firstAt.IsZero() {
			firstAt = time.Now()
		}
		agg.Feed(obj)
		buf, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
			break
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	// 上游的 [DONE] 只是结束标记、不会被转发，所以这里必须补发恰好一个。
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}

	elapsed := time.Since(started)
	pts, prompt, completion, reasoning := usageParts(agg.Usage())
	h.Pool.ReportSuccess(lastAcc, skey, model, pts)
	ttfb := int64(0)
	if !firstAt.IsZero() {
		ttfb = firstAt.Sub(started).Milliseconds()
	}
	h.record(usage.Record{
		Account: lastAcc.Name, Model: model, Kind: "chat", Stream: true, Status: 200,
		Prompt: prompt, Completion: completion, Reasoning: reasoning, Points: pts,
		LatencyMs: elapsed.Milliseconds(), TTFBMs: ttfb,
		ClientIP: clientIP(r), UserAgent: ua(r),
	})
	h.logStream(lastAcc, model, elapsed, ttfb, completion, reasoning, pts, h.requestIP(r))
}

// responses POST /v1/responses（Codex Responses API → chat completions）。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	req, err := readJSON(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), "invalid_request_error")
		return
	}
	chatReq, err := bridge.ResponsesToChat(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	model := h.resolveModel(r.Context(), chatReq)
	chatReq["model"] = model
	if h.imageOnlyModel(r.Context(), model) {
		// 图像模型的 responses 前门：input 折成 prompt + image，响应封成 output[]。
		h.imageResponses(w, r, req, model)
		return
	}
	if h.rejectUnknownModel(w, model) {
		return
	}
	if h.rejectDeadModel(w, model) {
		return
	}
	skey := pool.SessionKey(chatReq)
	extra := headerExtras(r, req)

	started := time.Now()
	if truthy(req["stream"]) {
		h.responsesStream(w, r, chatReq, model, skey, extra)
		return
	}
	obj, err := h.collectStream(r.Context(), chatReq, model, skey, extra)
	if err != nil {
		h.fail(w, r, "responses", model, nil, http.StatusBadGateway, err.Error(), started)
		return
	}
	writeJSON(w, bridge.ChatToResponse(obj, model))
}

// responsesStream Codex Responses SSE 输出。
func (h *Handler) responsesStream(w http.ResponseWriter, r *http.Request, chatReq map[string]any, model, skey string, extra map[string]string) {
	started := time.Now()
	sreq := cloneMap(chatReq)
	sreq["stream"] = true

	var tried []string
	var acc *pool.Account
	var resp *upstream.Response
	var lastStatus int
	var lastBody string
	for attempt := 0; attempt <= h.Cfg.MaxRetries; attempt++ {
		a, err := h.Pool.Acquire(tried, model, skey)
		if err != nil {
			if len(tried) == 0 {
				h.fail(w, r, "responses", model, nil, http.StatusServiceUnavailable, err.Error(), started)
				return
			}
			break
		}
		tried = append(tried, a.Name)
		acc = a
		rs, err := h.Upstream.Do(r.Context(), http.MethodPost, "chat/completions", a.Session, sreq, extra)
		if err != nil {
			h.Pool.Release(a)
			h.Pool.ReportFailure(a, 0, err.Error(), skey, model)
			lastStatus, lastBody = http.StatusBadGateway, err.Error()
			continue
		}
		if rs.Status == http.StatusOK {
			acc, resp = a, rs
			break
		}
		body := rs.BodyText()
		h.Pool.Release(a)
		lastStatus, lastBody = rs.Status, body
		h.Pool.ReportFailure(a, rs.Status, body, skey, model)
		if pool.FatalRequest(pool.Classify(rs.Status, body)) {
			break
		}
	}
	if resp == nil {
		if lastStatus == 0 {
			lastStatus = http.StatusBadGateway
		}
		h.fail(w, r, "responses", model, acc, lastStatus, lastBody, started)
		return
	}
	defer h.Pool.Release(acc)
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Loomy-Account", acc.Name)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	emit := func(ev bridge.SSEEvent) {
		buf, err := json.Marshal(ev.Data)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, buf)
		if flusher != nil {
			flusher.Flush()
		}
	}

	st := bridge.NewResponsesStreamer(model, "")
	for _, ev := range st.Start() {
		emit(ev)
	}
	sc := newSSEStream(resp.Body)
	for {
		obj, ok, err := sc.Next()
		if err != nil || !ok {
			break
		}
		for _, ev := range st.Feed(obj) {
			emit(ev)
		}
	}
	for _, ev := range st.Finish() {
		emit(ev)
	}

	pts, prompt, completion, reasoning := usageParts(st.Usage())
	h.Pool.ReportSuccess(acc, skey, model, pts)
	h.record(usage.Record{
		Account: acc.Name, Model: model, Kind: "responses", Stream: true, Status: 200,
		Prompt: prompt, Completion: completion, Reasoning: reasoning, Points: pts,
		LatencyMs: time.Since(started).Milliseconds(),
		ClientIP:  clientIP(r), UserAgent: ua(r),
	})
	h.logStream(acc, model, time.Since(started), 0, completion, reasoning, pts, h.requestIP(r))
}

// anthropicMessages POST /v1/messages。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	req, err := readJSON(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), "invalid_request_error")
		return
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		writeAnthropicErr(w, http.StatusBadRequest, "messages 不能为空")
		return
	}
	chatReq := bridge.ToOpenAI(req)
	model := h.resolveModel(r.Context(), chatReq)
	chatReq["model"] = model
	if h.imageOnlyModel(r.Context(), model) {
		writeAnthropicErr(w, http.StatusBadRequest,
			"模型 "+model+" 只支持图像生成，不能用于对话；请改调 POST /v1/images/generations")
		return
	}
	if h.Cfg != nil && h.Cfg.StrictModelCheck && !h.catalogHas(r.Context(), model) {
		writeAnthropicErr(w, http.StatusNotFound,
			"上游没有模型 "+strings.TrimPrefix(model, "imodel/")+"；可用模型见 GET /v1/models")
		return
	}
	if h.Pool.ModelDead(model) {
		writeAnthropicErr(w, http.StatusNotFound, deadModelMessage(model))
		return
	}
	skey := pool.SessionKey(chatReq)
	extra := headerExtras(r, req)

	started := time.Now()
	if truthy(req["stream"]) {
		h.anthropicStream(w, r, chatReq, model, skey, extra)
		return
	}
	obj, err := h.collectStream(r.Context(), chatReq, model, skey, extra)
	if err != nil {
		h.noteFail(r, "messages", model, nil, http.StatusBadGateway, err.Error(), started)
		writeAnthropicErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, bridge.ToAnthropic(obj, model))
}

// anthropicStream Anthropic SSE 输出。
func (h *Handler) anthropicStream(w http.ResponseWriter, r *http.Request, chatReq map[string]any, model, skey string, extra map[string]string) {
	started := time.Now()
	sreq := cloneMap(chatReq)
	sreq["stream"] = true

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	// 先占好账号与响应头，避免把错误当成 SSE 发出去。
	var tried []string
	var acc *pool.Account
	var resp *upstream.Response
	var lastStatus int
	var lastBody string
	for attempt := 0; attempt <= h.Cfg.MaxRetries; attempt++ {
		a, err := h.Pool.Acquire(tried, model, skey)
		if err != nil {
			if len(tried) == 0 {
				h.noteFail(r, "messages", model, nil, http.StatusServiceUnavailable, err.Error(), started)
				writeAnthropicErr(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			break
		}
		tried = append(tried, a.Name)
		acc = a
		rs, err := h.Upstream.Do(r.Context(), http.MethodPost, "chat/completions", a.Session, sreq, extra)
		if err != nil {
			h.Pool.Release(a)
			h.Pool.ReportFailure(a, 0, err.Error(), skey, model)
			lastStatus, lastBody = http.StatusBadGateway, err.Error()
			continue
		}
		if rs.Status == http.StatusOK {
			acc, resp = a, rs
			break
		}
		body := rs.BodyText()
		h.Pool.Release(a)
		lastStatus, lastBody = rs.Status, body
		h.Pool.ReportFailure(a, rs.Status, body, skey, model)
		if pool.FatalRequest(pool.Classify(rs.Status, body)) {
			break
		}
	}
	if resp == nil {
		if lastStatus == 0 {
			lastStatus = http.StatusBadGateway
		}
		h.noteFail(r, "messages", model, nil, lastStatus, lastBody, started)
		writeAnthropicErr(w, lastStatus, lastBody)
		return
	}
	defer h.Pool.Release(acc)
	defer resp.Body.Close()

	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	emit := func(ev bridge.Event) {
		buf, err := json.Marshal(ev.Data)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, buf)
		if flusher != nil {
			flusher.Flush()
		}
	}

	st := bridge.NewStreamTranslator(model, "")
	for _, ev := range st.Start() {
		emit(ev)
	}
	sc := newSSEStream(resp.Body)
	for {
		obj, ok, err := sc.Next()
		if err != nil || !ok {
			break
		}
		for _, ev := range st.Feed(obj) {
			emit(ev)
		}
	}
	for _, ev := range st.Finish() {
		emit(ev)
	}

	pts, prompt, completion, reasoning := usageParts(st.Usage())
	h.Pool.ReportSuccess(acc, skey, model, pts)
	h.record(usage.Record{
		Account: acc.Name, Model: model, Kind: "messages", Stream: true, Status: 200,
		Prompt: prompt, Completion: completion, Reasoning: reasoning, Points: pts,
		LatencyMs: time.Since(started).Milliseconds(),
		ClientIP:  clientIP(r), UserAgent: ua(r),
	})
}

// passthrough 图片 / 向量等固定上游端点。
func (h *Handler) passthrough(suffix, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "读取请求体失败", "invalid_request_error")
			return
		}
		var payload map[string]any
		if len(body) > 0 {
			if err := json.Unmarshal(body, &payload); err != nil {
				writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON", "invalid_request_error")
				return
			}
		}
		var model string
		if kind == "image" {
			var replaced string
			model, replaced = h.resolveImageModel(r.Context(), payload)
			if replaced != "" {
				log.Printf("[image] 模型 %q 不在目录，回退到 %s", replaced, model)
			}
		} else {
			model = h.resolveModel(r.Context(), payload)
		}
		if h.rejectUnknownModel(w, model) {
			return
		}

		started := time.Now()
		var tried []string
		var lastAcc *pool.Account
		var lastStatus int
		var lastBody string
		for attempt := 0; attempt <= h.Cfg.MaxRetries; attempt++ {
			acc, err := h.Pool.Acquire(tried, model, "")
			if err != nil {
				if len(tried) == 0 {
					h.fail(w, r, kind, model, nil, http.StatusServiceUnavailable, err.Error(), started)
					return
				}
				break
			}
			tried = append(tried, acc.Name)
			lastAcc = acc
			resp, err := h.Upstream.Do(r.Context(), http.MethodPost, suffix, acc.Session, payload, nil)
			if err != nil {
				h.Pool.Release(acc)
				h.Pool.ReportFailure(acc, 0, err.Error(), "", model)
				lastStatus, lastBody = http.StatusBadGateway, err.Error()
				continue
			}
			// 生图 400 多为客户端带了下游 provider 不认的扩展字段（见 imageslim.go）：
			// 第一次先按白名单减肥重试，避免把「请求有误」直接抛给用户。
			if resp.Status == http.StatusBadRequest && kind == "image" {
				// 去掉哪些字段已由 shrinkImageRetry 打日志；这里不再额外写请求行，
				// 否则一次请求会同时出现「已精简」和「200 points=…」两行。
				resp, payload, _ = h.shrinkImageRetry(r.Context(), suffix, acc.Session, payload, resp)
			}
			out := resp.BodyText()
			h.Pool.Release(acc)
			if resp.Status == http.StatusOK {
				var obj map[string]any
				usageObj := map[string]any{}
				if json.Unmarshal([]byte(out), &obj) == nil {
					usageObj, _ = obj["usage"].(map[string]any)
				}
				pts, prompt, completion, reasoning := usageParts(usageObj)
				// 图像生成没有 usage 对象，扣分在响应**顶层** points_consumed。
				// 不兜这一层，面板的 points_used 会永远是 0（实测 7 张图 770 分全漏记）。
				if pts == 0 && obj != nil {
					pts = int64(numOf(obj["points_consumed"]))
				}
				h.Pool.ReportSuccess(acc, "", model, pts)
				h.record(usage.Record{
					Account: acc.Name, Model: model, Kind: kind, Status: 200,
					Prompt: prompt, Completion: completion, Reasoning: reasoning, Points: pts,
					LatencyMs: time.Since(started).Milliseconds(),
					ClientIP:  clientIP(r), UserAgent: ua(r),
				})
				// passthrough（生图/embeddings/search）也要进「运行日志」：
				// 否则面板看不到这些请求，更没法核对来源 IP / 代理是否生效。
				h.logCall(acc, model, 200, time.Since(started),
					usageSummary(prompt, completion, pts, reasoning), h.requestIP(r))
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("X-Loomy-Account", acc.Name)
				w.WriteHeader(200)
				_, _ = io.WriteString(w, out)
				return
			}
			lastStatus, lastBody = resp.Status, out
			// 非对话端点：404 只说明该模型不支持这个能力，不能写模型台账（见 pool 注释）。
			h.Pool.ReportFailureNoDead(acc, resp.Status, out, "", model)
			if pool.FatalRequest(pool.Classify(resp.Status, out)) {
				break
			}
		}
		if lastStatus == 0 {
			lastStatus = http.StatusBadGateway
		}
		h.fail(w, r, kind, model, lastAcc, lastStatus, lastBody, started)
	}
}

// search POST /v1/search（内置联网检索，只读，不占账号额度）。
func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	payload, err := readJSON(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败", "invalid_request_error")
		return
	}
	session := h.anySession()
	if session == "" {
		h.fail(w, r, "search", "", nil, http.StatusServiceUnavailable, "没有可用账号", started)
		return
	}
	obj, status, err := h.Upstream.Search(r.Context(), session, payload)
	if err != nil {
		h.fail(w, r, "search", "", nil, http.StatusBadGateway, err.Error(), started)
		return
	}
	if status != 200 {
		h.fail(w, r, "search", "", nil, status, toJSON(obj), started)
		return
	}
	writeJSON(w, obj)
}

// ---------------------------------------------------------------- 工具函数

func (h *Handler) resolveModel(ctx context.Context, payload map[string]any) string {
	model := strings.TrimSpace(str(payload["model"]))
	if model == "" {
		model = h.Cfg.DefaultModel
	}

	// 先按目录归一：精确优先，其次最长前缀（deepseek-v4.1-flash-xxx →
	// DeepSeek-V4.1-Flash）。归一后下游的 imageOnlyModel / 严格校验 / 下架台账
	// / 发往上游的 model 字段全都用同一个标准 id，不必各自处理别名。
	if m, ok := h.matchModel(ctx, model); ok {
		model = m.ID
	} else if h.Cfg.StripPrefix && !strings.Contains(model, "/") {
		// 目录里没有：保持旧行为，裸 id 补 provider 前缀，交给严格校验或上游处理。
		//
		// 不能无脑补：上游 /models 给的是裸 id（spark-x、DeepSeek-V4.1-Flash），
		// 补了前缀上游认不出就会**静默回落到默认模型**（客户端以为在用 spark-x，
		// 实际扣的是默认模型的积分，响应里 model 字段也会变）。所以命中目录的
		// 一律原样用目录 id。
		model = modelProviderPrefix + model
	}
	if payload != nil {
		payload["model"] = model
		// deepseek 系只认 <image_base64> 标记，其余认 image_url（见 imgtag.go 的实测依据）。
		rewriteImageTags(payload, model)
		liftMaxTokens(payload)
	}
	return model
}

// resolveImageModel 图像端点的模型解析：在普通解析之上加两层兜底。
//
//  1. 目录里有 → 用目录里的标准 id（精确优先，其次最长前缀，见 matchCatalog）；
//  2. 目录里没有 → 按**家族前缀**找同族图像模型（doubao-seedream-4-0-250828 →
//     doubao-seedream-5-lite、qwen-image-2.5 → qwen-image-3.0-pro），同族多个取版本最高；
//  3. 连家族都对不上 → 用配置的 image_fallback_model（默认 doubao-seedream-5-lite）。
//
// 对话端点不走这条路：上游对认不出的模型名会静默用默认模型执行并照常扣分，
// 所以拼错的对话模型必须 404，不能变成一次生图。
//
// 返回最终模型与「被替换掉的原始名字」（空串表示没替换）。
func (h *Handler) resolveImageModel(ctx context.Context, payload map[string]any) (string, string) {
	raw := strings.TrimSpace(str(payload["model"]))
	fb := h.imageFallbackModel(ctx)
	if raw == "" {
		if fb == "" {
			return h.resolveModel(ctx, payload), ""
		}
		// 生图请求没写 model：默认模型往往是对话模型，这里直接用图像兜底。
		// 不额外记日志——请求行里的 model 已经是兜底后的名字。
		payload["model"] = fb
		return fb, ""
	}

	model := h.resolveModel(ctx, payload)
	if h.catalogHas(ctx, model) {
		return model, ""
	}

	target := ""
	if m, ok := familyFallback(h.catalog(ctx), raw); ok {
		target = m.ID
	}
	if target == "" {
		target = fb
	}
	if target == "" {
		return model, ""
	}
	payload["model"] = target
	// 回显客户端原本写的名字：resolveModel 可能已经补过 imodel/ 前缀。
	return target, raw
}

// catalogModelID 把名字归一成目录 id；目录还没拉到时按原样返回（冷启窗口）。
// 目录里确实没有这个模型时返回空串——宁可直接 404，也不发一个不存在的名字。
func (h *Handler) catalogModelID(ctx context.Context, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if m, ok := h.matchModel(ctx, name); ok {
		return m.ID
	}
	if h.Models == nil || len(h.Models.Cached()) == 0 {
		return name
	}
	return ""
}

// imageFallbackModel 返回配置的图像通用兜底模型（归一成目录 id）。
// 未配置、或目录里已经没有这个模型时返回空串。
func (h *Handler) imageFallbackModel(ctx context.Context) string {
	if h.Cfg == nil {
		return ""
	}
	return h.catalogModelID(ctx, h.Cfg.ImageFallbackModel)
}

// catalog 取模型目录，**只读缓存**：对话热路径不该为了一次类型判断去等一趟上游。
// 缓存由启动预热（WarmCatalog）、/v1/models 与面板查询填充；缓存过期时
// ModelCache 会返回上一份目录，所以预热过就有类型信息可用。
func (h *Handler) catalog(ctx context.Context) []config.Model {
	if h.Models == nil {
		return nil
	}
	return h.Models.Get(ctx, "")
}

// WarmCatalog 用任一可用登录态把模型目录拉进缓存，返回条目数。
//
// 必须在启动时调一次（见 cmd/server）：matchModel（精确 / 最长前缀归一）与
// imageOnlyModel（图像模型不进对话端点）都依赖目录里的元数据，冷缓存下
// 两者都会误判，而上游对认不出的模型名不会报错——它静默回落到默认模型
// （实测响应里 model 变成 DeepSeek-V4.1-Flash，扣的是默认模型的积分）。
func (h *Handler) WarmCatalog(ctx context.Context) int {
	if h.Models == nil {
		return 0
	}
	return len(h.Models.Get(ctx, h.anySession()))
}

// matchModel 在目录里解析模型名（精确优先，其次最长前缀）。
// 目录拉不到时返回 false，调用方退回旧行为（补前缀 / 放行）。
func (h *Handler) matchModel(ctx context.Context, model string) (config.Model, bool) {
	return matchCatalog(h.catalog(ctx), model)
}

// imageOnlyModel 判断模型是否只能走生图端点（上游目录里 type=image）。
// 目录拉不到时返回 false：宁可放行让上游自己判定，也不臆测模型类型。
func (h *Handler) imageOnlyModel(ctx context.Context, model string) bool {
	if m, ok := h.matchModel(ctx, model); ok {
		return m.IsImage()
	}
	return false
}

// deadModelMessage 模型已被上游判定「暂未开放 / 已下架」时的提示。
func deadModelMessage(model string) string {
	return "模型 " + model + " 在上游不可用（404 已下架 / 暂未开放），已从模型列表隐藏；" +
		"可等它重新上架或改用其它模型"
}

// catalogHas 判断模型是否在上游目录里（`imodel/` 前缀与大小写不敏感）。
// 目录为空（还没预热、也没兜底表）时返回 true：宁可放行，也不能因为没查到目录
// 就把所有请求 404 掉。
func (h *Handler) catalogHas(ctx context.Context, model string) bool {
	if h.Models == nil {
		return true
	}
	list := h.Models.Cached()
	if len(list) == 0 {
		return true // 目录还没拉到：放行，由上游自己判
	}
	if _, ok := matchCatalog(list, model); ok {
		return true
	}
	return false
}

// rejectUnknownModel 目录里没有这个模型就直接 404（OpenAI 形态），不打上游。
//
// 为什么必须拦：Loomy 对**认不出的模型名**不报错，而是静默用默认模型执行并照常
// 返回 200（实测 `no-such-model-xyz` → `"model":"DeepSeek-V4.1-Flash"`，按默认
// 模型扣分）。不拦的话客户端会拿到一个"成功"的错答案，账单也对不上——比 404 更糟。
// 返回 true 表示已经写过响应。
func (h *Handler) rejectUnknownModel(w http.ResponseWriter, model string) bool {
	if h.Cfg == nil || !h.Cfg.StrictModelCheck || model == "" {
		return false
	}
	if h.catalogHas(context.Background(), model) {
		return false
	}
	// 文案里用客户端原本写的名字（resolveModel 可能补过 imodel/ 前缀，回显前缀
	// 会让用户以为是自己的模型名写错了）。
	_, base := splitModelPrefix(model)
	msg := "上游没有模型 " + base + "；可用模型见 GET /v1/models"
	if sug := suggestModels(h.Models.Cached(), model, 3); len(sug) > 0 {
		msg += "。最接近：" + strings.Join(sug, "、")
	}
	writeErr(w, http.StatusNotFound, msg, "not_found_error")
	return true
}

// rejectDeadModel 模型处于「已下架」台账内就直接回 404（OpenAI 形态），不打上游。
// 返回 true 表示已经写过响应。
func (h *Handler) rejectDeadModel(w http.ResponseWriter, model string) bool {
	if h.Pool == nil || !h.Pool.ModelDead(model) {
		return false
	}
	writeErr(w, http.StatusNotFound, deadModelMessage(model), "not_found_error")
	return true
}

// rejectImageModel 图像模型被当对话模型用时的 400（OpenAI 形态）。
func rejectImageModel(w http.ResponseWriter, model string) {
	writeErr(w, http.StatusBadRequest,
		"模型 "+model+" 只支持图像生成，不能用于对话；请改调 POST /v1/images/generations",
		"invalid_request_error")
}

func (h *Handler) record(r usage.Record) {
	if h.Ledger != nil {
		h.Ledger.Record(r)
	}
}

// noteFail 记一条失败台账。失败不进台账的话，「请求记录」页只剩成功，
// 排障只能翻 stderr。
func (h *Handler) noteFail(r *http.Request, kind, model string, acc *pool.Account, status int, body string, started time.Time) {
	if status == 0 {
		status = http.StatusBadGateway
	}
	name := ""
	if acc != nil {
		name = acc.Name
	}
	h.record(usage.Record{
		Account: name, Model: model, Kind: kind, Status: status,
		LatencyMs: time.Since(started).Milliseconds(),
		ClientIP:  clientIP(r), UserAgent: ua(r), Error: truncate(body, 300),
	})
}

// fail 记失败台账并回写上游错误体。
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, kind, model string, acc *pool.Account, status int, body string, started time.Time) {
	h.noteFail(r, kind, model, acc, status, body, started)
	h.writeUpstreamError(w, status, body)
}

// requestIP 仅在 log_client_info 打开时返回来源 IP，供「运行日志」核对
// 客户端是否走了预期出口（代理是否生效）。
func (h *Handler) requestIP(r *http.Request) string {
	if h.Cfg == nil || !h.Cfg.SendClientInfo || r == nil {
		return ""
	}
	return clientIP(r)
}

func (h *Handler) logCall(acc *pool.Account, model string, status int, d time.Duration, note, ip string) {
	if !h.Cfg.LogRequests {
		return
	}
	name := "-"
	if acc != nil {
		name = acc.Name
	}
	seq := h.reqSeq.Add(1)
	if ip != "" {
		note = strings.TrimSpace(note + " ip=" + ip)
	}
	log.Printf("| #%03d | %s | %-11s | %3d | %s | %6dms | %s |",
		seq, time.Now().Format("15:04:05"), truncate(model, 11), status,
		truncate(name, 20), d.Milliseconds(), note)
}

func (h *Handler) logStream(acc *pool.Account, model string, d time.Duration, ttfbMs, completion, reasoning, pts int64, ip string) {
	if !h.Cfg.LogRequests {
		return
	}
	seq := h.reqSeq.Add(1)
	rate := "-"
	if gen := d.Milliseconds() - ttfbMs; gen > 0 && completion > 0 {
		rate = fmt.Sprintf("%.1ftok/s", float64(completion)/float64(gen)*1000)
	}
	name := "-"
	if acc != nil {
		name = acc.Name
	}
	extra := ""
	if reasoning > 0 {
		extra = fmt.Sprintf(" think=%d", reasoning)
	}
	if ip != "" {
		extra += " ip=" + ip
	}
	log.Printf("| #%03d | %s | %-11s | stream | 200 | %s | TTFB=%dms | tok=%d%s | %s | total=%s |",
		seq, time.Now().Format("15:04:05"), truncate(model, 11), truncate(name, 20),
		ttfbMs, completion, extra, rate, d.Round(time.Millisecond))
	_ = pts
}

func (h *Handler) writeUpstreamError(w http.ResponseWriter, status int, body string) {
	// 上游错误体尽量原样透传（信息量比自造的大）。
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err == nil && obj != nil {
		if _, ok := obj["error"]; ok {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			writeJSON(w, obj)
			return
		}
	}
	if status == 0 {
		status = http.StatusBadGateway
	}
	writeErr(w, status, truncate(body, 800), "upstream_error")
}

// SSEStream SSE 增量读取器。
type SSEStream struct {
	body    io.Reader
	buf     []byte
	eof     bool
	sawDone bool
}

func newSSEStream(body io.Reader) *SSEStream {
	return &SSEStream{body: body, buf: make([]byte, 0, 16<<10)}
}

// Next 读下一个 data 帧；ok=false 表示流结束。
func (s *SSEStream) Next() (map[string]any, bool, error) {
	tmp := make([]byte, 8<<10)
	for {
		// 从缓冲里切出一行
		if i := indexByte(s.buf, '\n'); i >= 0 {
			line := trimCR(s.buf[:i])
			s.buf = s.buf[i+1:]
			if obj, done, ok := parseSSELine(line); ok {
				if done {
					s.sawDone = true
					return nil, false, nil
				}
				return obj, true, nil
			}
			continue
		}
		if s.eof {
			if len(s.buf) > 0 {
				line := trimCR(s.buf)
				s.buf = s.buf[:0]
				if obj, done, ok := parseSSELine(line); ok {
					if done {
						s.sawDone = true
						return nil, false, nil
					}
					return obj, true, nil
				}
			}
			return nil, false, nil
		}
		n, err := s.body.Read(tmp)
		if n > 0 {
			s.buf = append(s.buf, tmp[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.eof = true
				continue
			}
			return nil, false, err
		}
	}
}

func parseSSELine(line []byte) (map[string]any, bool, bool) {
	if !hasPrefix(line, "data:") {
		return nil, false, false
	}
	payload := trimSpace(line[5:])
	if len(payload) == 0 {
		return nil, false, false
	}
	if string(payload) == "[DONE]" {
		return nil, true, true
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, false, false
	}
	return obj, false, true
}

// Aggregator 把 SSE 增量聚合成一条完整响应。
type Aggregator struct {
	content strings.Builder
	reason  strings.Builder
	tools   map[int]*toolBuf
	order   []int
	finish  string
	usage   map[string]any
	id      string
	created int64
	model   string
}

type toolBuf struct {
	ID   string
	Name string
	Args strings.Builder
}

func newAggregator() *Aggregator {
	return &Aggregator{tools: map[int]*toolBuf{}, usage: map[string]any{}}
}

// Feed 吃一个增量。
func (a *Aggregator) Feed(obj map[string]any) {
	if id := str(obj["id"]); id != "" {
		a.id = id
	}
	if m := str(obj["model"]); m != "" {
		a.model = m
	}
	if c, ok := obj["created"].(float64); ok && c > 0 {
		a.created = int64(c)
	}
	if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
		for k, v := range u {
			a.usage[k] = v
		}
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	ch, _ := choices[0].(map[string]any)
	if fr := str(ch["finish_reason"]); fr != "" {
		a.finish = fr
	}
	delta, _ := ch["delta"].(map[string]any)
	if delta == nil {
		delta, _ = ch["message"].(map[string]any)
	}
	if delta == nil {
		return
	}
	if t := str(delta["content"]); t != "" {
		a.content.WriteString(t)
	}
	if r := str(delta["reasoning_content"]); r != "" {
		a.reason.WriteString(r)
	}
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			idx, _ := intOf(call["index"])
			buf, exists := a.tools[idx]
			if !exists {
				buf = &toolBuf{}
				a.tools[idx] = buf
				a.order = append(a.order, idx)
			}
			if id := str(call["id"]); id != "" {
				buf.ID = id
			}
			fn, _ := call["function"].(map[string]any)
			if name := str(fn["name"]); name != "" {
				buf.Name = name
			}
			if args := str(fn["arguments"]); args != "" {
				buf.Args.WriteString(args)
			}
		}
	}
}

// Usage 累积的 usage。
func (a *Aggregator) Usage() map[string]any { return a.usage }

// Result 拼出非流式响应。
func (a *Aggregator) Result(model string) map[string]any {
	content := a.content.String()
	msg := map[string]any{"role": "assistant", "content": content}
	if r := a.reason.String(); r != "" {
		msg["reasoning_content"] = r
	}
	if len(a.order) > 0 {
		sort.Ints(a.order)
		calls := make([]any, 0, len(a.order))
		for _, idx := range a.order {
			buf := a.tools[idx]
			if buf.Name == "" {
				continue
			}
			id := buf.ID
			if id == "" {
				id = "call_" + randomHex(6)
			}
			args := buf.Args.String()
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": buf.Name, "arguments": args},
			})
		}
		if len(calls) > 0 {
			msg["tool_calls"] = calls
			if content == "" {
				msg["content"] = nil
			}
		}
	}
	id := a.id
	if id == "" {
		id = "chatcmpl-" + randomHex(6)
	}
	finish := a.finish
	if finish == "" {
		finish = "stop"
	}
	name := a.model
	if name == "" {
		name = model
	}
	created := a.created
	if created == 0 {
		created = time.Now().Unix()
	}
	return map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": name,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "logprobs": nil, "finish_reason": finish,
		}},
		"usage": a.usage,
	}
}

// headerExtras 从请求体里摘出归属头参数（ChatId / MsgId / TurnId / purpose）。
func headerExtras(r *http.Request, payload map[string]any) map[string]string {
	extra := map[string]string{}
	for _, pair := range [][2]string{
		{"chat_id", "ChatId"}, {"msg_id", "MsgId"}, {"turn_id", "TurnId"},
	} {
		if v, ok := payload[pair[0]]; ok {
			if s := str(v); s != "" {
				extra[pair[1]] = s
			}
			delete(payload, pair[0])
		}
	}
	if v, ok := payload["request_purpose"]; ok {
		if s := str(v); s != "" {
			extra["X-Loomy-Request-Purpose"] = s
		}
		delete(payload, "request_purpose")
	}
	// 客户端自带的 traceparent 优先（保持链路串起来）。
	if tp := r.Header.Get("traceparent"); tp != "" {
		extra["traceparent"] = tp
	}
	return extra
}

func (h *Handler) writeUpstreamErrorPreserving(w http.ResponseWriter, status int, body string) {
	h.writeUpstreamError(w, status, body)
}

// Version 网关版本（发布时由 CI 用 -X 注入）。
var Version = "dev"
