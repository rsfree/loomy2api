// imagefront.go 生图的「非 generations」前门。
//
// 蓝本 jushenzhidao/image-adapter：`/v1/images/edits` 与 generations 不是两套
// 语义，只是载体不同（multipart 文件上传 vs JSON）。所以这里不做第二套适配：
// 把 multipart 归一成同一个 canonical JSON（image 可以是字符串或数组、mask 原样），
// 再交给 generations 那条透传管道——选号、冷却、记账、模型校验全部复用。
//
// 实测上游（2026-10-22）：
//   - `image` 传数组 = 多图输入，主体能同时保留（两张参考图成功合成）；
//   - `image` 传 URL 或 data URI 都可以；
//   - 字段本身是透传的，未知字段上游会忽略。
package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxImagePart 单张参考图上限。整包还有 withAuth 的 256MB 兜底，
// 这里限制的是「单张解码后」的大小，避免一个坏请求把内存打满。
const maxImagePart = 20 << 20

// imageEdits POST /v1/images/edits：OpenAI multipart 前门（也接受 JSON）。
func (h *Handler) imageEdits(w http.ResponseWriter, r *http.Request) {
	h.imageFront(w, r, true)
}

func (h *Handler) imageFront(w http.ResponseWriter, r *http.Request, requireImage bool) {
	payload, err := parseImageFront(r, requireImage)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求体序列化失败: "+err.Error(), "invalid_request_error")
		return
	}
	// 复用 generations 的透传管道：重写 body 后直接交给它。
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))
	r2.Header = r.Header.Clone()
	r2.Header.Set("Content-Type", "application/json")
	h.passthrough("images/generations", "image")(w, r2)
}

// parseImageFront 把两种载体归一成 canonical JSON。
func parseImageFront(r *http.Request, requireImage bool) (map[string]any, error) {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "multipart/form-data") {
		return parseMultipartImage(r, requireImage)
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("读取请求体失败: %w", err)
	}
	payload := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
		}
	}
	images, err := imageList(payload["image"])
	if err != nil {
		return nil, err
	}
	if len(images) == 0 && requireImage {
		return nil, fmt.Errorf("image 不能为空：edits 需要至少一张参考图")
	}
	switch len(images) {
	case 0:
		delete(payload, "image")
	case 1:
		payload["image"] = images[0]
	default:
		arr := make([]any, len(images))
		for i, s := range images {
			arr[i] = s
		}
		payload["image"] = arr
	}
	if m, ok := payload["mask"].(string); ok && strings.TrimSpace(m) == "" {
		delete(payload, "mask")
	}
	return payload, nil
}

// imageList 把 image 字段归一成非空字符串列表。
// 支持 "url"、["url1","url2"]，以及空值（= 不传）。
func imageList(v any) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return nil, fmt.Errorf("image 不能是空字符串（不传图请省略该字段）")
		}
		return []string{s}, nil
	case []string:
		out := make([]string, 0, len(x))
		for _, s := range x {
			if strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("image 数组里不能有空字符串")
			}
			out = append(out, strings.TrimSpace(s))
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			s, ok := item.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("image 数组元素必须是非空字符串")
			}
			out = append(out, strings.TrimSpace(s))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("image 字段类型不支持（只接受字符串或字符串数组）")
	}
}

// parseMultipartImage 流式读取 multipart：保持 image[] 的顺序（提示词里
// 「第一张/第二张」依赖顺序，用 ParseMultipartForm 的 map 会丢序）。
func parseMultipartImage(r *http.Request, requireImage bool) (map[string]any, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("解析 multipart 失败: %w", err)
	}
	payload := map[string]any{}
	var images []string
	mask := ""

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 multipart 失败: %w", err)
		}
		name := part.FormName()
		data, err := io.ReadAll(io.LimitReader(part, maxImagePart+1))
		_ = part.Close()
		if err != nil {
			return nil, fmt.Errorf("读取字段 %s 失败: %w", name, err)
		}
		if len(data) > maxImagePart {
			return nil, fmt.Errorf("字段 %s 超过 %d MB 上限", name, maxImagePart>>20)
		}

		isImage := strings.HasPrefix(name, "image")
		if !isImage && name != "mask" {
			payload[name] = strings.TrimSpace(string(data))
			continue
		}
		// 文件 part、data URI、裸二进制 → data URI；普通 URL 文本原样保留。
		uri, ok := partDataURI(data)
		if !ok {
			if len(bytes.TrimSpace(data)) == 0 {
				continue // 空 part 视同没传（与 image-adapter 同口径）
			}
			uri = strings.TrimSpace(string(data))
		}
		if name == "mask" {
			mask = uri
			continue
		}
		images = append(images, uri)
	}

	if len(images) == 0 && requireImage {
		return nil, fmt.Errorf("image 不能为空：edits 需要至少一张参考图")
	}
	switch len(images) {
	case 0:
	case 1:
		payload["image"] = images[0]
	default:
		arr := make([]any, len(images))
		for i, s := range images {
			arr[i] = s
		}
		payload["image"] = arr
	}
	if mask != "" {
		payload["mask"] = mask
	}
	if s, ok := payload["n"].(string); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			payload["n"] = n
		}
	}
	return payload, nil
}

