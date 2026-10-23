// Package upstream Loomy 模型网关客户端（OpenAI 兼容上游）。
//
// 上游是 OpenAI 兼容的，但它对请求头有硬要求：
//   - Authorization 与 token 都要带（客户端用 useSessionAuth，网关两者都收）；
//   - traceparent 缺失会让上游挂死到超时（客户端源码里实测的结论）；
//   - loomy-version 只做存在性检查。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rsfree/loomy2api/internal/account"
	"github.com/rsfree/loomy2api/internal/config"
	"github.com/rsfree/loomy2api/internal/egress"
)

// 真实 /models 响应把「倍率」和「限时免费」都写在展示名里，没有结构化字段：
//
//	"Spark-X2.6-preview(限时免费)(x0.0)" → 限免，倍率 0
//	"DeepSeek V4.1 Flash(x2.0)"          → 倍率 2.0
//	"MiniMax M3 （x4.0）"                → 全角括号，倍率 4.0
//
// 不解析就会让面板倍率列永远是「—」，也看不出哪些模型限免（抓包核对）。
var (
	reModelMult = regexp.MustCompile(`[（(]\s*[xX×]\s*([0-9]+(?:\.[0-9]+)?)\s*[）)]`)
	reModelFree = regexp.MustCompile(`限时免费|限免`)
)

// Client 上游模型网关客户端。
type Client struct {
	BaseURL string
	Version string
	Proxy   string
	Timeout time.Duration

	HTTP *http.Client

	// 出口隔离（按账号绑定本机源 IP）。未配置时保持原有单出口行为。
	egressResolver *egress.Resolver
	sessionOwner   func(session string) string
	clients        *egress.ClientCache
}

// NewClient 构造客户端。
func NewClient(base, version, proxy string, timeout time.Duration) *Client {
	c := &Client{
		BaseURL: strings.TrimRight(base, "/"),
		Version: version,
		Proxy:   proxy,
		Timeout: timeout,
		// 总时长不设上限：长思考 / 长输出不能被掐断。
		HTTP: &http.Client{Transport: newTransport(nil), Timeout: 0},
	}
	// 每个出口一个 Transport：连接池与源地址绑定，混用等于让出口隔离失效。
	c.clients = egress.NewClientCache(func(ip net.IP) *http.Client {
		return &http.Client{Transport: newTransport(ip), Timeout: 0}
	})
	return c
}

// newTransport 构造上游 Transport；ip 非空时把源地址钉在该出口。
func newTransport(ip net.IP) *http.Transport {
	d := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	if ip != nil {
		d.LocalAddr = &net.TCPAddr{IP: ip}
	}
	return &http.Transport{
		DialContext:           d.DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// SetEgress 注入出口策略与「会话 → 账号名」映射。
//
// 用映射函数而不是在调用点传账号名：所有上游调用（对话、生图、模型目录、
// 额度）都带 session，一处理完就全覆盖，新增端点也不会漏掉出口绑定。
func (c *Client) SetEgress(r *egress.Resolver, owner func(session string) string) {
	c.egressResolver = r
	c.sessionOwner = owner
}

// clientFor 取该会话所属账号应使用的 client；未启用/未知会话时回落默认出口。
func (c *Client) clientFor(session string) *http.Client {
	if c == nil || c.egressResolver == nil || !c.egressResolver.Enabled() || c.sessionOwner == nil {
		return c.HTTP
	}
	name := c.sessionOwner(session)
	if name == "" {
		return c.HTTP
	}
	ip := c.egressResolver.IP(name)
	if ip == nil {
		return c.HTTP
	}
	return c.clients.Get(ip)
}

// Headers 生成上游请求头。
func (c *Client) Headers(session string, extra map[string]string) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+session)
	// 双写：客户端 provider 用 useSessionAuth，上游两种拼写都接受。
	h.Set("token", session)
	h.Set("traceparent", "00-"+account.RandomHex(16)+"-"+account.RandomHex(8)+"-01")
	if c.Version != "" {
		h.Set("loomy-version", c.Version)
	}
	for k, v := range extra {
		if v != "" {
			h.Set(k, v)
		}
	}
	return h
}

