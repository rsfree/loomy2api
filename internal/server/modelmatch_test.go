package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rsfree/loomy2api/internal/config"
)

func catalogOf(ids ...string) []config.Model {
	out := make([]config.Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, config.Model{ID: id})
	}
	return out
}

// TestMatchCatalogExactFirst 精确命中优先于前缀：目录里有同名前缀更长的条目时
// 也必须落在完全同名的那个上。
func TestMatchCatalogExactFirst(t *testing.T) {
	list := catalogOf(
		"DeepSeek-V4.1-Flash",
		"DeepSeek-V4.1-Flash-Thinking",
		"spark-x",
		"spark-x2.6",
	)

	cases := []struct {
		in   string
		want string
	}{
		{"DeepSeek-V4.1-Flash", "DeepSeek-V4.1-Flash"},
		{"deepseek-v4.1-flash", "DeepSeek-V4.1-Flash"},
		{"DEEPSEEK-V4.1-FLASH", "DeepSeek-V4.1-Flash"},
		{"imodel/DeepSeek-V4.1-Flash", "DeepSeek-V4.1-Flash"},
		{"spark-x", "spark-x"},
		{"spark-x2.6", "spark-x2.6"},
	}
	for _, c := range cases {
		got, ok := matchCatalog(list, c.in)
		if !ok || got.ID != c.want {
			t.Errorf("matchCatalog(%q) = %q/%v，期望 %q", c.in, got.ID, ok, c.want)
		}
	}
}

// TestMatchCatalogLongestPrefix 带后缀的请求要落到**最长**前缀，而不是最短那个。
func TestMatchCatalogLongestPrefix(t *testing.T) {
	list := catalogOf(
		"DeepSeek-V4.1-Flash",
		"DeepSeek-V4.1-Flash-Thinking",
		"spark-x",
		"spark-x2.6",
	)
	cases := []struct {
		in   string
		want string
	}{
		// 用户给的例子：带版本后缀。
		{"deepseek-v4.1-flash-xxx", "DeepSeek-V4.1-Flash"},
		{"deepseek-v4.1-flash-2026-08-01", "DeepSeek-V4.1-Flash"},
		{"deepseek-v4.1-flash-thinking-xxx", "DeepSeek-V4.1-Flash-Thinking"},
		{"spark-x-latest", "spark-x"},
		{"spark-x2.6-v2", "spark-x2.6"},
		{"spark-x:free", "spark-x"},
		{"imodel/spark-x-2026", "spark-x"},
	}
	for _, c := range cases {
		got, ok := matchCatalog(list, c.in)
		if !ok || got.ID != c.want {
			t.Errorf("matchCatalog(%q) = %q/%v，期望 %q", c.in, got.ID, ok, c.want)
		}
	}
}

// TestMatchCatalogRejectsWordGlued 前缀后面是字母数字时不算命中：
// 拼错的名字要 404，不能被悄悄路由到别的模型上。
func TestMatchCatalogRejectsWordGlued(t *testing.T) {
	list := catalogOf("spark-x", "kimi-k2")
	for _, in := range []string{"spark-xylophone", "spark-x123", "kimi-k2abc"} {
		if got, ok := matchCatalog(list, in); ok {
			t.Errorf("matchCatalog(%q) 不该命中，实际 %q", in, got.ID)
		}
	}
}

// TestMatchCatalogProviderPrefix 目录与请求两侧的 imodel/ 前缀都要能对齐。
func TestMatchCatalogProviderPrefix(t *testing.T) {
	list := catalogOf("imodel/spark-x", "imodel/Kimi-k2.6")

	if got, ok := matchCatalog(list, "spark-x-2026"); !ok || got.ID != "imodel/spark-x" {
		t.Errorf("带前缀目录 + 裸请求：%q/%v", got.ID, ok)
	}
	if got, ok := matchCatalog(list, "imodel/kimi-k2.6-pro"); !ok || got.ID != "imodel/Kimi-k2.6" {
		t.Errorf("两侧都带前缀：%q/%v", got.ID, ok)
	}
	if got, ok := matchCatalog(list, "IMODEL/spark-x"); !ok || got.ID != "imodel/spark-x" {
		t.Errorf("前缀大小写：%q/%v", got.ID, ok)
	}
}

