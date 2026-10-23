package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSlimImageBody 扩展字段被剔除，n>1 也去掉；核心字段保留。
func TestSlimImageBody(t *testing.T) {
	payload := map[string]any{
		"model": "doubao-seedream-5-lite", "prompt": "猫", "size": "2K",
		"stream": true, "sequential_image_generation": "auto",
		"sequential_image_generation_options": map[string]any{"max_images": 4},
		"watermark":                           true, "seed": 42, "n": 4,
	}
	out, dropped, ok := slimImageBody(payload)
	if !ok {
		t.Fatal("应判定为需要精简")
	}
	for _, k := range []string{"stream", "sequential_image_generation", "watermark", "seed"} {
		if _, exists := out[k]; exists {
			t.Errorf("字段 %s 未被剔除", k)
		}
	}
	if _, exists := out["n"]; exists {
		t.Error("n>1 应被剔除")
	}
	if out["model"] != payload["model"] || out["prompt"] != "猫" || out["size"] != "2K" {
		t.Errorf("核心字段丢失：%v", out)
	}
	if len(dropped) != 6 {
		t.Errorf("dropped = %v，期望 6 项", dropped)
	}
}

// TestSlimImageBodyNoop 只有核心字段时不该触发重试。
func TestSlimImageBodyNoop(t *testing.T) {
	out, dropped, ok := slimImageBody(map[string]any{
		"model": "m", "prompt": "p", "size": "1024x1024", "n": 1,
	})
	if ok || out != nil || dropped != nil {
		t.Errorf("不该精简：%v %v %v", out, dropped, ok)
	}
}

// TestMinimalImageBody 去掉 n / response_format，只留四个核心字段。
func TestMinimalImageBody(t *testing.T) {
	out, ok := minimalImageBody(map[string]any{
		"model": "m", "prompt": "p", "size": "2K", "n": 1, "response_format": "b64_json",
	})
	if !ok {
		t.Fatal("应判定为可继续精简")
	}
	if len(out) != 3 {
		t.Errorf("out = %v", out)
	}
	if _, again := minimalImageBody(out); again {
		t.Error("已是最小集合时不该再精简")
	}
}

// TestImage400RetriesSlim 每次生图 400 都要自动精简重试，客户端不应看到 400。
func TestImage400RetriesSlim(t *testing.T) {
	var calls int
	fs := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"object":"list","data":[
				{"id":"doubao-seedream-5-lite","name":"5 lite","type":"image"}]}`)
			return
		}
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"请求有误，请重试；若多次出现请更新应用版本","code":400}}`)
			return
		}
		_, _ = io.WriteString(w, `{"created":1,"data":[{"url":"https://example.com/x.png"}],"points_consumed":110}`)
	})
	h, p, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	body := `{"model":"doubao-seedream-4-5-251128","prompt":"一只猫","size":"2K",
		"stream":true,"sequential_image_generation":"auto","watermark":true,"seed":7,"n":4}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("客户端不该看到 400：%d %s", w.Code, w.Body.String())
	}
	if calls != 2 {
		t.Errorf("上游调用 %d 次，期望首次 400 + 精简重试 1 次", calls)
	}
	sent := fs.bodies[len(fs.bodies)-1]
	for _, k := range []string{"stream", "sequential_image_generation", "watermark", "seed", "n"} {
		if strings.Contains(sent, `"`+k+`"`) {
			t.Errorf("重试请求仍带 %s：%s", k, sent)
		}
	}
	if !strings.Contains(sent, `"model":"doubao-seedream-5-lite"`) {
		t.Errorf("重试没带兜底模型：%s", sent)
	}
	if accs := p.All(); len(accs) != 1 || accs[0].PointsUsed != 110 {
		t.Errorf("重试成功的积分未入账：%+v", accs)
	}
}

// TestImage400NoExtraFields 请求本来就干净时不做无意义重试。
func TestImage400NoExtraFields(t *testing.T) {
	var calls int
	fs := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"object":"list","data":[
				{"id":"doubao-seedream-5-lite","name":"5 lite","type":"image"}]}`)
			return
		}
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"请求有误","code":400}}`)
	})
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"猫","size":"1024x1024"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 %d，期望 400", w.Code)
	}
	if calls != 1 {
		t.Errorf("上游调用 %d 次，期望只 1 次", calls)
	}
	if !strings.Contains(w.Body.String(), "请求有误") {
		t.Errorf("错误原文应透传：%s", w.Body.String())
	}
}

// TestSlimImageBodyKeepsImages 减肥白名单必须包含 images：
// 把它当扩展字段剔掉，等于把多张参考图整段扔掉（上游不报错，只会出一张文生图）。
func TestSlimImageBodyKeepsImages(t *testing.T) {
	out, _, ok := slimImageBody(map[string]any{
		"model": "m", "prompt": "p", "images": []any{"u1", "u2"},
		"size": "2K", "watermark": true,
	})
	if !ok {
		t.Fatal("应判定为需要精简")
	}
	if _, exists := out["images"]; !exists {
		t.Errorf("images 被剔除了：%v", out)
	}
}

// TestMinimalImageBodyKeepsImages 退到最小集合时同样要保留 images。
func TestMinimalImageBodyKeepsImages(t *testing.T) {
	out, ok := minimalImageBody(map[string]any{
		"model": "m", "prompt": "p", "images": []any{"u1", "u2"},
		"size": "2K", "response_format": "b64_json",
	})
	if !ok {
		t.Fatal("应判定为可继续精简")
	}
	if _, exists := out["images"]; !exists {
		t.Errorf("images 被剔除了：%v", out)
	}
}

// TestImage400RetryKeepsReferenceImages 全链路回归：400 触发减肥重试后，
// 重试请求里必须还带着 images（顺序不变），只去掉真正多余的字段。
func TestImage400RetryKeepsReferenceImages(t *testing.T) {
	calls := 0
	fs := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"object":"list","data":[
				{"id":"doubao-seedream-5-lite","name":"5 lite","type":"image"}]}`)
			return
		}
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"请求有误，请重试","code":400}}`)
			return
		}
		_, _ = io.WriteString(w, `{"created":1,"data":[{"url":"https://example.com/x.png"}],"points_consumed":110}`)
	})
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	body := `{"model":"doubao-seedream-5-lite","prompt":"合成","images":["u1","u2"],
		"size":"2K","watermark":true,"seed":7}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("客户端不该看到 400：%d %s", w.Code, w.Body.String())
	}
	if calls != 2 {
		t.Fatalf("上游调用 %d 次，期望 400 + 精简重试", calls)
	}
	retry := fs.bodies[len(fs.bodies)-1]
	if !strings.Contains(retry, `"images":["u1","u2"]`) {
		t.Errorf("重试把参考图丢了：%s", retry)
	}
	for _, k := range []string{"watermark", "seed"} {
		if strings.Contains(retry, `"`+k+`"`) {
			t.Errorf("重试仍带 %s：%s", k, retry)
		}
	}
}