// Response 一次上游响应的封装。
type Response struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

// JSON 读取完整响应体。
func (r *Response) JSON() (map[string]any, error) {
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("上游响应不是 JSON（HTTP %d）: %s", r.Status, truncate(string(raw), 200))
	}
	return out, nil
}

// BodyText 读取完整响应体为字符串。
func (r *Response) BodyText() string {
	defer r.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	return string(raw)
}

// Do 发一次上游请求。
func (c *Client) Do(ctx context.Context, method, suffix, session string, payload any, extra map[string]string) (*Response, error) {
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(string(buf))
	}
	url := c.BaseURL + "/" + strings.TrimLeft(suffix, "/")
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header = c.Headers(session, extra)

	resp, err := c.clientFor(session).Do(req)
	if err != nil {
		return nil, err
	}
	// 上游把业务错误（如登录态失效）塞进 200 的 body 里，状态码仍是 200。
	// 流式响应不能预读，其余预读整包后按错误信封改判状态码，交给调用方的
	// 状态码分类逻辑处理，避免错误被当成成功放行。
	if resp.StatusCode == http.StatusOK && !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		status := resp.StatusCode
		if errEnvelope(raw) {
			status = http.StatusBadGateway
		}
		return &Response{Status: status, Header: resp.Header, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

const maxBodyBytes = 32 << 20

// errEnvelope 判定「HTTP 200 + 业务错误」响应：顶层有非零 code、且没有正常
// 响应的数据键（choices/output/object）。错误信封自己也带 data:{}，故不排除 data。
//
// 成功码必须按上游真实取值判断：至少有两种形态（抓包核对 2026-10-21）
//   - 模型网关 /points/*：code = "000000"（"成功"）
//   - 部分端点：code = 0 / 200 / "success" / "ok"
//
// 早期只认 "0" 和 "200"，导致所有 code="000000" 的成功响应被误判成
// 业务错误、状态码被改判 502，额度刷新与积分查询整条链路永久失败。
func errEnvelope(raw []byte) bool {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return false
	}
	for _, k := range []string{"choices", "output", "object"} {
		if _, ok := obj[k]; ok {
			return false
		}
	}
	code, ok := obj["code"]
	if !ok {
		return false
	}
	s := strings.TrimSpace(fmt.Sprint(code))
	// 去掉前导零后判零，"000000" / "0" / 0 都算成功。
	trimmed := strings.TrimLeft(s, "0")
	switch {
	case trimmed == "": // "0" / "000000"
		return false
	case trimmed == "200": // "200"
		return false
	case s == "success", s == "ok": // 文本成功码
		return false
	case strings.EqualFold(s, "success"), strings.EqualFold(s, "ok"):
		return false
	}
	return true
}

// GetJSON 便捷：GET 并解析 JSON。
func (c *Client) GetJSON(ctx context.Context, suffix, session string) (map[string]any, int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	resp, err := c.Do(ctx, http.MethodGet, suffix, session, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.Status, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{"_raw": string(raw)}, resp.Status, nil
	}
	return out, resp.Status, nil
}

// PostJSON 便捷：POST 并解析 JSON（状态码非 2xx 也解析，交给调用方分类）。
func (c *Client) PostJSON(ctx context.Context, suffix, session string, payload any, extra map[string]string) (map[string]any, int, error) {
	resp, err := c.Do(ctx, http.MethodPost, suffix, session, payload, extra)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.Status, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{"_raw": string(raw)}, resp.Status, nil
	}
	return out, resp.Status, nil
}

// ------------------------------------------------------------------ 端点

