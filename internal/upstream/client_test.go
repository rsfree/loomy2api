package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuanjie-ai/loomy2api/internal/config"
)

// TestHeadersRequired 三个必需头齐全且 Authorization 双写。
func TestHeadersRequired(t *testing.T) {
	c := NewClient("https://example.com/api/v1", "0.9.38", "", 10*time.Second)
	h := c.Headers("sess-abc", nil)

	if got := h.Get("Authorization"); got != "Bearer sess-abc" {
		t.Errorf("Authorization = %q", got)
	}
	if got := h.Get("token"); got != "sess-abc" {
		t.Errorf("token = %q（useSessionAuth 双写）", got)
	}
	if got := h.Get("loomy-version"); got != "0.9.38" {
		t.Errorf("loomy-version = %q", got)
	}
	tp := h.Get("traceparent")
	parts := strings.Split(tp, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 {
		t.Errorf("traceparent 格式不对: %q（缺它上游会挂死到超时）", tp)
	}
	if parts[3] != "01" {
		t.Errorf("traceparent 采样标志 = %q", parts[3])
	}
}

// TestHeadersTraceparentUnique 每次请求的 traceparent 必须不同。
func TestHeadersTraceparentUnique(t *testing.T) {
	c := NewClient("https://example.com", "1.0.0", "", time.Second)
	a := c.Headers("s", nil).Get("traceparent")
	b := c.Headers("s", nil).Get("traceparent")
	if a == b {
		t.Error("traceparent 应每请求重新生成")
	}
}

// TestHeadersExtraOverrides 额外头能覆盖默认值。
func TestHeadersExtraOverrides(t *testing.T) {
	c := NewClient("https://example.com", "1.0.0", "", time.Second)
	h := c.Headers("s", map[string]string{"ChatId": "c1", "traceparent": "custom"})
	if h.Get("ChatId") != "c1" {
		t.Errorf("ChatId = %q", h.Get("ChatId"))
	}
	if h.Get("traceparent") != "custom" {
		t.Errorf("额外头应可覆盖 traceparent，实际 %q", h.Get("traceparent"))
	}
}

// TestHeadersSkipsEmptyExtra 空值额外头不应写入。
func TestHeadersSkipsEmptyExtra(t *testing.T) {
	c := NewClient("https://example.com", "1.0.0", "", time.Second)
	h := c.Headers("s", map[string]string{"ChatId": ""})
	if h.Get("ChatId") != "" {
		t.Error("空值额外头不应写入")
	}
}

// TestModelsParsing /models 解析成模型目录。
func TestModelsParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":[
			{"id":"imodel/spark-x","name":"Spark X2.5","multiplier":0.1,
			 "context_length":1048576,"max_output_tokens":8192,
			 "capabilities":{"text":true,"image":true,"audio":false},
			 "reasoning_efforts":["low","high"]}
		]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	models, err := c.Models(context.Background(), "sess")
	if err != nil {
		t.Fatalf("/models 失败: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("模型数 %d", len(models))
	}
	m := models[0]
	if m.ID != "imodel/spark-x" || m.Name != "Spark X2.5" {
		t.Errorf("模型元信息不对: %+v", m)
	}
	if m.Multiplier != 0.1 || m.ContextLength != 1048576 || m.MaxOutput != 8192 {
		t.Errorf("数值字段不对: %+v", m)
	}
	// 只收集 true 的能力
	if len(m.Capabilities) != 2 {
		t.Errorf("能力项应只收 true 的: %v", m.Capabilities)
	}
	if len(m.ReasoningTiers) != 2 {
		t.Errorf("思考档位未解析: %v", m.ReasoningTiers)
	}
}

// TestModelsNon200 /models 非 200 应返回错误。
func TestModelsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	if _, err := c.Models(context.Background(), "s"); err == nil {
		t.Error("非 200 应返回错误")
	}
}