// normalizeImageField 把 image 收敛成上游认识的最小形态：
// 单元素数组压成字符串，空值直接删掉，多图数组保持不动。
//
// 为什么要这一层：edits / chat / responses 三个前门早就归一过，但
// generations 是直通（body 原样转发），`"image": ["url"]` 会原封不动上去。
// 上游对数组形态不报错、只是把参考图丢掉——出图成功但完全不像原图，
// 也就是「垫图没生效」。同一个垫图走不同前门结果不同，这种不一致必须在
// 网关侧收口。
func normalizeImageField(payload map[string]any) {
	if payload == nil {
		return
	}
	// 空白字符串：imageList 会当成「错误」以便 edits 前门回 400，
	// 但直通路径上更合理的是视同没传图。
	if s, ok := payload["image"].(string); ok && strings.TrimSpace(s) == "" {
		delete(payload, "image")
		return
	}
	images, err := imageList(payload["image"])
	if err != nil {
		return // 类型不对就原样透传，让上游自己给出报错
	}
	switch len(images) {
	case 0:
		delete(payload, "image")
	case 1:
		payload["image"] = images[0]
	}
}

// imageNote 给日志补一句「这次请求带了参考图」，回退模型时能一眼看出
// 是图生图被换了模型（换成不支持垫图的模型就是白扣分）。
func imageNote(payload map[string]any) string {
	images, err := imageList(payload["image"])
	if err != nil || len(images) == 0 {
		return ""
	}
	return fmt.Sprintf("（请求含 %d 张参考图）", len(images))
}

// partDataURI 判断 part 内容是不是图片二进制，是则转 data URI。
// 文本 URL 返回 false（调用方保留原文）。
func partDataURI(data []byte) (string, bool) {
	if len(data) == 0 {
		return "", false
	}
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("data:image/")) {
		return string(trimmed), true
	}
	if bytes.HasPrefix(trimmed, []byte("http://")) || bytes.HasPrefix(trimmed, []byte("https://")) {
		return "", false
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return "", false
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), true
}

// ---------------------------------------------------------------- 对话前门

// captureWriter 把 passthrough 的输出收进内存，供 chat / responses 前门二次封装。
type captureWriter struct {
	h    http.Header
	code int
	buf  bytes.Buffer
}

func (c *captureWriter) Header() http.Header {
	if c.h == nil {
		c.h = http.Header{}
	}
	return c.h
}
func (c *captureWriter) WriteHeader(code int) { c.code = code }
func (c *captureWriter) Write(p []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return c.buf.Write(p)
}

// imageChat 图像模型的 /v1/chat/completions 前门。
func (h *Handler) imageChat(w http.ResponseWriter, r *http.Request, req map[string]any, model string) {
	prompt, images := chatPromptImages(req)
	payload := imagePayload(req, model, prompt, images)
	code, out := h.generateImageCaptured(r, payload)
	if code != http.StatusOK {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_, _ = w.Write(out)
		return
	}
	urls, pts := imageURLs(out)
	text := markdownImages(urls)
	created := time.Now().Unix()
	id := fmt.Sprintf("imgchat_%d", time.Now().UnixNano())

	if truthy(req["stream"]) {
		writeImageChatStream(w, id, model, created, text, pts)
		return
	}
	writeJSON(w, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": imageUsage(pts),
	})
}

// imageResponses 图像模型的 /v1/responses 前门。
func (h *Handler) imageResponses(w http.ResponseWriter, r *http.Request, req map[string]any, model string) {
	prompt, images := responsesPromptImages(req)
	payload := imagePayload(req, model, prompt, images)
	code, out := h.generateImageCaptured(r, payload)
	if code != http.StatusOK {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_, _ = w.Write(out)
		return
	}
	if truthy(req["stream"]) {
		writeErr(w, http.StatusBadRequest, "生图 responses 前门暂不支持 stream=true；请去掉 stream 或改用 /v1/chat/completions", "invalid_request_error")
		return
	}
	urls, pts := imageURLs(out)
	writeJSON(w, map[string]any{
		"id":         fmt.Sprintf("resp_%d", time.Now().UnixNano()),
		"object":     "response",
		"created_at": time.Now().Unix(),
		"model":      model,
		"status":     "completed",
		"output": []any{map[string]any{
			"id":     fmt.Sprintf("msg_%d", time.Now().UnixNano()),
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": markdownImages(urls), "annotations": []any{},
			}},
		}},
		"usage": imageUsage(pts),
	})
}