// Models 拉取模型目录。
func (c *Client) Models(ctx context.Context, session string) ([]config.Model, error) {
	resp, err := c.Do(ctx, http.MethodGet, "models", session, nil, nil)
	if err != nil {
		return nil, err
	}
	obj, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("/models HTTP %d", resp.Status)
	}
	list, _ := obj["data"].([]any)
	if list == nil {
		list, _ = obj["models"].([]any)
	}
	out := make([]config.Model, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		model := config.Model{
			ID:               str(m["id"]),
			Name:             str(m["name"]),
			Multiplier:       num(m["multiplier"]),
			ContextLength:    int(num(m["context_length"])),
			MaxOutput:        int(num(m["max_output_tokens"])),
			Type:             str(m["type"]),
			Protocol:         str(m["protocol"]),
			IsDefault:        boolOf(m["is_default_model"]),
			DefaultReasoning: str(m["default_reasoning_effort"]),
		}
		// 倍率/限免没有结构化字段，只能从展示名解析（见 reModelMult 注释）。
		if mm := reModelMult.FindStringSubmatch(model.Name); mm != nil {
			if f, err := strconv.ParseFloat(mm[1], 64); err == nil {
				model.Multiplier = f
			}
			model.IsFree = model.Multiplier == 0
		}
		if reModelFree.MatchString(model.Name) {
			model.IsFree = true
		}
		if caps, ok := m["capabilities"].(map[string]any); ok {
			for k, v := range caps {
				if b, _ := v.(bool); b {
					model.Capabilities = append(model.Capabilities, k)
				}
			}
		}
		if efforts, ok := m["reasoning_efforts"].([]any); ok {
			for _, e := range efforts {
				model.ReasoningTiers = append(model.ReasoningTiers, str(e))
			}
		}
		if model.ID != "" {
			out = append(out, model)
		}
	}
	return out, nil
}

// Quota 额度快照。字段全部来自上游真实响应，不做本地推算：
//
//	GET  points/records      → balance / dailyBalance / availableBalance
//	POST points/first-login  → permanentBalance / dailyQuota / dailyConsumed / dailyCycleDate
//
// 为什么必须同时打两个端点：points/records 只回三个余额字段，dailyQuota 与
// dailyConsumed 只有 first-login 才给。早期版本只读 points/records，导致
// 「每日赠送额度」与「当日已用」始终为空（面板显示 ——）。
//
// 口径（上游明确）：每日赠送额度登录后刷新为 5000；扣费时先扣活动/兑换码
// 来源的积分，再扣普通长期积分（permanentBalance）。
type Quota struct {
	Balance     *int64 // 普通长期积分余额（points/records.balance）
	Daily       *int64 // 每日赠送剩余（points/records.dailyBalance）
	Available   *int64 // 可用总额（points/records.availableBalance）
	Permanent   *int64 // 长期积分（first-login.permanentBalance）
	DailyQuota  *int64 // 每日赠送上限，通常 5000（first-login.dailyQuota）
	DailyUsed   *int64 // 当日已消耗（first-login.dailyConsumed）
	CycleDate   string // 每日额度所属日期，形如 2026-10-21（first-login.dailyCycleDate）
	RefreshedAt int64  // 快照取得时间（unix 秒）
}

// available 取可用总额；上游没给 availableBalance 时按 balance+daily 估。
func (q *Quota) available() *int64 {
	if q == nil {
		return nil
	}
	if q.Available != nil {
		return q.Available
	}
	if q.Balance == nil && q.Daily == nil {
		return nil
	}
	var sum int64
	if q.Balance != nil {
		sum += *q.Balance
	}
	if q.Daily != nil {
		sum += *q.Daily
	}
	return &sum
}

// dailyRemaining 每日赠送剩余：优先用上游 dailyBalance，
// 否则用 dailyQuota - dailyConsumed 推算（不会小于 0）。
func (q *Quota) dailyRemaining() *int64 {
	if q == nil {
		return nil
	}
	if q.Daily != nil {
		return q.Daily
	}
	if q.DailyQuota == nil {
		return nil
	}
	used := int64(0)
	if q.DailyUsed != nil {
		used = *q.DailyUsed
	}
	left := *q.DailyQuota - used
	if left < 0 {
		left = 0
	}
	return &left
}

