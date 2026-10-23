package server

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rsfree/loomy2api/internal/config"
	"github.com/rsfree/loomy2api/internal/egress"
	"github.com/rsfree/loomy2api/internal/pool"
	"github.com/rsfree/loomy2api/internal/usage"
)

// captureLog 把标准 logger 的输出临时收进 buffer（日志行断言用）。
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// newOriginHandler 造一个只够跑日志/台账的最小 Handler。
func newOriginHandler(t *testing.T) *Handler {
	t.Helper()
	cfg := config.Defaults()
	cfg.LogRequests = true
	cfg.SendClientInfo = true
	return New(cfg, pool.New(""), nil, usage.New("", 100))
}

// newEgress 启用出口隔离，把一个账号固定绑到 127.0.0.1。
func newEgress(t *testing.T, names ...string) *egress.Resolver {
	t.Helper()
	r := egress.New(egress.Config{
		Mode:  egress.ModeLocal,
		Local: egress.Local{IPs: []string{"127.0.0.1"}},
	})
	if !r.Enabled() {
		t.Fatalf("出口隔离未启用：%v", r.Error())
	}
	r.Rebuild(names)
	return r
}

// TestLogCallCarriesOrigins 请求行必须同时带「出口」与「来源」两个方向，
// 且顺序固定为 egress → src → ua（对齐 workbuddy2api）。
// 历史格式是 `ip=… egress=…`：两个 IP 并排且都叫 ip，读起来分不清方向。
func TestLogCallCarriesOrigins(t *testing.T) {
	h := newOriginHandler(t)
	h.Egress = newEgress(t, "acc1")
	buf := captureLog(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36")

	h.logCall(&pool.Account{Name: "acc1"}, "DeepSeek-V4.1-Flash", 200, 1200*time.Millisecond,
		"points=110", req)

	line := buf.String()
	for _, want := range []string{
		"points=110 | egress=127.0.0.1 src=203.0.113.7 ua=\"Chrome/120.0.0.0\"",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("日志行缺 %q：%s", want, line)
		}
	}
	if strings.Contains(line, " ip=") {
		t.Errorf("旧的 ip= 字段应已改名 src=：%s", line)
	}
}

// TestLogCallOriginsRespectSwitches 未启用出口隔离时不追加 egress（旧格式不变）；
// log_client_info 关闭时不采集来源 IP / UA。
func TestLogCallOriginsRespectSwitches(t *testing.T) {
	h := newOriginHandler(t)
	h.Egress = egress.New(egress.Config{Mode: egress.ModeDirect})
	buf := captureLog(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("User-Agent", "curl/8.4.0")
	h.logCall(&pool.Account{Name: "acc1"}, "seedream", 200, time.Second, "points=110", req)

	line := buf.String()
	if strings.Contains(line, "egress=") {
		t.Errorf("direct 模式不该出现 egress：%s", line)
	}
	if !strings.Contains(line, "src=203.0.113.7") {
		t.Errorf("来源 IP 应保留：%s", line)
	}

	buf.Reset()
	h.Cfg.SendClientInfo = false
	h.logCall(&pool.Account{Name: "acc1"}, "seedream", 200, time.Second, "points=110", req)
	if got := buf.String(); strings.Contains(got, "src=") || strings.Contains(got, "ua=") {
		t.Errorf("log_client_info 关闭后不该记录来源：%s", got)
	}
}

// TestEgressLabelSemantics 三种取值口径：未启用空、有分配给 IP、无账号给 direct。
func TestEgressLabelSemantics(t *testing.T) {
	h := newOriginHandler(t)
	if got := h.egressLabel(&pool.Account{Name: "acc1"}); got != "" {
		t.Errorf("未启用出口隔离应为空，得到 %q", got)
	}
	h.Egress = newEgress(t, "acc1")
	if got := h.egressLabel(&pool.Account{Name: "acc1"}); got != "127.0.0.1" {
		t.Errorf("有分配应为 IP，得到 %q", got)
	}
	if got := h.egressLabel(nil); got != "direct" {
		t.Errorf("没绑定账号应为 direct，得到 %q", got)
	}
	// 不在分配表里的账号仍走 hash 槽位（Resolver.Slot 的兜底），标签照实报 IP——
	// 这时请求确实从那个地址出去，报 direct 反而是错的。
	if got := h.egressLabel(&pool.Account{Name: "unknown"}); got != "127.0.0.1" {
		t.Errorf("未登记账号应按 hash 槽位给 IP，得到 %q", got)
	}
}

// TestRecordCarriesOrigins 台账（面板「请求记录」的数据源）必须带出口，
// 与 ClientIP 是相反方向；同时补上原先两处漏填的来源字段。
func TestRecordCarriesOrigins(t *testing.T) {
	h := newOriginHandler(t)
	h.Egress = newEgress(t, "acc1")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("User-Agent", "OpenAI/Python 1.30.0")

	h.record(usage.Record{Account: "acc1", Model: "m", Kind: "chat", Status: 200}, req, &pool.Account{Name: "acc1"})

	recs := h.Ledger.Recent(1, usage.RecordFilter{})
	if len(recs) != 1 {
		t.Fatalf("台账条数 %d", len(recs))
	}
	r := recs[0]
	if r.Egress != "127.0.0.1" {
		t.Errorf("Egress = %q，期望 127.0.0.1", r.Egress)
	}
	if r.ClientIP != "203.0.113.7" {
		t.Errorf("ClientIP = %q", r.ClientIP)
	}
	if !strings.HasPrefix(r.UserAgent, "OpenAI/Python") {
		t.Errorf("UserAgent = %q", r.UserAgent)
	}

	// 接口侧的两个筛选参数（?egress= / ?client_ip=）走同一套 Match。
	if got := h.Ledger.Recent(10, usage.RecordFilter{Egress: "127.0.0"}); len(got) != 1 {
		t.Errorf("按出口子串筛选命中 %d 条，期望 1", len(got))
	}
	if got := h.Ledger.Recent(10, usage.RecordFilter{Egress: "10.0.0."}); len(got) != 0 {
		t.Errorf("不匹配的出口不该命中，得到 %d 条", len(got))
	}
	if got := h.Ledger.Recent(10, usage.RecordFilter{ClientIP: "203.0.113"}); len(got) != 1 {
		t.Errorf("按来源子串筛选命中 %d 条", len(got))
	}
	if got := h.Ledger.Recent(10, usage.RecordFilter{Keyword: "127.0.0.1"}); len(got) != 1 {
		t.Errorf("关键字搜索应能命中出口 IP，得到 %d 条", len(got))
	}
}

// TestShortUA 日志里只留「是什么客户端」，浏览器 UA 不能被整串铺进来。
func TestShortUA(t *testing.T) {
	cases := map[string]string{
		"curl/8.4.0": "curl/8.4.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36": "Chrome/120.0.0.0",
		"OpenAI/Python 1.30.0": "OpenAI/Python",
		"node":                 "node",
		"":                     "",
	}
	for in, want := range cases {
		if got := shortUA(in); got != want {
			t.Errorf("shortUA(%q) = %q，期望 %q", in, got, want)
		}
	}
}
