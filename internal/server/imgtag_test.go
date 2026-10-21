package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// bodyOf 从 JSON 字面量取 body，省掉手搓嵌套 map 的噪音。
func bodyOf(t *testing.T, raw string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("测试 JSON 不合法: %v", err)
	}
	return body
}

// contentOf 取第一条消息的 content。
func contentOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("messages 为空")
	}
	parts, _ := msgs[0].(map[string]any)["content"].([]any)
	return parts
}

// deepseek 系：data URI 图片块被并进前一个文本块，图片块本身消失。
func TestRewriteImageTagsForDeepSeek(t *testing.T) {
	body := bodyOf(t, `{"messages":[{"role":"user","content":[
		{"type":"text","text":"这张图是什么"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`)
	if n := rewriteImageTags(body, "deepseek-v4-flash-0731"); n != 1 {
		t.Fatalf("转换数 = %d，期望 1", n)
	}
	parts := contentOf(t, body)
	if len(parts) != 1 {
		t.Fatalf("图片块应被并入，实际剩 %d 块: %v", len(parts), parts)
	}
	text := str(parts[0].(map[string]any)["text"])
	if !strings.Contains(text, "<image_base64>AAA</image_base64>") || !strings.HasPrefix(text, "这张图是什么\n") {
		t.Errorf("文本 = %q，期望原文在前、标记在后", text)
	}
}

// GLM 系：保持 image_url 原样（转了反而看不见图）。
func TestRewriteImageTagsSkipsNonDeepSeek(t *testing.T) {
	raw := `{"messages":[{"role":"user","content":[
		{"type":"text","text":"看图"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,BBB"}}]}]}`
	body := bodyOf(t, raw)
	if n := rewriteImageTags(body, "GLM-5.3-Flash"); n != 0 {
		t.Fatalf("非 deepseek 模型不该转换，实际 %d", n)
	}
	parts := contentOf(t, body)
	if len(parts) != 2 || str(parts[1].(map[string]any)["type"]) != "image_url" {
		t.Errorf("图片块应原样保留: %v", parts)
	}
}

// http(s) 图片 URL 不转：标记里放 URL 没有实测依据。
func TestRewriteImageTagsSkipsURLImages(t *testing.T) {
	body := bodyOf(t, `{"messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`)
	if n := rewriteImageTags(body, "deepseek-v4.1-flash"); n != 0 {
		t.Fatalf("URL 图片不该转换，实际 %d", n)
	}
}

// 图片是唯一内容（前面没有文本块）：标记自己成一块，并保留简化写法 {"image_url":"data:…"}。
func TestRewriteImageTagsSoloImage(t *testing.T) {
	body := bodyOf(t, `{"messages":[{"role":"user","content":[
		{"type":"image_url","image_url":"data:image/jpeg;base64,CCC"}]}]}`)
	if n := rewriteImageTags(body, "deepseek-v4-flash-0731"); n != 1 {
		t.Fatalf("转换数 = %d，期望 1", n)
	}
	parts := contentOf(t, body)
	if len(parts) != 1 || str(parts[0].(map[string]any)["text"]) != "<image_base64>CCC</image_base64>" {
		t.Errorf("独图应转成单块标记: %v", parts)
	}
}

// 纯文本请求（绝大多数）：一个字都不改。
func TestRewriteImageTagsNoImages(t *testing.T) {
	body := bodyOf(t, `{"messages":[{"role":"user","content":"你好"}]}`)
	if n := rewriteImageTags(body, "deepseek-v4-flash-0731"); n != 0 {
		t.Fatalf("纯文本不该转换，实际 %d", n)
	}
	parts := body["messages"].([]any)[0].(map[string]any)["content"]
	if str(parts) != "你好" {
		t.Errorf("纯文本内容被改写: %v", parts)
	}
}