// Quota 查询积分余额。上游只认 camelCase 的 pageSize（下划线会被静默忽略）。
//
// 两个端点都打：records 给余额，first-login 给每日额度口径。first-login 是
// 幂等的（已领过回 alreadyProcessed=true），顺带完成「每日登录刷新额度」。
func (c *Client) Quota(ctx context.Context, session string) (*Quota, error) {
	q := &Quota{}
	// 该端点上游只挂 GET，POST 会直接 404（额度刷新曾因此永久失败）。
	suffix := "points/records?record_type=all&pageNo=1&pageSize=1"
	obj, status, err := c.GetJSON(ctx, suffix, session)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("/points/records HTTP %d", status)
	}
	if data, _ := obj["data"].(map[string]any); data != nil {
		q.Balance = ptrI64(data["balance"])
		q.Daily = ptrI64(data["dailyBalance"])
		q.Available = ptrI64(data["availableBalance"])
	}

	// 每日额度明细。失败不致命：余额已经拿到，面板退化成只显示余额。
	if fl, err := c.FirstLogin(ctx, session); err == nil {
		// FirstLogin 返回的是完整信封，字段在 data 里。
		data := mapOfAny(fl["data"])
		if data == nil {
			data = fl
		}
		q.Permanent = ptrI64(data["permanentBalance"])
		q.DailyQuota = ptrI64(data["dailyQuota"])
		q.DailyUsed = ptrI64(data["dailyConsumed"])
		q.CycleDate = str(data["dailyCycleDate"])
		if q.Balance == nil {
			q.Balance = ptrI64(data["permanentBalance"])
		}
		if q.Daily == nil {
			q.Daily = ptrI64(data["dailyBalance"])
		}
		if q.Available == nil {
			q.Available = ptrI64(data["currentBalance"])
		}
	}
	q.RefreshedAt = time.Now().Unix()
	return q, nil
}

// Points 查询积分流水汇总（面板额度页）。
func (c *Client) Points(ctx context.Context, session string, pages int) (map[string]any, error) {
	out, err := c.Records(ctx, session, pages)
	if err != nil {
		return nil, err
	}
	// dailyQuota / dailyConsumed 不在 points/records 响应里，只有 first-login 提供。
	// 这里只调 first-login（不调 Quota，否则会重复请求一轮 records）。
	if fl, ferr := c.FirstLogin(ctx, session); ferr == nil {
		data := mapOfAny(fl["data"])
		if data == nil {
			data = fl
		}
		if v := ptrI64(data["dailyQuota"]); v != nil {
			out["daily_quota"] = *v
		}
		if v := ptrI64(data["dailyConsumed"]); v != nil {
			out["daily_consumed"] = *v
		}
		if v := ptrI64(data["permanentBalance"]); v != nil {
			out["permanent"] = *v
		}
		out["daily_cycle_date"] = str(data["dailyCycleDate"])
	}
	return out, nil
}

// Records 只取积分流水，**不触发** first-login。
//
// Points 会在取完流水后再调一次 points/first-login（为了拿 dailyQuota）——
// 那是个有副作用的 POST（领每日奖励），面板的「积分账本」不需要它，也不该
// 为了展示表格去反复触发奖励接口（121 个账号 × 每轮一次）。
func (c *Client) Records(ctx context.Context, session string, pages int) (map[string]any, error) {
	if pages < 1 {
		pages = 5
	}
	var records []map[string]any
	var first map[string]any
	seen := map[string]bool{}
	var debits, credits int64
	for p := 1; p <= pages; p++ {
		suffix := fmt.Sprintf("points/records?record_type=all&pageNo=%d&pageSize=100", p)
		obj, status, err := c.GetJSON(ctx, suffix, session)
		if err != nil {
			return nil, err
		}
		if status != 200 {
			return nil, fmt.Errorf("/points/records HTTP %d", status)
		}
		data, _ := obj["data"].(map[string]any)
		if data == nil {
			break
		}
		if p == 1 {
			first = data
		}
		list, _ := data["list"].([]any)
		if list == nil {
			list, _ = data["records"].([]any)
		}
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			key := str(m["ledgerId"])
			if key == "" {
				key = str(m["createdAt"]) + str(m["modelName"]) + str(m["pointsActual"])
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			dir := str(m["direction"])
			pts := int64(num(m["pointsActual"]))
			switch dir {
			case "debit":
				debits += pts
			case "credit":
				credits += pts
			}
			records = append(records, map[string]any{
				"direction":     dir,
				"source":        str(m["consumeSource"]),
				"model":         str(m["modelName"]),
				"points":        pts,
				"balance_after": m["balanceAfter"],
				"time":          str(m["createdAt"]),
				"desc":          truncate(str(m["description"]), 80),
			})
		}
		if len(list) < 100 {
			break
		}
	}
	if first == nil {
		first = map[string]any{}
	}
	return map[string]any{
		"balance":       first["balance"],
		"daily_balance": first["dailyBalance"],
		"available":     first["availableBalance"],
		"records":       records,
		"debits":        debits,
		"credits":       credits,
		"pages":         pages,
	}, nil
}

