package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rsfree/loomy2api/internal/upstream"
)

// 生图请求的「400 减肥重试」。
//
// 现象（2026-10-22 线上）：客户端按 Seedream 4.5 / 万相新版的原生参数发请求
// （stream、sequential_image_generation、watermark、seed、n>1 …），网关按家族
// 把它回退到 doubao-seedream-5-lite 后，Loomy 背后的 tencent-tokenhub 直接回
//
//	400 {"message":"请求有误，请重试；若多次出现请更新应用版本"}
//
// 这些扩展字段对出图没有作用，丢掉比把 400 抛给用户好。两级降级：
// 先去掉白名单外的字段，仍 400 就只留 model / prompt / image / size。

// imageCoreKeys 生图请求的核心字段（其余都可能被下游 provider 拒绝）。
// `images` 必须在内：多图走的是这个字段，减肥时把它丢掉等于把垫图也丢了
// （上游不会报错，只会安静地出一张文生图）。
var imageCoreKeys = map[string]bool{
	"model": true, "prompt": true, "image": true, "images": true,
	"size": true, "n": true, "response_format": true,
}

// slimImageBody 去掉白名单外的扩展字段；n>1 一并去掉（多数图像模型一次只出一张）。
// 没有可去掉的内容时 ok=false，调用方不要重试。
func slimImageBody(payload map[string]any) (map[string]any, []string, bool) {
	out := make(map[string]any, len(payload))
	var dropped []string
	for k, v := range payload {
		if imageCoreKeys[k] {
			out[k] = v
			continue
		}
		dropped = append(dropped, k)
	}
	if n, ok := out["n"]; ok && numOf(n) > 1 {
		delete(out, "n")
		dropped = append(dropped, "n")
	}
	if len(dropped) == 0 {
		return nil, nil, false
	}
	sort.Strings(dropped)
	return out, dropped, true
}

// minimalImageBody 只留 model / prompt / image(images) / size。
// 已经是最小集合时 ok=false。
func minimalImageBody(payload map[string]any) (map[string]any, bool) {
	keep := map[string]bool{"model": true, "prompt": true, "image": true, "images": true, "size": true}
	out := make(map[string]any, len(keep))
	extra := 0
	for k, v := range payload {
		if keep[k] {
			out[k] = v
			continue
		}
		extra++
	}
	if extra == 0 {
		return nil, false
	}
	return out, true
}

// imageDigest 记录请求结构（不含 prompt 正文），用于定位是哪个字段被拒。
func imageDigest(payload map[string]any) string {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	img := "none"
	switch v := payload["images"].(type) {
	case []any:
		if len(v) > 0 {
			img = fmt.Sprintf("images(%d)", len(v))
		}
	}
	if img == "none" {
		switch v := payload["image"].(type) {
		case string:
			img = "single"
		case []any:
			img = fmt.Sprintf("array(%d)", len(v))
		}
	}
	return fmt.Sprintf("keys=%v size=%v n=%v image=%s", keys, payload["size"], payload["n"], img)
}

// shrinkImageRetry 生图 400 后按两级减肥重试（同一账号、同一上下文）。
// deadline 非零时，跨过预算就不再重试：上游排队到 300s 才回 400 的场景里，
// 三级各等一次会把一次请求拖到 15 分钟，字段再怎么减也没用。
// 返回最终响应、实际采用的 payload、被去掉的键。
func (h *Handler) shrinkImageRetry(ctx context.Context, suffix, session string,
	payload map[string]any, resp *upstream.Response, deadline time.Time) (*upstream.Response, map[string]any, []string) {

	orig := resp.BodyText() // 先读掉，避免连接泄漏
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		log.Printf("[image] 已到总时限，跳过精简重试（最后状态 %d）", resp.Status)
		return withBody(resp, orig), payload, nil
	}
	slim, dropped, ok := slimImageBody(payload)
	if !ok {
		return withBody(resp, orig), payload, nil
	}
	log.Printf("[image] 上游 400，去掉扩展字段 %v 重试（%s）", dropped, imageDigest(payload))
	r2, err := h.Upstream.Do(ctx, http.MethodPost, suffix, session, slim, nil)
	if err != nil {
		return withBody(resp, orig), payload, nil
	}
	resp, payload = r2, slim
	if resp.Status != http.StatusBadRequest {
		return resp, payload, dropped
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return withBody(resp, resp.BodyText()), payload, dropped
	}

	body2 := resp.BodyText()
	min, ok := minimalImageBody(payload)
	if !ok {
		return withBody(resp, body2), payload, dropped
	}
	log.Printf("[image] 仍 400，退到核心字段重试（%s）", imageDigest(payload))
	r3, err := h.Upstream.Do(ctx, http.MethodPost, suffix, session, min, nil)
	if err != nil {
		return withBody(resp, body2), payload, dropped
	}
	return r3, min, dropped
}

// withBody 把已读出的响应体重挂回去，保证调用方仍能拿到上游错误原文。
func withBody(resp *upstream.Response, body string) *upstream.Response {
	if resp == nil {
		return nil
	}
	resp.Body = io.NopCloser(strings.NewReader(body))
	return resp
}