// TestQuotaCamelCasePageSize 上游只认 camelCase 的 pageSize。
func TestQuotaCamelCasePageSize(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "first-login") {
			_, _ = io.WriteString(w, `{"data":{"alreadyProcessed":true}}`)
			return
		}
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"data":{"balance":100,"dailyBalance":20,"availableBalance":120}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	q, err := c.Quota(context.Background(), "s")
	if err != nil {
		t.Fatalf("Quota 失败: %v", err)
	}
	if !strings.Contains(gotQuery, "pageSize=") {
		t.Errorf("必须用 camelCase pageSize（下划线会被静默忽略）: %q", gotQuery)
	}
	if strings.Contains(gotQuery, "page_size=") {
		t.Errorf("不应使用 page_size: %q", gotQuery)
	}
	if q.Balance == nil || *q.Balance != 100 {
		t.Errorf("Balance = %v", q.Balance)
	}
	if q.Daily == nil || *q.Daily != 20 {
		t.Errorf("Daily = %v", q.Daily)
	}
	if a := q.available(); a == nil || *a != 120 {
		t.Errorf("available = %v", a)
	}
}

// TestQuotaDerivesAvailable 上游没给 availableBalance 时用 balance+daily 估算。
func TestQuotaDerivesAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "first-login") {
			_, _ = io.WriteString(w, `{"data":{"alreadyProcessed":true}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"balance":70,"dailyBalance":30}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	q, err := c.Quota(context.Background(), "s")
	if err != nil {
		t.Fatalf("Quota 失败: %v", err)
	}
	if a := q.available(); a == nil || *a != 100 {
		t.Errorf("available 应为 100，实际 %v", a)
	}
}

// TestQuotaEmptyData 空 data 不 panic。
func TestQuotaEmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	q, err := c.Quota(context.Background(), "s")
	if err != nil {
		t.Fatalf("空 data 不应报错: %v", err)
	}
	if q.Balance != nil || q.Daily != nil || q.available() != nil {
		t.Errorf("空 data 时应返回 nil: %v %v %v", q.Balance, q.Daily, q.available())
	}
}

// TestCheckinParsesRealSample Checkin 把抓包样本解析成结构化结果，
// 并给出可直接落池的 Quota 快照。样本为 2026-10-21 真实响应。
func TestCheckinParsesRealSample(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"code":"000000","desc":"成功","trace_id":"bf5c","data":{"alreadyProcessed":true,
			"currentBalance":10000,"permanentBalance":5000,"dailyBalance":5000,"dailyQuota":5000,
			"dailyConsumed":0,"dailyCycleDate":"2026-10-21","registerReward":0,
			"invitationCodes":[{"inviteCode":"FWF6D5","usedCount":0,"maxUses":1000,"status":"active"}]}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	res, err := c.Checkin(context.Background(), "sess")
	if err != nil {
		t.Fatalf("Checkin 失败: %v", err)
	}
	if !strings.Contains(gotBody, "{}") {
		t.Errorf("first-login 请求体应为空对象，实际 %q", gotBody)
	}
	if !res.AlreadyProcessed {
		t.Error("alreadyProcessed 应解析为 true")
	}
	if res.CurrentBalance != 10000 || res.PermanentBalance != 5000 || res.DailyBalance != 5000 {
		t.Errorf("余额口径不对: %+v", res)
	}
	if res.DailyQuota != 5000 || res.DailyConsumed != 0 {
		t.Errorf("每日额度口径不对: quota=%d consumed=%d", res.DailyQuota, res.DailyConsumed)
	}
	if res.CycleDate != "2026-10-21" {
		t.Errorf("CycleDate = %q", res.CycleDate)
	}
	if len(res.InvitationCodes) != 1 || res.InvitationCodes[0]["inviteCode"] != "FWF6D5" {
		t.Errorf("邀请码未解析: %v", res.InvitationCodes)
	}
	q := res.Quota
	if q == nil || q.Available == nil || *q.Available != 10000 || q.DailyQuota == nil || *q.DailyQuota != 5000 {
		t.Fatalf("Quota 快照不对: %+v", q)
	}
	if q.Permanent == nil || *q.Permanent != 5000 || q.DailyUsed == nil || *q.DailyUsed != 0 {
		t.Errorf("Quota 快照字段不全: %+v", q)
	}
	if q.CycleDate != "2026-10-21" || q.RefreshedAt == 0 {
		t.Errorf("Quota 快照日期/时间戳不对: %q %d", q.CycleDate, q.RefreshedAt)
	}
}

// TestCheckinErrorEnvelope 业务错误码必须报错而不是当成签到成功。
func TestCheckinErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"100002","desc":"缺少 token","data":{}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	if _, err := c.Checkin(context.Background(), ""); err == nil {
		t.Fatal("401 信封必须返回错误")
	}
}

// TestQuotaDailyFromFirstLogin 每日额度口径来自 points/first-login：
// points/records 不返回 dailyQuota / dailyConsumed，必须由 first-login 补齐。
// 真实响应样本见 docs/PROTOCOL.md（2026-10-21 抓包）。
func TestQuotaDailyFromFirstLogin(t *testing.T) {
	var hitFirstLogin bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "first-login") {
			hitFirstLogin = true
			if r.Method != http.MethodPost {
				t.Errorf("first-login 必须是 POST，实际 %s", r.Method)
			}
			_, _ = io.WriteString(w, `{"code":"000000","data":{"alreadyProcessed":true,
				"currentBalance":10000,"permanentBalance":5000,"dailyBalance":5000,
				"dailyQuota":5000,"dailyConsumed":0,"dailyCycleDate":"2026-10-21"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":"000000","data":{"balance":5000,"dailyBalance":5000,"availableBalance":10000}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	q, err := c.Quota(context.Background(), "s")
	if err != nil {
		t.Fatalf("Quota 失败: %v", err)
	}
	if !hitFirstLogin {
		t.Error("必须调用 points/first-login 才能拿到每日额度口径")
	}
	if q.DailyQuota == nil || *q.DailyQuota != 5000 {
		t.Errorf("DailyQuota 应为 5000，实际 %v", q.DailyQuota)
	}
	if q.DailyUsed == nil || *q.DailyUsed != 0 {
		t.Errorf("DailyUsed 应为 0，实际 %v", q.DailyUsed)
	}
	if q.Permanent == nil || *q.Permanent != 5000 {
		t.Errorf("Permanent 应为 5000，实际 %v", q.Permanent)
	}
	if q.CycleDate != "2026-10-21" {
		t.Errorf("CycleDate = %q", q.CycleDate)
	}
	if r := q.dailyRemaining(); r == nil || *r != 5000 {
		t.Errorf("每日剩余应为 5000，实际 %v", r)
	}
	if q.RefreshedAt == 0 {
		t.Error("RefreshedAt 未设置")
	}
}

// TestQuotaDailyRemainingFallsBackToQuotaMinusUsed dailyBalance 缺失时
// 用 dailyQuota - dailyConsumed 推算，且不会为负。
func TestQuotaDailyRemainingFallsBackToQuotaMinusUsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "first-login") {
			_, _ = io.WriteString(w, `{"data":{"dailyQuota":5000,"dailyConsumed":1200}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"balance":300}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	q, err := c.Quota(context.Background(), "s")
	if err != nil {
		t.Fatalf("Quota 失败: %v", err)
	}
	if r := q.dailyRemaining(); r == nil || *r != 3800 {
		t.Errorf("每日剩余应为 3800，实际 %v", r)
	}

	over := &Quota{DailyQuota: ptrI64(5000), DailyUsed: ptrI64(9000)}
	if r := over.dailyRemaining(); r == nil || *r != 0 {
		t.Errorf("超额消耗时每日剩余应为 0，实际 %v", r)
	}
}

// TestQuotaFirstLoginFailureKeepsBalance first-login 失败不应让整个额度查询失败。
func TestQuotaFirstLoginFailureKeepsBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "first-login") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"balance":42,"dailyBalance":8}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	q, err := c.Quota(context.Background(), "s")
	if err != nil {
		t.Fatalf("first-login 失败不应影响余额: %v", err)
	}
	if q.Balance == nil || *q.Balance != 42 {
		t.Errorf("Balance 应为 42，实际 %v", q.Balance)
	}
	if q.DailyQuota != nil {
		t.Errorf("first-login 失败时 DailyQuota 应为 nil，实际 %v", q.DailyQuota)
	}
}

// TestPointsPaginationAndDedup 积分流水分页 + 去重 + 收支汇总。
func TestPointsPaginationAndDedup(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Points 末尾会补调一次 first-login 取每日额度口径，不计入翻页次数。
		if strings.Contains(r.URL.Path, "first-login") {
			_, _ = io.WriteString(w, `{"data":{"dailyQuota":5000,"dailyConsumed":0}}`)
			return
		}
		calls++
		if calls == 1 {
			// 第一页满 100 条 → 会翻第二页
			items := make([]map[string]any, 0, 100)
			for i := 0; i < 100; i++ {
				items = append(items, map[string]any{
					"ledgerId":  "l" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
					"direction": "debit", "pointsActual": 1, "modelName": "m",
				})
			}
			payload, _ := json.Marshal(map[string]any{"data": map[string]any{
				"balance": 500, "dailyBalance": 10, "availableBalance": 510,
				"list": items,
			}})
			_, _ = w.Write(payload)
			return
		}
		// 第二页：含一条与第一页重复的 ledgerId
		_, _ = io.WriteString(w, `{"data":{"list":[
			{"ledgerId":"dup","direction":"credit","pointsActual":100,"description":"签到"},
			{"ledgerId":"dup","direction":"credit","pointsActual":100,"description":"签到"}
		]}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	out, err := c.Points(context.Background(), "s", 5)
	if err != nil {
		t.Fatalf("Points 失败: %v", err)
	}
	if calls != 2 {
		t.Errorf("应翻 2 页（第一页满 100 条），实际 %d", calls)
	}
	records, _ := out["records"].([]map[string]any)
	if len(records) != 101 {
		t.Errorf("去重后应 101 条（100 + 1 条唯一 credit），实际 %d", len(records))
	}
	if out["credits"].(int64) != 100 {
		t.Errorf("credits = %v，期望 100（重复条目不应累计）", out["credits"])
	}
	if out["balance"] == nil {
		t.Errorf("balance 未返回: %v", out["balance"])
	}
}

// TestSearch 内置检索。
func TestSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/search/tencent") {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"results":[{"title":"x"}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	obj, status, err := c.Search(context.Background(), "s", map[string]any{"query": "天气"})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if status != 200 {
		t.Errorf("状态码 %d", status)
	}
	if obj["results"] == nil {
		t.Errorf("结果未透传: %v", obj)
	}
}

// TestPostJSONNonJSONBody 上游返回非 JSON 时不报错（_raw 兜底，交给错误分类）。
func TestPostJSONNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	obj, status, err := c.PostJSON(context.Background(), "chat/completions", "s", map[string]any{}, nil)
	if err != nil {
		t.Fatalf("非 JSON 不应报错: %v", err)
	}
	if status != 502 {
		t.Errorf("状态码 %d", status)
	}
	if obj["_raw"] == nil {
		t.Errorf("应把原文放进 _raw: %v", obj)
	}
}

// TestGetJSON 语法糖与 Do 等价。
func TestGetJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("方法 = %s", r.Method)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	obj, status, err := c.GetJSON(context.Background(), "models", "s")
	if err != nil {
		t.Fatalf("GetJSON 失败: %v", err)
	}
	if status != 200 || obj["ok"] != true {
		t.Errorf("响应不对: %d %v", status, obj)
	}
}