// FirstLogin 触发每日首次登录奖励（幂等：已领过返回 alreadyProcessed）。
func (c *Client) FirstLogin(ctx context.Context, session string) (map[string]any, error) {
	obj, status, err := c.PostJSON(ctx, "points/first-login", session, map[string]any{}, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("/points/first-login HTTP %d", status)
	}
	return obj, nil
}

// CheckinResult 一次「签到/领取每日积分」的结构化结果（抓包核对过的字段）。
type CheckinResult struct {
	AlreadyProcessed bool             // 本周期已领过（重复调用不会重复给分）
	CurrentBalance   int64            // 当前可用总额
	PermanentBalance int64            // 普通长期积分
	DailyBalance     int64            // 每日赠送剩余
	DailyQuota       int64            // 每日赠送上限（登录后自动刷新为 5000）
	DailyConsumed    int64            // 当日已消耗
	CycleDate        string           // 每日额度所属日期
	InvitationCodes  []map[string]any // 邀请码列表（inviteCode/usedCount/maxUses/status）
	Quota            *Quota           // 可直接喂给 pool.Account.SetQuota 的快照
}

// Checkin 模拟客户端每日登录领积分：打的正是官方客户端每次启动都会调的
// POST /points/first-login（幂等）。口径（官方说明）：
//   - 每日赠送积分在每日登录后自动刷新为 5000；
//   - 消耗顺序：活动/兑换码积分 → 普通长期积分（上游侧执行，网关只镜像展示）。
func (c *Client) Checkin(ctx context.Context, session string) (*CheckinResult, error) {
	obj, err := c.FirstLogin(ctx, session)
	if err != nil {
		return nil, err
	}
	data := mapOfAny(obj["data"])
	if data == nil {
		data = obj
	}
	res := &CheckinResult{
		AlreadyProcessed: boolOf(data["alreadyProcessed"]),
		CurrentBalance:   int64(num(data["currentBalance"])),
		PermanentBalance: int64(num(data["permanentBalance"])),
		DailyBalance:     int64(num(data["dailyBalance"])),
		DailyQuota:       int64(num(data["dailyQuota"])),
		DailyConsumed:    int64(num(data["dailyConsumed"])),
		CycleDate:        str(data["dailyCycleDate"]),
	}
	if list, ok := data["invitationCodes"].([]any); ok {
		for _, it := range list {
			if m, ok := it.(map[string]any); ok {
				res.InvitationCodes = append(res.InvitationCodes, m)
			}
		}
	}
	// first-login 自带全部额度口径，直接构造快照（不必再打 records）。
	res.Quota = &Quota{
		Balance:     ptrI64(data["permanentBalance"]),
		Daily:       ptrI64(data["dailyBalance"]),
		Available:   ptrI64(data["currentBalance"]),
		Permanent:   ptrI64(data["permanentBalance"]),
		DailyQuota:  ptrI64(data["dailyQuota"]),
		DailyUsed:   ptrI64(data["dailyConsumed"]),
		CycleDate:   res.CycleDate,
		RefreshedAt: time.Now().Unix(),
	}
	return res, nil
}

