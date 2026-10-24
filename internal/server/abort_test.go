package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rsfree/loomy2api/internal/pool"
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

// fakeTimeoutError 模拟 transport 层的超时（TLSHandshakeTimeout / dial i/o timeout）：
// 它实现 net.Error 且 Timeout() = true，但不是我们自己的 ctx 到期。
type fakeTimeoutError struct{}

func (fakeTimeoutError) Error() string   { return "net/http: TLS handshake timeout" }
func (fakeTimeoutError) Timeout() bool   { return true }
func (fakeTimeoutError) Temporary() bool { return true }

// TestAbortStatusMapping 只有「客户端断开」映射成 499，其余一律 502。
func TestAbortStatusMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{errClientGone, statusClientClosed},
		{fakeTimeoutError{}, http.StatusBadGateway},
		{context.DeadlineExceeded, http.StatusBadGateway},
		{io.EOF, http.StatusBadGateway},
	}
	for _, c := range cases {
		if got := abortStatus(c.err); got != c.want {
			t.Errorf("abortStatus(%v) = %d，期望 %d", c.err, got, c.want)
		}
	}
}

// TestAttemptCapExpired 只有我们自己的 ctx 到期才算 cap 超时。
// 0.0.34 的回归就是把 net.Error.Timeout() 也算了进来，导致 TLS 握手超时
// （15s）在第一次尝试就被判死成 504，换号重试被跳过。
func TestAttemptCapExpired(t *testing.T) {
	if !attemptCapExpired(context.DeadlineExceeded) {
		t.Error("自己的 ctx 到期应判 cap 超时")
	}
	if attemptCapExpired(context.Canceled) {
		t.Error("客户端取消不该判成 cap 超时（否则会把断开记成 504）")
	}
	if attemptCapExpired(nil) {
		t.Error("没有 ctx 错误时不该判成 cap 超时")
	}
	if attemptCapExpired(fakeTimeoutError{}) {
		t.Error("传输层超时不是 cap 超时：它必须继续走换号重试")
	}
}

// TestConnFaultStillRetries 连接层故障（EOF / reset）必须换号重试并最终 502、
// 且计入账号 failures；不能像 0.0.34 那样第一次尝试就判死成 504。
//
// 注意：upstream 客户端的 Client.Timeout 恒为 0（长思考不许被掐断），所以这里
// 用「上游直接掐断连接」制造 url.Error，而不是靠客户端超时来模拟。
func TestConnFaultStillRetries(t *testing.T) {
	fs := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"doubao-seedream-5-lite","type":"image"}]}`)
			return
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close() // 连接直接断：客户端拿到 EOF
				return
			}
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	h, p, _ := newTestHandler(t, fs.srv.URL, nil)
	prewarm(t, h)
	// 换号重试要有号可换：再塞一个，否则第一次失败就会因「没有其他账号」退出。
	bal := int64(1000)
	p.Add(&pool.Account{
		Name: "acc2", Enabled: true, Session: "sess-2",
		ExpireAt: time.Now().Add(24 * time.Hour).Unix(), Available: &bal, LoginID: "13800000001",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"a cat"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态码 %d，期望 502（不是 504）：%s", w.Code, w.Body.String())
	}
	if got := imageHits(fs); got != 2 { // MaxRetries=1 ⇒ 两次尝试
		t.Errorf("连接故障应换号重试，上游被打了 %d 次", got)
	}
	if acc := p.All()[0]; acc.Failures == 0 {
		t.Error("连接故障应计入账号 failures（否则坏出口永远轮不到冷却）")
	}
}