// TestErrEnvelopeSuccessCodes 成功码有多种形态，任何一种都不能被判成业务错误。
//
// 真实上游（2026-10-21 抓包）：points/records 与 points/first-login 成功时
// 返回 code="000000"、desc="成功"。早期只认 "0"/"200"，把全部成功的积分响应
// 改判成 502，额度刷新与积分查询整条链路永久失败。
func TestErrEnvelopeSuccessCodes(t *testing.T) {
	okCases := []string{
		`{"code":"000000","desc":"成功","data":{"balance":5000}}`,
		`{"code":"0","desc":"ok","data":{}}`,
		`{"code":0,"desc":"ok","data":{}}`,
		`{"code":"200","data":{}}`,
		`{"code":200,"data":{}}`,
		`{"code":"success","data":{}}`,
		`{"code":"OK","data":{}}`,
		`{"choices":[],"code":"100002"}`, // 有 choices 就是模型响应，不看 code
		`{"object":"list","code":"100002"}`,
		`{"data":{"balance":1}}`, // 无 code 字段
	}
	for _, raw := range okCases {
		if errEnvelope([]byte(raw)) {
			t.Errorf("误判为错误信封: %s", raw)
		}
	}

	errCases := []string{
		`{"code":"100002","desc":"缺少 token","data":{}}`,
		`{"code":"100001","desc":"登录已失效"}`,
		`{"code":500}`,
		`{"code":"999999","desc":"系统繁忙"}`,
	}
	for _, raw := range errCases {
		if !errEnvelope([]byte(raw)) {
			t.Errorf("应判为错误信封: %s", raw)
		}
	}
}