// ---------------------------------------------------------------- 新手任务

// OnboardingPoints 新手任务 key → 积分（合计 10000）。
//
// 来源：Loomy 客户端内置的 TASK_POINTS 表（8 项之和 = 10000）。服务端响应里的
// earned 是**累计已领**、不是本次增量，且官方客户端自己也按本地表现算，
// 所以这里一律按表计分，不采信响应体的数字（只采信 alreadyCompleted 判幂等）。
var OnboardingPoints = map[string]int64{
	"first_message":    500,
	"pick_skill":       1000,
	"generate_ppt":     1500,
	"set_schedule":     1000,
	"install_skill":    1500,
	"configure_remote": 1000,
	"create_soul":      1500,
	"share_soul":       2000,
}

// OnboardingOrder 任务的展示顺序（与官方客户端任务列表一致）。
//
// map 的遍历顺序是随机的，面板「任务中心」要按固定顺序渲染成矩阵，否则每次刷新
// 列位置都在跳——用户没法对比两个账号的同一项任务。
var OnboardingOrder = []string{
	"first_message", "pick_skill", "generate_ppt", "set_schedule",
	"install_skill", "configure_remote", "create_soul", "share_soul",
}

// OnboardingTitles 任务 key 的中文标题（面板展示用）。
var OnboardingTitles = map[string]string{
	"first_message":    "发送你的第一条消息",
	"pick_skill":       "试试选择一个技能",
	"generate_ppt":     "生成第一份 PPT",
	"set_schedule":     "设置定时任务",
	"install_skill":    "在技能广场安装一个技能",
	"configure_remote": "配置远程控制",
	"create_soul":      "创建你的第一个搭子",
	"share_soul":       "把搭子分享给朋友",
}

// OnboardingTotal 全部新手任务积分合计。
const OnboardingTotal int64 = 10000

// OnboardingTasks GET /onboarding/tasks：8 个任务的完成状态。
//
// 返回的 tasks 已按本地表归一（缺的补 false、服务端多给的忽略），earned 按本地
// 表现算 —— 这样面板与「本次领了多少」永远自洽。
func (c *Client) OnboardingTasks(ctx context.Context, session string) (map[string]bool, int64, error) {
	obj, status, err := c.GetJSON(ctx, "onboarding/tasks", session)
	if err != nil {
		return nil, 0, err
	}
	if status != 200 {
		return nil, 0, fmt.Errorf("/onboarding/tasks HTTP %d", status)
	}
	data := mapOfAny(obj["data"])
	if data == nil {
		data = obj
	}
	raw, _ := data["tasks"].(map[string]any)
	tasks := make(map[string]bool, len(OnboardingPoints))
	var earned int64
	for key := range OnboardingPoints {
		done := false
		if raw != nil {
			done = boolOf(raw[key])
		}
		tasks[key] = done
		if done {
			earned += OnboardingPoints[key]
		}
	}
	return tasks, earned, nil
}

// OnboardingComplete POST /onboarding/tasks/complete，body 只有 {"key":...}。
//
// 服务端**不校验前置行为**（实测：不真的发对话/生成 PPT 也能拿满 10000），
// 且幂等判据是响应体的 alreadyCompleted —— 不是 HTTP 码、不是信封里的 code。
// 返回 already=true 表示此前已完成（本次没加分）。
func (c *Client) OnboardingComplete(ctx context.Context, session, key string) (already bool, balance int64, err error) {
	obj, status, err := c.PostJSON(ctx, "onboarding/tasks/complete", session,
		map[string]any{"key": key}, nil)
	if err != nil {
		return false, 0, err
	}
	if status != 200 {
		return false, 0, fmt.Errorf("/onboarding/tasks/complete HTTP %d", status)
	}
	data := mapOfAny(obj["data"])
	if data == nil {
		data = obj
	}
	return boolOf(data["alreadyCompleted"]), int64(num(data["balance"])), nil
}

