// helpers.go HTTP 层小工具：JSON 响应、错误体、字符串转换与 usage 解析。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/rsfree/loomy2api/internal/account"
)

// writeJSON 写出 JSON（不设状态码，调用方负责）。
func writeJSON(w http.ResponseWriter, v any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeErr 写出 OpenAI 形态的错误体。
func writeErr(w http.ResponseWriter, status int, msg, kind string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	writeJSON(w, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    kind,
			"code":    status,
		},
	})
}

// writeAnthropicErr 写出 Anthropic 形态的错误体。
func writeAnthropicErr(w http.ResponseWriter, status int, msg string) {
	kind := "api_error"
	switch status {
	case http.StatusBadRequest:
		kind = "invalid_request_error"
	case http.StatusUnauthorized:
		kind = "authentication_error"
	case http.StatusTooManyRequests:
		kind = "rate_limit_error"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	writeJSON(w, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": kind, "message": msg},
	})
}

// readJSON 读取请求体并解析成 map。
func readJSON(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("不是合法 JSON: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// usageParts 从 usage 对象里取出 (积分, prompt, completion, reasoning)。
func usageParts(usage map[string]any) (points, prompt, completion, reasoning int64) {
	if usage == nil {
		return
	}
	points = int64(numOf(usage["points_consumed"]))
	prompt = int64(numOf(usage["prompt_tokens"]))
	completion = int64(numOf(usage["completion_tokens"]))
	if d, ok := usage["completion_tokens_details"].(map[string]any); ok {
		reasoning = int64(numOf(d["reasoning_tokens"]))
	}
	return
}

func usageSummary(prompt, completion, points, reasoning int64) string {
	parts := []string{}
	if prompt > 0 || completion > 0 {
		parts = append(parts, fmt.Sprintf("tok=%d/%d", prompt, completion))
	}
	if reasoning > 0 {
		parts = append(parts, fmt.Sprintf("think=%d", reasoning))
	}
	if points > 0 {
		parts = append(parts, fmt.Sprintf("points=%d", points))
	}
	return strings.Join(parts, " ")
}

// clientIP 取调用来源 IP（代理头优先）。
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.Index(v, ","); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func ua(r *http.Request) string { return truncate(r.Header.Get("User-Agent"), 200) }

func rid(h http.Header) string {
	for _, k := range []string{"X-Request-Id", "X-Request-ID", "Traceparent"} {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	}
	return false
}

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
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

func numOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
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

func intOf(v any) (int, bool) {
	if v == nil {
		return 0, false
	}
	return int(numOf(v)), true
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func randomHex(n int) string { return account.RandomHex(n) }

func toJSON(v any) string {
	buf, _ := json.Marshal(v)
	return string(buf)
}

// ---- 字节级小工具（避免为 SSE 逐行扫描引入 bufio.Scanner 的 64KB 限制）

func indexByte(b []byte, c byte) int { return bytes.IndexByte(b, c) }

func hasPrefix(b []byte, s string) bool {
	if len(b) < len(s) {
		return false
	}
	return string(b[:len(s)]) == s
}

func trimSpace(b []byte) []byte { return bytes.TrimSpace(b) }

func trimCR(b []byte) []byte { return bytes.TrimRight(b, "\r") }
