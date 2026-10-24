// imagefront.go 生图的「非 generations」前门。
//
// 蓝本 jushenzhidao/image-adapter：`/v1/images/edits` 与 generations 不是两套
// 语义，只是载体不同（multipart 文件上传 vs JSON）。所以这里不做第二套适配：
// 把 multipart 归一成同一个 canonical JSON，再交给 generations 那条透传管道——
// 选号、冷却、记账、模型校验全部复用。
//
// 参考图字段的真实口径（2026-10-23 读官方客户端 0.9.38 的
// electron/image/image-generation-service.js 及其 __tests__ 得到）：
// 图生图 body = { model, prompt, images: ["<url | data URI>", ...], n: 1,
// size: "2304x1728", response_format: "url" } —— **多图走 `images` 数组**，
// 单图同样是数组：客户端不分单双，一律发 `images`，且从不上报单数 `image`。
//
// 单数 `image` 是本网关的历史形态。上游对 `image` 传数组会静默忽略
// （2026-10-22 实测：出图与原图无关），传字符串则正常垫图。因此：
//   - 单图继续走 `image` 字符串（已实测有效，dhash≈1，NCC≈0.998）；
//   - 多图改走 `images` 数组（与官方客户端一致）。
//
// 前端上传器上限 4 张（main bundle 常量 `Cu=4`），网关同口径截断。
// 未知字段上游会忽略（但 `seed` / `watermark` 这类可能触发 400，见 imageslim.go）。
package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxImagePart 单张参考图上限。整包还有 withAuth 的 256MB 兜底，
// 这里限制的是「单张解码后」的大小，避免一个坏请求把内存打满。
const maxImagePart = 20 << 20

// imageSlow400Threshold 判「排队型 400」的阈值：真正的字段错误（seed/watermark
// 不认）上游秒回；排队到一分钟后才回的通用 400 再怎么减字段也一样，直接放弃精简。
// 用 var 而不是 const：测试要把它调小才能覆盖这个分支。
var imageSlow400Threshold = 60 * time.Second

// imageAttemptCap 单次上游生图调用的硬上限。实测成功请求的 p99 ≈ 869s，
// 取 900s 兜住长尾；它只管「这一次调用最多等多久」，不参与「还要不要换号」
// 的预算（那个由 image_deadline_seconds 控制）。
// 用 var 而非 const 是为了让测试能把它压到毫秒级。
var imageAttemptCap = 900 * time.Second

// maxRefImages 单次请求的参考图上限。与官方客户端对齐：前端上传器的常量是
// `Cu=4`（超出提示「最多上传 4 张图片」），网关同口径截断，只截尾不重排。
const maxRefImages = 4

// maxImageRefChars 单个参考图字符串的上限（字符数）。对齐参考项目
// image-adapter 的 MAX_IMAGE_REF_CHARS：base64 膨胀 4/3，32M 字符 ≈ 24MB 原图，
// 再大就不是「参考图」而是误传了整个文件。
const maxImageRefChars = 32 << 20

// refImageKeys 参考图字段，按优先级排列：官方客户端发 `images`（数组），
// OpenAI 兼容客户端常发 `image`（字符串或数组）。两个都收，合并后去重。
var refImageKeys = []string{"images", "image"}

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
	images, err := collectRefImages(payload)
	if err != nil {
		return nil, err
	}
	if len(images) == 0 && requireImage {
		return nil, fmt.Errorf("image 不能为空：edits 需要至少一张参考图")
	}
	if err := checkMask(payload, len(images)); err != nil {
		return nil, err
	}
	_ = applyRefImages(payload, images)
	return payload, nil
}

// imageList 把参考图字段归一成非空字符串列表。
// 支持 "url"、["url1","url2"]、null（= 不传）；空串与超长元素直接报错。
func imageList(v any) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return nil, fmt.Errorf("不能是空字符串（不传图请省略该字段或传 null）")
		}
		if len(s) > maxImageRefChars {
			return nil, fmt.Errorf("单张超过 %d MB 上限", maxImageRefChars>>20)
		}
		return []string{s}, nil
	case []string:
		out := make([]string, 0, len(x))
		for _, s := range x {
			s = strings.TrimSpace(s)
			if s == "" {
				return nil, fmt.Errorf("数组里不能有空字符串")
			}
			if len(s) > maxImageRefChars {
				return nil, fmt.Errorf("单张超过 %d MB 上限", maxImageRefChars>>20)
			}
			out = append(out, s)
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("数组元素必须是字符串（URL、data URI 或 base64）")
			}
			s = strings.TrimSpace(s)
			if s == "" {
				return nil, fmt.Errorf("数组里不能有空字符串")
			}
			if len(s) > maxImageRefChars {
				return nil, fmt.Errorf("单张超过 %d MB 上限", maxImageRefChars>>20)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("只接受字符串或字符串数组（URL、data URI 或 base64）")
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
	if mask != "" {
		payload["mask"] = mask
	}
	if err := checkMask(payload, len(images)); err != nil {
		return nil, err
	}
	_ = applyRefImages(payload, images)
	if s, ok := payload["n"].(string); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			payload["n"] = n
		}
	}
	return payload, nil
}