// ClaimOnboarding 领完所有未完成的新手任务，返回本次新领的任务与积分。
//
// 先 GET 一次状态，只对未完成的 key 发 complete —— 已完成的重复 POST 虽然幂等，
// 但每次 8 个请求没必要（这个方法会被签到链路定期调到）。
func (c *Client) ClaimOnboarding(ctx context.Context, session string) (claimed []string, points int64, err error) {
	tasks, _, err := c.OnboardingTasks(ctx, session)
	if err != nil {
		return nil, 0, err
	}
	for key, done := range tasks {
		if done {
			continue
		}
		already, _, cerr := c.OnboardingComplete(ctx, session, key)
		if cerr != nil {
			// 单个任务失败不放弃其余：8 个任务各自独立结算。
			continue
		}
		if already {
			continue
		}
		claimed = append(claimed, key)
		points += OnboardingPoints[key]
	}
	sort.Strings(claimed)
	return claimed, points, nil
}

// Search 内置联网检索（腾讯搜索通道）。
func (c *Client) Search(ctx context.Context, session string, payload map[string]any) (map[string]any, int, error) {
	return c.PostJSON(ctx, "search/tencent", session, payload, nil)
}

// ---------------------------------------------------------------- helpers

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case json.Number:
		return t.String()
	}
	return ""
}

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	case int:
		return float64(t)
	case string:
		var f float64
		_, _ = fmt.Sscanf(t, "%f", &f)
		return f
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}

func ptrI64(v any) *int64 {
	if v == nil {
		return nil
	}
	n := int64(num(v))
	return &n
}

// boolOf 宽松取布尔（兼容 JSON true/false 与 "true" 字符串）。
func boolOf(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	}
	return false
}

// mapOfAny 安全取嵌套对象（缺失或类型不符返回 nil）。
func mapOfAny(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------------------------------------------------------------- 邀请码

// 事实来源（2026-10-23 客户端 electron/points-service.js + 实测抓包）：
//
//	GET  /api/v1/invitation-codes   → {list:[{inviteCode,usedCount,maxUses,status}]}
//	GET  /api/v1/points/activation  → {activated, appliedInvitationCode}
//	POST /api/v1/points/activation  → body {inviteCode, deviceId}
//
// 关键约束：**一个账号只能绑一次别人的邀请码**（绑过再提交回
// alreadyActivated=true 且不再给分），所以「绑定」是不可逆的一次性资源；
// 而奖励是双边的（inviteeReward / inviterReward 各 5000）。
// 网关侧因此必须先把状态查清楚再动手，不能靠重试兜底。

// InviteCode 账号自持的一个邀请码。
type InviteCode struct {
	Code      string `json:"inviteCode"`
	UsedCount int64  `json:"usedCount"`
	MaxUses   int64  `json:"maxUses"`
	Status    string `json:"status"`
}

// Activation 账号的邀请码绑定状态（GET 与 POST 响应同形）。
type Activation struct {
	Activated        bool   // 已绑定（true 后不可再绑别人的码）
	AlreadyActivated bool   // 本次提交前就已绑过（幂等判据）
	AppliedCode      string // 绑的是哪个码
	InviteeReward    int64  // 本次作为「被邀方」到账积分
	InviterReward    int64  // 本次作为「邀请方」到账积分（绑别人的码时通常为 0）
	CurrentBalance   int64
	PermanentBalance int64
	Codes            []InviteCode // 绑定后刷新出的自持码列表
}

// parseInviteCodes 归一 invitationCodes / list 两种拼写（GET 用 list、POST 用 invitationsCodes）。
func parseInviteCodes(raw any) []InviteCode {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]InviteCode, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, InviteCode{
			Code:      str(m["inviteCode"]),
			UsedCount: int64(num(m["usedCount"])),
			MaxUses:   int64(num(m["maxUses"])),
			Status:    str(m["status"]),
		})
	}
	return out
}

