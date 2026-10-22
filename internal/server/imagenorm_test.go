package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// upstreamImage 取假上游最后一次收到的生图 body 里的 image 字段。
func upstreamImage(t *testing.T, fs *fakeUpstream) (any, bool) {
	t.Helper()
	last := fs.bodies[len(fs.bodies)-1]
	var payload map[string]any
	if err := json.Unmarshal([]byte(last), &payload); err != nil {
		t.Fatalf("上游 body 不是 JSON：%s", last)
	}
	v, ok := payload["image"]
	return v, ok
}

// TestImageSingleElementArrayBecomesString 单元素数组必须压成字符串再转发。
// 上游对 `"image":["url"]` 不报错但会丢掉参考图（生图退化成纯文生图），
// 所以直通 generations 也得做和 edits 前门一样的归一。
func TestImageSingleElementArrayBecomesString(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"高清化","image":["https://example.com/a.jpg"],"size":"1024x1024"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	v, ok := upstreamImage(t, fs)
	if !ok {
		t.Fatalf("上游没收到 image 字段：%s", fs.bodies[len(fs.bodies)-1])
	}
	s, isStr := v.(string)
	if !isStr || s != "https://example.com/a.jpg" {
		t.Errorf("单元素数组没被压成字符串：%#v", v)
	}
}

// TestImageMultiElementArrayKept 多图输入仍要发数组（顺序是语义的一部分）。
func TestImageMultiElementArrayKept(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"合成","image":["https://example.com/a.jpg","https://example.com/b.jpg"]}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	v, _ := upstreamImage(t, fs)
	arr, ok := v.([]any)
	if !ok || len(arr) != 2 || arr[0] != "https://example.com/a.jpg" || arr[1] != "https://example.com/b.jpg" {
		t.Errorf("多图数组被改坏了：%#v", v)
	}
}

// TestImageEmptyArrayDropped 空数组/空字符串当作「没传图」，别让上游拿空值当参考图。
func TestImageEmptyArrayDropped(t *testing.T) {
	for _, body := range []string{
		`{"model":"doubao-seedream-5-lite","prompt":"画猫","image":[]}`,
		`{"model":"doubao-seedream-5-lite","prompt":"画猫","image":"  "}`,
	} {
		fs := imageFallbackUpstream(t)
		h, _, _ := newTestHandler(t, fs.srv.URL, nil)
		_ = h.Models.Get(context.Background(), "sess")

		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
		}
		if v, ok := upstreamImage(t, fs); ok {
			t.Errorf("空参考图仍被转发：%#v（请求 %s）", v, body)
		}
	}
}

// TestImageFallbackVisibleInHeader 模型被回退必须让客户端看得见：
// 静默替换模型是「垫图不生效」最隐蔽的一种（换成了不支持垫图的模型）。
func TestImageFallbackVisibleInHeader(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-0-260128","prompt":"高清化","image":["https://example.com/a.jpg"]}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Loomy-Model"); got != "doubao-seedream-5-lite" {
		t.Errorf("X-Loomy-Model 应是回退后的模型，得到 %q", got)
	}
	if got := w.Header().Get("X-Loomy-Model-Fallback"); !strings.Contains(got, "doubao-seedream-5-0-260128") {
		t.Errorf("X-Loomy-Model-Fallback 应回显客户端原本写的模型，得到 %q", got)
	}
}

// TestImageNoFallbackNoHeader 目录里有的模型不该出现回退响应头。
func TestImageNoFallbackNoHeader(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"画猫"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if got := w.Header().Get("X-Loomy-Model-Fallback"); got != "" {
		t.Errorf("精确命中时不该有回退头，得到 %q", got)
	}
	if got := w.Header().Get("X-Loomy-Model"); got != "doubao-seedream-5-lite" {
		t.Errorf("X-Loomy-Model 应为 doubao-seedream-5-lite，得到 %q", got)
	}
}