// TestMatchCatalogEmpty 空请求 / 空目录不该命中。
func TestMatchCatalogEmpty(t *testing.T) {
	if _, ok := matchCatalog(nil, "spark-x"); ok {
		t.Error("空目录不该命中")
	}
	if _, ok := matchCatalog(catalogOf("spark-x"), "   "); ok {
		t.Error("空请求不该命中")
	}
}

// TestSuggestModels 404 文案的候选：请求名是目录 id 的前缀时给出提示。
func TestSuggestModels(t *testing.T) {
	list := catalogOf("DeepSeek-V4.1-Flash", "DeepSeek-V4.1-Flash-Thinking", "spark-x")
	got := suggestModels(list, "deepseek-v4", 3)
	if len(got) != 2 || got[0] != "DeepSeek-V4.1-Flash" {
		t.Errorf("候选 = %v", got)
	}
	if s := suggestModels(list, "no-such", 3); len(s) != 0 {
		t.Errorf("不该有候选：%v", s)
	}
}

// TestChatResolvesPrefixedModel 端到端：带后缀的请求打到上游时必须是目录里的标准 id。
func TestChatResolvesPrefixedModel(t *testing.T) {
	fs := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"object":"list","data":[
				{"id":"DeepSeek-V4.1-Flash","name":"DeepSeek V4.1 Flash(x2.0)","type":"chat"},
				{"id":"spark-x","name":"Spark-X2.5 (x0.0)","type":"chat"}
			]}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	})
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	if list := h.Models.Get(context.Background(), "sess"); len(list) != 2 {
		t.Fatalf("目录预热失败：%d", len(list))
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash-2026-08-01","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	if len(fs.bodies) == 0 || !strings.Contains(fs.bodies[len(fs.bodies)-1], `"model":"DeepSeek-V4.1-Flash"`) {
		t.Fatalf("上游没收到标准 id：%v", fs.bodies)
	}
}

// TestImageModelSuffixRoutesToImage 图像模型带后缀也要被识别成图像模型，
// 走生图前门而不是对话链路。
func TestImageModelSuffixRoutesToImage(t *testing.T) {
	fs := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			_, _ = io.WriteString(w, `{"object":"list","data":[
				{"id":"Hy-Image-3.5-preview","name":"Hy image","type":"image"}
			]}`)
		case strings.Contains(r.URL.Path, "images/generations"):
			_, _ = io.WriteString(w, `{"created":1,"data":[{"url":"https://example.com/x.png"}],"points_consumed":110}`)
		default:
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"这不该发生"},"finish_reason":"stop"}],"usage":{}}`)
		}
	})
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"Hy-Image-3.5-preview-v2","messages":[{"role":"user","content":"画只猫"}]}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	var hit bool
	for _, r := range fs.seen {
		if strings.HasSuffix(r.URL.Path, "/images/generations") {
			hit = true
		}
		if strings.Contains(r.URL.Path, "chat/completions") {
			t.Fatalf("图像模型不该走对话链路：%s", r.URL.Path)
		}
	}
	if !hit {
		t.Fatalf("没走生图端点：%v", fs.seen)
	}
}

// TestUnknownModelSuggests 拼错时 404 文案要带最接近的模型，便于用户改正。
func TestUnknownModelSuggests(t *testing.T) {
	fs := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"object":"list","data":[
				{"id":"DeepSeek-V4.1-Flash","name":"DeepSeek V4.1 Flash(x2.0)","type":"chat"}
			]}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	})
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 %d，期望 404", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "DeepSeek-V4.1-Flash") || !strings.Contains(body, "最接近") {
		t.Errorf("404 文案没有候选提示：%s", body)
	}
}