// generateImageCaptured 复用 generations 透传管道（选号/冷却/记账全一致），
// 只是把响应收进内存而不是直接写给客户端。
func (h *Handler) generateImageCaptured(r *http.Request, payload map[string]any) (int, []byte) {
	body, err := json.Marshal(payload)
	if err != nil {
		return http.StatusBadRequest, []byte(`{"error":{"message":"序列化失败"}}`)
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))
	r2.Header = r.Header.Clone()
	r2.Header.Set("Content-Type", "application/json")
	cw := &captureWriter{}
	h.passthrough("images/generations", "image")(cw, r2)
	if cw.code == 0 {
		cw.code = http.StatusOK
	}
	return cw.code, cw.buf.Bytes()
}

// chatPromptImages 把 chat messages 折成 prompt + 参考图列表。
func chatPromptImages(req map[string]any) (string, []string) {
	var texts []string
	var images []string
	msgs, _ := req["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		switch c := m["content"].(type) {
		case string:
			if strings.TrimSpace(c) != "" {
				texts = append(texts, c)
			}
		case []any:
			for _, p := range c {
				part, _ := p.(map[string]any)
				switch str(part["type"]) {
				case "text", "input_text":
					if s := str(part["text"]); strings.TrimSpace(s) != "" {
						texts = append(texts, s)
					}
				case "image_url", "input_image":
					if u := imageURLOf(part); u != "" {
						images = append(images, u)
					}
				}
			}
		}
	}
	return joinPrompt(texts), images
}

// responsesPromptImages 把 responses input 折成 prompt + 参考图列表。
func responsesPromptImages(req map[string]any) (string, []string) {
	var texts []string
	var images []string
	switch in := req["input"].(type) {
	case string:
		texts = append(texts, in)
	case []any:
		for _, raw := range in {
			item, _ := raw.(map[string]any)
			if s := str(item["text"]); strings.TrimSpace(s) != "" {
				texts = append(texts, s)
			}
			if u := imageURLOf(item); u != "" {
				images = append(images, u)
			}
			content, _ := item["content"].([]any)
			for _, c := range content {
				part, _ := c.(map[string]any)
				if s := str(part["text"]); strings.TrimSpace(s) != "" {
					texts = append(texts, s)
				}
				if u := imageURLOf(part); u != "" {
					images = append(images, u)
				}
			}
		}
	}
	return joinPrompt(texts), images
}

func imageURLOf(part map[string]any) string {
	switch v := part["image_url"].(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		return strings.TrimSpace(str(v["url"]))
	}
	return ""
}

func joinPrompt(parts []string) string {
	s := strings.TrimSpace(strings.Join(parts, "\n"))
	if s == "" {
		return "根据参考图生成图片"
	}
	return s
}

func imagePayload(req map[string]any, model, prompt string, images []string) map[string]any {
	p := map[string]any{"model": model, "prompt": prompt}
	switch len(images) {
	case 0:
	case 1:
		p["image"] = images[0]
	default:
		arr := make([]any, len(images))
		for i, s := range images {
			arr[i] = s
		}
		p["image"] = arr
	}
	if n, ok := req["n"]; ok {
		p["n"] = n
	}
	if s := str(req["size"]); s != "" {
		p["size"] = s
	}
	return p
}

func imageURLs(out []byte) ([]string, int64) {
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	var urls []string
	if data, ok := obj["data"].([]any); ok {
		for _, it := range data {
			m, _ := it.(map[string]any)
			if u := str(m["url"]); u != "" {
				urls = append(urls, u)
			}
		}
	}
	pts := int64(0)
	switch v := obj["points_consumed"].(type) {
	case float64:
		pts = int64(v)
	case int64:
		pts = v
	}
	return urls, pts
}

func markdownImages(urls []string) string {
	if len(urls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(urls))
	for _, u := range urls {
		parts = append(parts, "![image]("+u+")\n"+u)
	}
	return strings.Join(parts, "\n\n")
}

func imageUsage(pts int64) map[string]any {
	return map[string]any{
		"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
		"points_consumed": pts,
	}
}

func writeImageChatStream(w http.ResponseWriter, id, model string, created int64, text string, pts int64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	chunk := func(delta map[string]any, finish any) {
		obj := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(obj)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	chunk(map[string]any{"role": "assistant"}, nil)
	chunk(map[string]any{"content": text}, nil)
	chunk(map[string]any{}, "stop")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}