// normalizeImageField 把参考图字段收敛成上游能吃的形态，返回给客户端看的警告码
// （空 = 无需告警）。直通路径与两个前门共用同一套收敛。
//
// 输出形态：
//   - 0 张：`image` / `images` 都删掉（别把空值当参考图送上去）；
//   - 1 张：`image` 字符串——2026-10-22 实测有效（dhash≈1，NCC≈0.998）；
//   - 多张：`images` 数组——官方客户端 0.9.38 的图生图形态。
func normalizeImageField(payload map[string]any) (string, error) {
	if payload == nil {
		return "", nil
	}
	images, err := collectRefImages(payload)
	if err != nil {
		return "", err
	}
	if err := checkMask(payload, len(images)); err != nil {
		return "", err
	}
	return applyRefImages(payload, images), nil
}

// checkMask mask 只有配合参考图才有意义：没有 image 的 mask 上游要么 400、
// 要么静默忽略，提前拒掉比让它变成一次没必要的上游调用清楚（与 image-adapter
// 的 canonical 校验同口径）。
func checkMask(payload map[string]any, imageCount int) error {
	raw, exists := payload["mask"]
	if !exists {
		return nil
	}
	if raw == nil {
		delete(payload, "mask") // null = 没有蒙版，别把空值送上游
		return nil
	}
	s, ok := raw.(string)
	if !ok {
		return fmt.Errorf("mask 必须是字符串（URL、data URI 或 base64）")
	}
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("mask 不能是空字符串（没有蒙版请省略该字段）")
	}
	if len(s) > maxImageRefChars {
		return fmt.Errorf("mask 超过 %d MB 上限", maxImageRefChars>>20)
	}
	if imageCount == 0 {
		return fmt.Errorf("mask 需要配合 image 使用：没有参考图时不要传 mask")
	}
	if s != strings.TrimSpace(s) {
		payload["mask"] = strings.TrimSpace(s)
	}
	return nil
}

// collectRefImages 合并 `images` 与 `image` 两个字段，保持出现顺序。
// 去重只发生在「跨字段」：同一字段内的重复要留着，否则提示词里的「第二张」
// 会因为两张图恰好相同而被悄悄抹掉下标。
// 返回 error 表示字段形态不合法（类型不对、空串、单张超限）——调用方回 400，
// 不要静默透传：`""` 这种「看起来传了图」的请求一旦放行，就变成一次纯文生图。
func collectRefImages(payload map[string]any) ([]string, error) {
	var out []string
	seenFromPrev := make(map[string]struct{})
	for _, key := range refImageKeys {
		v, exists := payload[key]
		if !exists {
			continue
		}
		list, err := imageList(v)
		if err != nil {
			return nil, fmt.Errorf("%s 字段不合法：%w", key, err)
		}
		for _, s := range list {
			if _, dup := seenFromPrev[s]; dup {
				continue
			}
			out = append(out, s)
		}
		for _, s := range list {
			seenFromPrev[s] = struct{}{}
		}
	}
	return out, nil
}

// applyRefImages 写入参考图字段并按上限截断，返回警告码。
// 顺序敏感：提示词里的「第一张 / 第二张」依赖数组顺序，所以只截尾不重排。
func applyRefImages(payload map[string]any, images []string) string {
	warn := ""
	if len(images) > maxRefImages {
		warn = fmt.Sprintf("images-truncated-to-%d", maxRefImages)
		images = images[:maxRefImages]
	}
	switch len(images) {
	case 0:
		delete(payload, "image")
		delete(payload, "images")
	case 1:
		payload["image"] = images[0]
		delete(payload, "images")
	default:
		arr := make([]any, len(images))
		for i, s := range images {
			arr[i] = s
		}
		payload["images"] = arr
		delete(payload, "image")
	}
	return warn
}

// imageNote 给日志补一句「这次请求带了参考图」，回退模型时能一眼看出
// 是图生图被换了模型（换成不支持垫图的模型就是白扣分）。
func imageNote(payload map[string]any) string {
	images, err := collectRefImages(payload)
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
	payload, warn := imagePayload(req, model, prompt, images)
	if warn != "" {
		w.Header().Set("X-Loomy-Image-Warn", warn)
	}
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
	payload, warn := imagePayload(req, model, prompt, images)
	if warn != "" {
		w.Header().Set("X-Loomy-Image-Warn", warn)
	}
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
				case "image_url", "input_image", "image":
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

// imageURLOf 从内容块里取图片引用，兼容各家拼法（与 image-adapter 的
// _IMAGE_PART_TYPES / _image_ref 同口径）：
//
//	{"type":"image_url","image_url":{"url":"…"}}   OpenAI chat
//	{"type":"input_image","image_url":"…"}         OpenAI responses
//	{"type":"image","image":"…"}                   部分客户端
//	{"type":"image","image":{"url":"…"}}           同上，对象形态
//
// 值可以是字符串（URL / data URI / base64），也可以是带 url 的对象。
func imageURLOf(part map[string]any) string {
	for _, key := range []string{"image_url", "image", "url"} {
		switch v := part[key].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case map[string]any:
			if s := strings.TrimSpace(str(v["url"])); s != "" {
				return s
			}
		}
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

// imagePayload 组装生成请求，返回给客户端看的警告码（参考图被截断时非空）。
func imagePayload(req map[string]any, model, prompt string, images []string) (map[string]any, string) {
	p := map[string]any{"model": model, "prompt": prompt}
	// 与直通路径同一套收敛：1 张走 image 字符串，多张走 images 数组。
	warn := applyRefImages(p, images)
	if warn != "" {
		log.Printf("[image] 对话前门参考图超过 %d 张，已截断（%s）", maxRefImages, warn)
	}
	if n, ok := req["n"]; ok {
		p["n"] = n
	}
	if s := str(req["size"]); s != "" {
		p["size"] = s
	}
	return p, warn
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
