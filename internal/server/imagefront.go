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
)

// maxImagePart 单张参考图上限。整包还有 withAuth 的 256MB 兜底，
// 这里限制的是「单张解码后」的大小，避免一个坏请求把内存打满。
const maxImagePart = 20 << 20

// imageEdits POST /v1/images/edits：OpenAI multipart 前门（也接受 JSON）。
// variations 是同一语义的旧端点，直接复用。
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
		return nil, fmt.Errorf("image 不能为空：edits / variations 需要至少一张参考图")
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
		return nil, fmt.Errorf("image 不能为空：edits / variations 需要至少一张参考图")
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