// parseActivation 归一 activation 响应。空 data 不算错误：上游对「从未绑过」
// 只回 {activated:false}，此时 appliedInvitationCode 缺失。
func parseActivation(obj map[string]any) *Activation {
	data := mapOfAny(obj["data"])
	if data == nil {
		data = obj
	}
	return &Activation{
		Activated:        boolOf(data["activated"]),
		AlreadyActivated: boolOf(data["alreadyActivated"]),
		AppliedCode:      str(data["appliedInvitationCode"]),
		InviteeReward:    int64(num(data["inviteeReward"])),
		InviterReward:    int64(num(data["inviterReward"])),
		CurrentBalance:   int64(num(data["currentBalance"])),
		PermanentBalance: int64(num(data["permanentBalance"])),
		Codes:            parseInviteCodes(data["invitationCodes"]),
	}
}

// InvitationCodes GET /api/v1/invitation-codes：账号自持的邀请码列表。
// 每个号注册后自带 1 个 active 码（maxUses 默认 1000）；没有码时回空切片而非报错。
func (c *Client) InvitationCodes(ctx context.Context, session string) ([]InviteCode, error) {
	obj, status, err := c.GetJSON(ctx, "invitation-codes", session)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("/invitation-codes HTTP %d", status)
	}
	data := mapOfAny(obj["data"])
	if data == nil {
		data = obj
	}
	return parseInviteCodes(data["list"]), nil
}

// Activation GET /api/v1/points/activation：该号绑过谁的码 / 有没有绑过。
func (c *Client) Activation(ctx context.Context, session string) (*Activation, error) {
	obj, status, err := c.GetJSON(ctx, "points/activation", session)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("/points/activation HTTP %d", status)
	}
	return parseActivation(obj), nil
}

// BindInviteCode POST /api/v1/points/activation：把 code 绑到该号。
//
// deviceId 传账号自己的 campus_device_id（account.Identity.CampusDev）：
// 上游用它做设备维度归因，缺省也能成功，但每号独立的值才是客户端的真实形态
// （一台机器一个 deviceId，账号池里一机一号）。
//
// 这是不可逆操作：成功后该号永久 activated。调用方必须先查 Activation。
func (c *Client) BindInviteCode(ctx context.Context, session, code, deviceID string) (*Activation, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, fmt.Errorf("邀请码不能为空")
	}
	body := map[string]any{"inviteCode": code}
	if deviceID = strings.TrimSpace(deviceID); deviceID != "" {
		body["deviceId"] = deviceID
	}
	obj, status, err := c.PostJSON(ctx, "points/activation", session, body, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		// 业务失败（码满 / 码失效 / 这个号已绑过）被上游塞进 200 的 body，
		// Do 按错误信封改判 502。把 desc 带出来：调用方要靠它区分
		// 「这个码废了，换一个」和「网络抖了，下次再来」——两者处理完全相反。
		if d := str(obj["desc"]); d != "" {
			return nil, &InviteError{Desc: d}
		}
		return nil, fmt.Errorf("/points/activation HTTP %d", status)
	}
	return parseActivation(obj), nil
}

// InviteError 绑定邀请码的业务失败（区别于网络/HTTP 错误）。
//
// 用类型而不是字符串匹配来让调用方分支：网络错误重试有意义，
// 业务失败重试只是把同一条错误再打一遍。
type InviteError struct{ Desc string }

func (e *InviteError) Error() string { return e.Desc }

// InviteRejectedCode 「这次失败是不是码本身不能再用了」。
//
// 上游没有结构化错误码，只能看中文 desc：提到「已绑」的是这个号自己
// 已经有邀请码了（码没问题，号没救），其余一律按码的问题处理——
// 误判的代价只是少用一个码（池子里还有一百多个），不误判的代价是
// 每次加号都拿一个废码去撞，永远绑不上。
func (e *InviteError) InviteRejectedCode() bool {
	d := e.Desc
	return !strings.Contains(d, "已绑") && !strings.Contains(d, "已经绑定")
}
