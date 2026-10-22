package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// imageFallbackUpstream 假上游：目录里只有 5-lite 这一个图像模型，gen 直接回图。
func imageFallbackUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	return newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"object":"list","data":[
				{"id":"doubao-seedream-5-lite","name":"Seedream 5 Lite","type":"image"},
				{"id":"Hy-Image-3.5-preview","name":"Hy image","type":"image"},
				{"id":"qwen-image-3.0-pro","name":"Qwen Image 3.0 Pro","type":"image"},
				{"id":"spark-x2.6","name":"Spark X2.6 (x0.0)","type":"chat"}
			]}`)
			return
		}
		_, _ = io.WriteString(w, `{"created":1,"data":[{"url":"https://example.com/x.png"}],"points_consumed":110}`)
	})
}

// TestImageUnknownModelFallsBack 生图端点收到目录外的模型名（如 Ark 的
// doubao-seedream-4-0-250828）时，要改发兜底模型，而不是 404。
func TestImageUnknownModelFallsBack(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, p, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-4-0-250828","prompt":"一只猫","size":"1024x1024"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	last := fs.bodies[len(fs.bodies)-1]
	if !strings.Contains(last, `"model":"doubao-seedream-5-lite"`) {
		t.Errorf("没改成兜底模型：%s", last)
	}
	if strings.Contains(last, "doubao-seedream-4-0-250828") {
		t.Errorf("原始未知模型名仍被转发：%s", last)
	}
	if accs := p.All(); len(accs) != 1 || accs[0].PointsUsed != 110 {
		t.Errorf("兜底生图的积分未入账：%+v", accs)
	}
}

// TestImageKnownModelNotReplaced 目录里有的图像模型必须原样使用。
func TestImageKnownModelNotReplaced(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"Hy-Image-3.5-preview","prompt":"一只猫"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	if last := fs.bodies[len(fs.bodies)-1]; !strings.Contains(last, `"model":"Hy-Image-3.5-preview"`) {
		t.Errorf("已知模型被改写：%s", last)
	}
}

// TestChatUnknownModelStill404 兜底只对生图端点生效：对话端点拼错模型仍然 404，
// 否则任何拼错的对话模型都会变成一次生图。
func TestChatUnknownModelStill404(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")
	before := len(fs.seen)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"doubao-seedream-4-0-250828","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 %d，期望 404", w.Code)
	}
	if len(fs.seen) != before {
		t.Errorf("404 的请求不该打上游")
	}
}

// TestImageFallbackDisabled 配置留空 = 关闭「通用」兜底，非 Seedream 的未知模型仍 404。
func TestImageFallbackDisabled(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")
	h.Cfg.ImageFallbackModel = ""
	before := len(fs.seen)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"flux-pro-1-1","prompt":"一只猫"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 %d，期望 404", w.Code)
	}
	if len(fs.seen) != before {
		t.Errorf("关闭兜底后不该打上游")
	}
}

// TestImageFallbackMissingFromCatalog 兜底模型自己不在目录里时不发上游：
// 发一个上游认不出的名字，可能被静默回落成默认对话模型，比 404 更糟。
func TestImageFallbackMissingFromCatalog(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")
	h.Cfg.ImageFallbackModel = "doubao-seedream-9-9"
	before := len(fs.seen)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"whatever-model","prompt":"一只猫"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 %d，期望 404", w.Code)
	}
	if len(fs.seen) != before {
		t.Errorf("兜底模型不存在时不该打上游")
	}
}

// TestImageEmptyModelUsesFallback 生图不写 model 时用图像兜底，而不是默认对话模型。
func TestImageEmptyModelUsesFallback(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"一只猫"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	if last := fs.bodies[len(fs.bodies)-1]; !strings.Contains(last, `"model":"doubao-seedream-5-lite"`) {
		t.Errorf("空 model 没用图像兜底：%s", last)
	}
}

// TestSeedreamFamilyAlwaysMaps 只要名字属于 doubao-seedream-*，即使目录里没有、
// 且通用兜底被关掉或指向别的模型，也要落到 doubao-seedream-5-lite。
func TestSeedreamFamilyAlwaysMaps(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")
	// 通用兜底指向另一个图像模型，验证家族规则优先。
	h.Cfg.ImageFallbackModel = "Hy-Image-3.5-preview"

	cases := []string{
		"doubao-seedream-4-0-250828",
		"doubao-seedream-3-0-t2i-250415",
		"DOUBAO-SEEDREAM-5-0-pro",
	}
	for _, name := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
			strings.NewReader(`{"model":"`+name+`","prompt":"一只猫"}`))
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s 状态码 %d：%s", name, w.Code, w.Body.String())
		}
		last := fs.bodies[len(fs.bodies)-1]
		if !strings.Contains(last, `"model":"doubao-seedream-5-lite"`) {
			t.Errorf("%s 没落 5-lite：%s", name, last)
		}
	}

	// 非 Seedream 的未知模型仍走配置的通用兜底。
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"some-unknown-image-model","prompt":"一只猫"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("通用兜底状态码 %d：%s", w.Code, w.Body.String())
	}
	if last := fs.bodies[len(fs.bodies)-1]; !strings.Contains(last, `"model":"Hy-Image-3.5-preview"`) {
		t.Errorf("通用兜底没生效：%s", last)
	}
}

// TestSeedreamFamilyWhenFallbackEmpty 通用兜底留空也不影响家族归并。
func TestSeedreamFamilyWhenFallbackEmpty(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")
	h.Cfg.ImageFallbackModel = ""

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-4-0-250828","prompt":"一只猫"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	if last := fs.bodies[len(fs.bodies)-1]; !strings.Contains(last, `"model":"doubao-seedream-5-lite"`) {
		t.Errorf("家族归并没生效：%s", last)
	}
}

// TestFamilyFallbackQwen qwen-image-2.5（目录里没有）要按家族落到 qwen-image-3.0-pro，
// 而不是落到配置的通用兜底 doubao-seedream-5-lite。
func TestFamilyFallbackQwen(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"qwen-image-2.5-preview","prompt":"一只猫"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	if last := fs.bodies[len(fs.bodies)-1]; !strings.Contains(last, `"model":"qwen-image-3.0-pro"`) {
		t.Errorf("没落到同族模型：%s", last)
	}
}

// TestFamilyFallbackPicksNewest 同族有多个时取版本最高的。
func TestFamilyFallbackPicksNewest(t *testing.T) {
	fs := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"object":"list","data":[
				{"id":"doubao-seedream-5-lite","name":"5 lite","type":"image"},
				{"id":"doubao-seedream-6-pro","name":"6 pro","type":"image"}
			]}`)
			return
		}
		_, _ = io.WriteString(w, `{"created":1,"data":[{"url":"https://example.com/x.png"}],"points_consumed":110}`)
	})
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-4-0-250828","prompt":"一只猫"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	if last := fs.bodies[len(fs.bodies)-1]; !strings.Contains(last, `"model":"doubao-seedream-6-pro"`) {
		t.Errorf("同族没取最高版本：%s", last)
	}
}

// TestFamilyFallbackIgnoresChatModels 家族回退只认 type=image：
// 同前缀的对话模型不能被当成图像兜底。
func TestFamilyFallbackIgnoresChatModels(t *testing.T) {
	fs := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"object":"list","data":[
				{"id":"doubao-seedream-5-lite-chat","name":"chat","type":"chat"}
			]}`)
			return
		}
		_, _ = io.WriteString(w, `{"created":1,"data":[{"url":"https://example.com/x.png"}],"points_consumed":110}`)
	})
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")
	before := len(fs.seen)

	// 没有可用的图像家族模型、通用兜底也不在目录里 → 404，不发上游。
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-4-0-250828","prompt":"一只猫"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 %d，期望 404", w.Code)
	}
	if len(fs.seen) != before {
		t.Errorf("不该打上游")
	}
}
