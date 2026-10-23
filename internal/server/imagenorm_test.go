package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// lastImagePayload 取假上游最后一次收到的生图 body。
func lastImagePayload(t *testing.T, fs *fakeUpstream) map[string]any {
	t.Helper()
	if len(fs.bodies) == 0 {
		t.Fatal("上游一次请求都没收到")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(fs.bodies[len(fs.bodies)-1]), &payload); err != nil {
		t.Fatalf("上游 body 不是 JSON：%s", fs.bodies[len(fs.bodies)-1])
	}
	return payload
}

// imagesOf 取 payload 里的 images 数组（缺失/空数组都返回 nil）。
func imagesOf(t *testing.T, payload map[string]any) []string {
	t.Helper()
	raw, exists := payload["images"]
	if !exists {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("images 不是数组：%#v", raw)
	}
	out := make([]string, 0, len(list))
	for i, item := range list {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("images[%d] 不是字符串：%#v", i, item)
		}
		out = append(out, s)
	}
	return out
}

// TestImageSingleStaysString 单图继续走 image 字符串——这条路径实测有效，
// 不要为了「统一」改成数组（客户端虽然发数组，但我们保留已验证的形态）。
func TestImageSingleStaysString(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"高清化","image":"https://example.com/a.jpg"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	payload := lastImagePayload(t, fs)
	if payload["image"] != "https://example.com/a.jpg" {
		t.Errorf("单图没按字符串转发：%#v", payload["image"])
	}
	if _, exists := payload["images"]; exists {
		t.Errorf("单图不该带 images 数组：%s", fs.bodies[len(fs.bodies)-1])
	}
	if got := w.Header().Get("X-Loomy-Image-Warn"); got != "" {
		t.Errorf("单图不该有警告头，得到 %q", got)
	}
}

// TestImageSingleElementArrayBecomesString 单元素数组压成字符串再转发：
// 上游对 `"image":["url"]` 不报错但会丢掉参考图（退化成纯文生图）。
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
	payload := lastImagePayload(t, fs)
	if payload["image"] != "https://example.com/a.jpg" {
		t.Errorf("单元素数组没被压成字符串：%#v", payload["image"])
	}
	if _, exists := payload["images"]; exists {
		t.Errorf("单图不该带 images 数组：%s", fs.bodies[len(fs.bodies)-1])
	}
}

// TestImageMultiForwardedAsImages 多图必须原样（有序）转发成 images 数组。
// 这是官方客户端的图生图形态；旧的「只留第一张」会把用户上传的第 2-4 张直接吞掉。
func TestImageMultiForwardedAsImages(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"把两张图合成一张","image":["https://example.com/a.jpg","https://example.com/b.jpg"]}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	payload := lastImagePayload(t, fs)
	want := []string{"https://example.com/a.jpg", "https://example.com/b.jpg"}
	if got := imagesOf(t, payload); !reflect.DeepEqual(got, want) {
		t.Errorf("images = %#v，期望 %#v", got, want)
	}
	if _, exists := payload["image"]; exists {
		t.Errorf("多图不该再带单数 image：%s", fs.bodies[len(fs.bodies)-1])
	}
	if got := w.Header().Get("X-Loomy-Image-Warn"); got != "" {
		t.Errorf("4 张以内不该告警，得到 %q", got)
	}
}