// TestDoRewritesBusinessError 上游把错误塞在 200 的 body 里时，状态要改判成 5xx，
// 否则错误会被当成成功放行、坏账号永远不进冷却。
func TestDoRewritesBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":"100002","desc":"登录已失效，请重新登录","data":{}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	resp, err := c.Do(context.Background(), http.MethodPost, "chat/completions", "s", map[string]any{}, nil)
	if err != nil {
		t.Fatalf("Do 失败: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("业务错误应改判 502，实际 %d", resp.Status)
	}
	if body := resp.BodyText(); !strings.Contains(body, "100002") {
		t.Errorf("body 应保留原文，实际 %q", body)
	}
}

// TestDoKeepsNormalJSON 正常 JSON 与 SSE 不受改判逻辑影响。
func TestDoKeepsNormalJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	resp, err := c.Do(context.Background(), http.MethodGet, "models", "s", nil, nil)
	if err != nil {
		t.Fatalf("Do 失败: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("正常响应不该改判，实际 %d", resp.Status)
	}
	if body := resp.BodyText(); !strings.Contains(body, `"object":"list"`) {
		t.Errorf("body 被改动: %q", body)
	}
}

// TestModelsParsesPromoFromName 真实 /models 响应（2026-10-21 抓包）没有 multiplier
// 字段：倍率与「限时免费」都写在展示名里。这个测试锁死解析口径，防止面板倍率列
// 退回「—」、限免模型又看不出来。
func TestModelsParsesPromoFromName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"reasoning_enabled":true,"object":"list","data":[
			{"id":"DeepSeek-V4.1-Flash","name":"DeepSeek V4.1 Flash(x2.0)","type":"chat","protocol":"openai_chat",
			 "context_length":1048576,"max_output_tokens":384000,"is_default_model":true,
			 "reasoning_efforts":["none","low","high"],"default_reasoning_effort":"low",
			 "capabilities":{"reasoning":true,"vision":true,"function_calling":true}},
			{"id":"MiniMax-M3","name":"MiniMax M3 （x4.0）","type":"chat","protocol":"openai_chat",
			 "capabilities":{"vision":true}},
			{"id":"spark-x2.6-preview","name":"Spark-X2.6-preview(限时免费)(x0.0)","type":"chat","protocol":"openai_chat"},
			{"id":"Hy-Image-3.5-preview","name":"Hy image 3.5 preview","type":"image","protocol":"openai_chat"}
		]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	list, err := c.Models(context.Background(), "sess")
	if err != nil {
		t.Fatalf("Models 失败: %v", err)
	}
	if len(list) != 4 {
		t.Fatalf("模型数 %d 期望 4", len(list))
	}
	byID := map[string]config.Model{}
	for _, m := range list {
		byID[m.ID] = m
	}

	ds := byID["DeepSeek-V4.1-Flash"]
	if ds.Multiplier != 2.0 {
		t.Errorf("DeepSeek 倍率 %v 期望 2.0（从名称 (x2.0) 解析）", ds.Multiplier)
	}
	if !ds.IsDefault || ds.IsFree {
		t.Errorf("DeepSeek 默认/限免标记错: default=%v free=%v", ds.IsDefault, ds.IsFree)
	}
	if ds.DefaultReasoning != "low" {
		t.Errorf("默认思考档 %q", ds.DefaultReasoning)
	}

	mm := byID["MiniMax-M3"]
	if mm.Multiplier != 4.0 {
		t.Errorf("MiniMax 全角括号倍率 %v 期望 4.0", mm.Multiplier)
	}

	sp := byID["spark-x2.6-preview"]
	if !sp.IsFree || sp.Multiplier != 0 {
		t.Errorf("限免模型应 is_free=true multiplier=0，实际 free=%v mult=%v", sp.IsFree, sp.Multiplier)
	}

	img := byID["Hy-Image-3.5-preview"]
	if img.IsFree {
		t.Error("没有倍率标注的图像模型不该被判成限免")
	}
	if img.Type != "image" {
		t.Errorf("type=%q 期望 image", img.Type)
	}
}

