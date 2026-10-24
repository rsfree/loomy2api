package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// queuedUpstream 一个「排队中」的上游：不主动返回，只等测试放行或客户端断开。
func queuedUpstream(release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"doubao-seedream-5-lite","type":"image"}]}`)
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
}

func prewarm(t *testing.T, h *Handler) {
	t.Helper()
	if list := h.Models.Get(context.Background(), "sess"); len(list) != 1 {
		t.Fatalf("目录预热失败：%d 个模型", len(list))
	}
}

func imageHits(fs *fakeUpstream) int {
	n := 0
	for _, r := range fs.seen {
		if strings.HasSuffix(r.URL.Path, "/images/generations") {
			n++
		}
	}
	return n
}

// TestImageAttemptCapIs504NotPunished 单次尝试上限到期必须是 504 timeout_error，
// 且不能把账号记成故障。旧实现：ctx 到期 → 502 + ReportFailure(0) → FaultUnknown
// → 给账号上冷却，还要再换一个号重试一次（一次超时罚两个号）。
func TestImageAttemptCapIs504NotPunished(t *testing.T) {
	old := imageAttemptCap
	imageAttemptCap = 120 * time.Millisecond
	t.Cleanup(func() { imageAttemptCap = old })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	fs := newFakeUpstream(t, queuedUpstream(release))
	h, p, _ := newTestHandler(t, fs.srv.URL, nil)
	prewarm(t, h)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"a cat"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("状态码 %d，期望 504：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"type":"timeout_error"`) {
		t.Errorf("错误体不是 timeout_error：%s", w.Body.String())
	}
	if got := imageHits(fs); got != 1 {
		t.Errorf("超时后不该换号重试，上游被打了 %d 次", got)
	}
	if acc := p.All()[0]; acc.Failures != 0 || !acc.Healthy() {
		t.Errorf("超时不该罚账号：failures=%d healthy=%v", acc.Failures, acc.Healthy())
	}
}

// TestImageClientGoneIs499NotPunished 调用方在 900s 整断开（线上 605 条的真实形态）
// 要记 499、立刻停止换号、不罚账号。
func TestImageClientGoneIs499NotPunished(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	fs := newFakeUpstream(t, queuedUpstream(release))
	h, p, _ := newTestHandler(t, fs.srv.URL, nil)
	prewarm(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 客户端已经走了
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"a cat"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != statusClientClosed {
		t.Fatalf("状态码 %d，期望 %d：%s", w.Code, statusClientClosed, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "client_closed_request") {
		t.Errorf("错误体不是 client_closed_request：%s", w.Body.String())
	}
	if got := imageHits(fs); got > 1 {
		t.Errorf("客户端断开后不该继续换号试：上游被打了 %d 次", got)
	}
	if acc := p.All()[0]; acc.Failures != 0 || !acc.Healthy() {
		t.Errorf("客户端断开不该罚账号：failures=%d healthy=%v", acc.Failures, acc.Healthy())
	}
}

// TestChatClientGoneIs499NotPunished 对话端点同一个毛病：调用方断开被记成
// 连接异常 → 502 + FaultUnknown 冷却。改后是 499 且不罚号。
func TestChatClientGoneIs499NotPunished(t *testing.T) {
	fs := newFakeUpstream(t, jsonHandler(http.StatusOK, `{"choices":[{"message":{"content":"hi"}}]}`))
	h, p, _ := newTestHandler(t, fs.srv.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"spark-x","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != statusClientClosed {
		t.Fatalf("状态码 %d，期望 %d：%s", w.Code, statusClientClosed, w.Body.String())
	}
	if acc := p.All()[0]; acc.Failures != 0 || !acc.Healthy() {
		t.Errorf("客户端断开不该罚账号：failures=%d healthy=%v", acc.Failures, acc.Healthy())
	}
}

// TestAbortStatusMapping 三个哨兵错误映射成对外状态码。
func TestAbortStatusMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{errClientGone, statusClientClosed},
		{errConnectTimeout, http.StatusGatewayTimeout},
		{context.DeadlineExceeded, http.StatusBadGateway},
		{io.EOF, http.StatusBadGateway},
	}
	for _, c := range cases {
		if got := abortStatus(c.err); got != c.want {
			t.Errorf("abortStatus(%v) = %d，期望 %d", c.err, got, c.want)
		}
	}
}

// TestAttemptTimedOut 三种超时措辞都要认，客户端取消（Canceled）不能算超时。
func TestAttemptTimedOut(t *testing.T) {
	if !attemptTimedOut(nil, context.DeadlineExceeded) {
		t.Error("attempt ctx 到期应判超时")
	}
	if !attemptTimedOut(context.DeadlineExceeded, nil) {
		t.Error("err 包装 DeadlineExceeded 应判超时")
	}
	if attemptTimedOut(context.Canceled, context.Canceled) {
		t.Error("客户端取消不该判成超时（否则会把断开记成 504）")
	}
}