// TestImageOfficialImagesFieldPassthrough 官方客户端的原生形态（images 数组）
// 必须原样放行，顺序不能动（提示词里的「第一张/第二张」靠顺序）。
func TestImageOfficialImagesFieldPassthrough(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	body := `{"model":"doubao-seedream-5-lite","prompt":"把三张图拼成海报",
		"images":["data:image/png;base64,AAA","https://example.com/b.jpg","detail"],
		"n":1,"size":"2304x1728","response_format":"url"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	payload := lastImagePayload(t, fs)
	want := []string{"data:image/png;base64,AAA", "https://example.com/b.jpg", "detail"}
	if got := imagesOf(t, payload); !reflect.DeepEqual(got, want) {
		t.Errorf("images = %#v，期望 %#v", got, want)
	}
	for k, v := range map[string]any{"n": float64(1), "size": "2304x1728", "response_format": "url"} {
		if payload[k] != v {
			t.Errorf("字段 %s 被改动：%#v", k, payload[k])
		}
	}
}

// TestImageMixedFieldsMergedAndDeduped 两个字段同发时按 images → image 合并，
// 重复的只留一次（有些客户端会把同一张图在两个字段里各写一遍）。
func TestImageMixedFieldsMergedAndDeduped(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	body := `{"model":"doubao-seedream-5-lite","prompt":"合成",
		"images":["https://a/1.png","https://a/2.png"],"image":"https://a/2.png"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	want := []string{"https://a/1.png", "https://a/2.png"}
	if got := imagesOf(t, lastImagePayload(t, fs)); !reflect.DeepEqual(got, want) {
		t.Errorf("合并去重后 = %#v，期望 %#v", got, want)
	}
}

// TestImageTooManyTruncated 超过官方客户端上限（4 张）时截尾并挂警告头。
func TestImageTooManyTruncated(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	body := `{"model":"doubao-seedream-5-lite","prompt":"合成",
		"images":["u1","u2","u3","u4","u5"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	want := []string{"u1", "u2", "u3", "u4"}
	if got := imagesOf(t, lastImagePayload(t, fs)); !reflect.DeepEqual(got, want) {
		t.Errorf("截断后 = %#v，期望 %#v", got, want)
	}
	if got := w.Header().Get("X-Loomy-Image-Warn"); got != "images-truncated-to-4" {
		t.Errorf("警告头 = %q，期望 images-truncated-to-4", got)
	}
}

// TestNormalizeImageField 归一函数的表驱动用例：字符串/数组两种形态、空值、
// 类型错、mask 规则、超限截断。
func TestNormalizeImageField(t *testing.T) {
	cases := []struct {
		name       string
		in         map[string]any
		wantImage  any // nil = 不应存在
		wantImages any // nil = 不应存在
		wantWarn   string
		wantErr    string // 非空 = 期望 400，错误文案需包含该片段
	}{
		{"字符串原样", map[string]any{"image": "https://a/1.jpg"}, "https://a/1.jpg", nil, "", ""},
		{"单元素数组压字符串", map[string]any{"image": []any{"https://a/1.jpg"}}, "https://a/1.jpg", nil, "", ""},
		{"多图转 images 数组", map[string]any{"image": []any{"https://a/1.jpg", "https://a/2.jpg"}},
			nil, []any{"https://a/1.jpg", "https://a/2.jpg"}, "", ""},
		{"images 数组原样保留", map[string]any{"images": []any{"u1", "u2"}}, nil, []any{"u1", "u2"}, "", ""},
		{"images 字符串也收", map[string]any{"images": "u1"}, "u1", nil, "", ""},
		{"images 单元素压字符串", map[string]any{"images": []any{"u1"}}, "u1", nil, "", ""},
		{"两字段合并去重", map[string]any{"images": []any{"u1"}, "image": "u1"}, "u1", nil, "", ""},
		{"空数组删字段", map[string]any{"image": []any{}}, nil, nil, "", ""},
		{"空 images 删字段", map[string]any{"images": []any{}}, nil, nil, "", ""},
		{"null 视同没传", map[string]any{"image": nil}, nil, nil, "", ""},
		{"空白串拒绝", map[string]any{"image": "   "}, "   ", nil, "", "空字符串"},
		{"数组内空白串拒绝", map[string]any{"image": []any{"u1", " "}}, nil, nil, "", "空字符串"},
		{"类型不支持拒绝", map[string]any{"image": map[string]any{"url": "x"}},
			map[string]any{"url": "x"}, nil, "", "字符串或字符串数组"},
		{"五张截断", map[string]any{"images": []any{"1", "2", "3", "4", "5"}},
			nil, []any{"1", "2", "3", "4"}, "images-truncated-to-4", ""},
		{"mask 有图保留", map[string]any{"image": "u1", "mask": "m1"}, "u1", nil, "", ""},
		{"mask 无图拒绝", map[string]any{"mask": "m1"}, nil, nil, "", "mask 需要配合 image"},
		{"空 mask 拒绝", map[string]any{"image": "u1", "mask": "  "}, "u1", nil, "", "mask 不能是空字符串"},
		{"null mask 删字段", map[string]any{"image": "u1", "mask": nil}, "u1", nil, "", ""},
	}
	for _, c := range cases {
		payload := map[string]any{}
		for k, v := range c.in {
			payload[k] = v
		}
		warn, err := normalizeImageField(payload)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s：错误 = %v，期望包含 %q", c.name, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s：不该报错：%v", c.name, err)
			continue
		}
		if warn != c.wantWarn {
			t.Errorf("%s：警告码 %q，期望 %q", c.name, warn, c.wantWarn)
		}
		gotImage, hasImage := payload["image"]
		if c.wantImage == nil {
			if hasImage {
				t.Errorf("%s：image 应被删除，仍是 %#v", c.name, gotImage)
			}
		} else if !reflect.DeepEqual(gotImage, c.wantImage) {
			t.Errorf("%s：image = %#v，期望 %#v", c.name, gotImage, c.wantImage)
		}
		gotImages, hasImages := payload["images"]
		if c.wantImages == nil {
			if hasImages {
				t.Errorf("%s：images 应不存在，仍是 %#v", c.name, gotImages)
			}
		} else if !reflect.DeepEqual(gotImages, c.wantImages) {
			t.Errorf("%s：images = %#v，期望 %#v", c.name, gotImages, c.wantImages)
		}
	}
}

// TestImageEmptyRefsDropped 空数组/空字符串当作「没传图」，两个字段都不该转发。
func TestImageEmptyRefsDropped(t *testing.T) {
	for _, body := range []string{
		`{"model":"doubao-seedream-5-lite","prompt":"画猫","image":[]}`,
		`{"model":"doubao-seedream-5-lite","prompt":"画猫","images":[]}`,
		`{"model":"doubao-seedream-5-lite","prompt":"画猫","image":null}`,
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
		payload := lastImagePayload(t, fs)
		if v, ok := payload["image"]; ok {
			t.Errorf("空参考图仍被转发 image：%#v（请求 %s）", v, body)
		}
		if v, ok := payload["images"]; ok {
			t.Errorf("空参考图仍被转发 images：%#v（请求 %s）", v, body)
		}
	}
}

// TestImageModelChatFrontDoorMultiImages 对话前门里的多张 image_url 也要
// 折成 images 数组，不能只留第一张。
func TestImageModelChatFrontDoorMultiImages(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	body := `{"model":"Hy-Image-3.5-preview","messages":[{"role":"user","content":[
		{"type":"text","text":"把这两张图合成一张"},
		{"type":"image_url","image_url":{"url":"https://a/1.png"}},
		{"type":"image_url","image_url":{"url":"https://a/2.png"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	want := []string{"https://a/1.png", "https://a/2.png"}
	if got := imagesOf(t, lastImagePayload(t, fs)); !reflect.DeepEqual(got, want) {
		t.Errorf("对话前门多图 = %#v，期望 %#v", got, want)
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

// TestImageFieldShapes image 字段的两种合法形态（字符串 / 数组）在真实请求
// 链路上的落点：单张压成字符串、多张转成上游的 images 数组。
func TestImageFieldShapes(t *testing.T) {
	cases := []struct {
		name       string
		refField   string
		wantImage  string
		wantImages []string
	}{
		{"字符串", `"image":"https://a/1.png"`, "https://a/1.png", nil},
		{"单元素数组", `"image":["https://a/1.png"]`, "https://a/1.png", nil},
		{"多元素数组", `"image":["https://a/1.png","https://a/2.png"]`, "",
			[]string{"https://a/1.png", "https://a/2.png"}},
		{"images 字符串", `"images":"https://a/1.png"`, "https://a/1.png", nil},
		{"images 数组", `"images":["https://a/1.png","https://a/2.png"]`, "",
			[]string{"https://a/1.png", "https://a/2.png"}},
	}
	for _, c := range cases {
		fs := imageFallbackUpstream(t)
		h, _, _ := newTestHandler(t, fs.srv.URL, nil)
		_ = h.Models.Get(context.Background(), "sess")

		body := `{"model":"doubao-seedream-5-lite","prompt":"合成",` + c.refField + `}`
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s：状态码 %d：%s", c.name, w.Code, w.Body.String())
		}
		payload := lastImagePayload(t, fs)
		if c.wantImage != "" {
			if payload["image"] != c.wantImage {
				t.Errorf("%s：image = %#v，期望 %q", c.name, payload["image"], c.wantImage)
			}
			if _, exists := payload["images"]; exists {
				t.Errorf("%s：单图不该带 images：%s", c.name, fs.bodies[len(fs.bodies)-1])
			}
			continue
		}
		if got := imagesOf(t, payload); !reflect.DeepEqual(got, c.wantImages) {
			t.Errorf("%s：images = %#v，期望 %#v", c.name, got, c.wantImages)
		}
		if _, exists := payload["image"]; exists {
			t.Errorf("%s：多图不该带 image：%s", c.name, fs.bodies[len(fs.bodies)-1])
		}
	}
}

// TestImageBlankRefRejected 空串是「调用方明确写了但没写对」，不是「没传图」：
// null / [] 才表示文生图。放行空串会让一次请求静默变成纯文生图（照扣 110 分）。
func TestImageBlankRefRejected(t *testing.T) {
	for _, field := range []string{
		`"image":""`,
		`"image":"   "`,
		`"images":""`,
		`"image":["https://a/1.png",""]`,
	} {
		fs := imageFallbackUpstream(t)
		h, _, _ := newTestHandler(t, fs.srv.URL, nil)
		_ = h.Models.Get(context.Background(), "sess")
		before := len(fs.seen)

		body := `{"model":"doubao-seedream-5-lite","prompt":"画猫",` + field + `}`
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("%s：状态码 %d，期望 400：%s", field, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "空字符串") {
			t.Errorf("%s：错误文案没点明空字符串：%s", field, w.Body.String())
		}
		if len(fs.seen) != before {
			t.Errorf("%s：不合法的请求不该打上游", field)
		}
	}
}

// TestImageMaskRequiresRef mask 没有参考图时上游要么 400、要么忽略，
// 网关按 image-adapter 的 canonical 规则提前拒掉，并保证不打上游。
func TestImageMaskRequiresRef(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")
	before := len(fs.seen)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"重绘","mask":"https://a/m.png"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 %d，期望 400：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "mask 需要配合 image") {
		t.Errorf("错误文案没说明 mask 依赖 image：%s", w.Body.String())
	}
	if len(fs.seen) != before {
		t.Error("mask 缺图不该打上游")
	}
}

// TestImageMaskForwardedWithRef 有参考图时 mask 原样转发。
func TestImageMaskForwardedWithRef(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"重绘天空",
			"image":"https://a/1.png","mask":"https://a/m.png"}`))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	payload := lastImagePayload(t, fs)
	if payload["mask"] != "https://a/m.png" {
		t.Errorf("mask 没被转发：%#v", payload["mask"])
	}
	if payload["image"] != "https://a/1.png" {
		t.Errorf("image 没被转发：%#v", payload["image"])
	}
}

// TestImageRefTooLarge 单张参考图超过上限直接拒绝（上限对齐 image-adapter
// 的 MAX_IMAGE_REF_CHARS），避免把误传的整份文件当参考图送去计费。
func TestImageRefTooLarge(t *testing.T) {
	huge := strings.Repeat("A", maxImageRefChars+1)
	if _, err := imageList(huge); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Errorf("超长字符串没被拒绝：%v", err)
	}
	if _, err := imageList([]any{"https://a/1.png", huge}); err == nil {
		t.Error("数组里的超长元素没被拒绝")
	}
}

// TestChatImagePartSpellings 对话前门认得三种图片块拼法（image_url / input_image /
// image），值可以是字符串或带 url 的对象——与参考项目 image-adapter 的
// _IMAGE_PART_TYPES / _image_ref 同口径。
func TestChatImagePartSpellings(t *testing.T) {
	cases := []struct {
		name string
		part string
	}{
		{"image_url 对象", `{"type":"image_url","image_url":{"url":"https://a/1.png"}}`},
		{"image_url 字符串", `{"type":"image_url","image_url":"https://a/1.png"}`},
		{"image 字符串", `{"type":"image","image":"https://a/1.png"}`},
		{"image 对象", `{"type":"image","image":{"url":"https://a/1.png"}}`},
	}
	for _, c := range cases {
		fs := imageFallbackUpstream(t)
		h, _, _ := newTestHandler(t, fs.srv.URL, nil)
		_ = h.Models.Get(context.Background(), "sess")

		body := `{"model":"Hy-Image-3.5-preview","messages":[{"role":"user","content":[` +
			`{"type":"text","text":"换个背景"},` + c.part + `]}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("%s：状态码 %d：%s", c.name, w.Code, w.Body.String())
		}
		payload := lastImagePayload(t, fs)
		if payload["image"] != "https://a/1.png" {
			t.Errorf("%s：参考图没折进来：%s", c.name, fs.bodies[len(fs.bodies)-1])
		}
	}
}

// TestResponsesImagePartSpelling responses 前门的 input_image（字符串形态）
// 同样要折成参考图。
func TestResponsesImagePartSpelling(t *testing.T) {
	fs := imageFallbackUpstream(t)
	h, _, _ := newTestHandler(t, fs.srv.URL, nil)
	_ = h.Models.Get(context.Background(), "sess")

	body := `{"model":"Hy-Image-3.5-preview","input":[{"role":"user","content":[` +
		`{"type":"input_text","text":"换背景"},` +
		`{"type":"input_image","image_url":"https://a/1.png"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	if payload := lastImagePayload(t, fs); payload["image"] != "https://a/1.png" {
		t.Errorf("input_image 没折进来：%s", fs.bodies[len(fs.bodies)-1])
	}
}