// TestClaimOnboardingClaimsOnlyIncomplete 新手任务：只对未完成的 key 发 complete，
// 积分按**本地表**现算（服务端 earned 是累计值、不可当增量），幂等判据是
// alreadyCompleted（既不是 HTTP 码也不是信封 code）。
func TestClaimOnboardingClaimsOnlyIncomplete(t *testing.T) {
	var completed []string
	var gotTasks, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/onboarding/tasks"):
			_, _ = io.WriteString(w, `{"code":"000000","desc":"成功","data":{"tasks":{
				"first_message":true,"pick_skill":false,"generate_ppt":false,"set_schedule":true,
				"install_skill":true,"configure_remote":true,"create_soul":true,"share_soul":true},
				"earned":8500,"total":10000}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/onboarding/tasks/complete"):
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			var body map[string]any
			_ = json.Unmarshal(b, &body)
			completed = append(completed, body["key"].(string))
			// alreadyCompleted=false 才算本次领到
			_, _ = io.WriteString(w, `{"code":"000000","data":{"alreadyCompleted":false,"balance":10000}}`)
		default:
			gotTasks = r.URL.Path
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	claimed, points, err := c.ClaimOnboarding(context.Background(), "sess")
	if err != nil {
		t.Fatalf("ClaimOnboarding: %v", err)
	}
	// 只有 pick_skill(1000) 与 generate_ppt(1500) 未完成
	if len(claimed) != 2 || claimed[0] != "generate_ppt" || claimed[1] != "pick_skill" {
		t.Fatalf("claimed = %v，期望 generate_ppt + pick_skill（已排序）", claimed)
	}
	if points != 2500 {
		t.Errorf("积分 = %d，期望 2500（按本地表算，不采信服务端 earned）", points)
	}
	if len(completed) != 2 {
		t.Errorf("应只对 2 个未完成任务发 complete，实际 %v", completed)
	}
	// 上报 body 只有 key（官方客户端也不带设备指纹 / 版本号）
	if strings.Count(gotBody, ":") != 1 || !strings.Contains(gotBody, `"key"`) {
		t.Errorf("complete 的 body 应只有 key 字段，实际 %s", gotBody)
	}
	_ = gotTasks
}

// TestOnboardingTasksNormalizes 目录归一：服务端多给的 key 忽略、缺的补 false，
// earned 一律按本地表现算（不采信响应体）。
func TestOnboardingTasksNormalizes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":"000000","data":{"tasks":{
			"first_message":true,"unknown_task":true},"earned":99999,"total":10000}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "1.0.0", "", 5*time.Second)
	tasks, earned, err := c.OnboardingTasks(context.Background(), "sess")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != len(OnboardingPoints) {
		t.Errorf("任务数 = %d，期望 %d（按本地表归一）", len(tasks), len(OnboardingPoints))
	}
	if _, ok := tasks["unknown_task"]; ok {
		t.Error("服务端多给的 key 应被忽略")
	}
	if !tasks["first_message"] || tasks["share_soul"] {
		t.Errorf("完成状态解析错: %v", tasks)
	}
	if earned != OnboardingPoints["first_message"] {
		t.Errorf("earned = %d，期望 %d（按本地表算）", earned, OnboardingPoints["first_message"])
	}
}

// TestOnboardingPointsSum 本地积分表必须正好 10000（错一项就等于少领钱）。
func TestOnboardingPointsSum(t *testing.T) {
	var sum int64
	for _, v := range OnboardingPoints {
		sum += v
	}
	if sum != OnboardingTotal {
		t.Fatalf("表内合计 = %d，期望 %d", sum, OnboardingTotal)
	}
}
