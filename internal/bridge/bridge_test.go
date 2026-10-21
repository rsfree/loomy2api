package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToOpenAIPlainText 字符串 content 直接透传。
func TestToOpenAIPlainText(t *testing.T) {
	req := map[string]any{
		"model":    "imodel/spark-x",
		"system":   "be nice",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out := ToOpenAI(req)
	msgs := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息（system + user），实际 %d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be nice" {
		t.Errorf("system 未正确翻译: %v", first)
	}
}

// TestToOpenAIToolRoundTrip tool_use / tool_result 双向翻译。
func TestToOpenAIToolRoundTrip(t *testing.T) {
	req := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{map[string]any{
					"type":  "tool_use",
					"id":    "toolu_1",
					"name":  "get_weather",
					"input": map[string]any{"city": "北京"},
				}},
			},
			map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content":     "晴 25°C",
				}},
			},
		},
	}
	out := ToOpenAI(req)
	msgs := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息，实际 %d: %v", len(msgs), msgs)
	}
	assistant := msgs[0].(map[string]any)
	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant 应带 1 个 tool_call: %v", assistant)
	}
	call := calls[0].(map[string]any)
	if call["id"] != "toolu_1" {
		t.Errorf("tool_call id 未保留: %v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("函数名未保留: %v", fn)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments 不是合法 JSON: %v", err)
	}
	if args["city"] != "北京" {
		t.Errorf("参数未保留: %v", args)
	}

	// tool_result 应变成独立的 tool 消息，且不产生空 user 脚手架
	toolMsg := msgs[1].(map[string]any)
	if toolMsg["role"] != "tool" {
		t.Errorf("tool_result 应翻译成 tool 消息，实际 %v", toolMsg["role"])
	}
	if toolMsg["tool_call_id"] != "toolu_1" {
		t.Errorf("tool_call_id 未保留: %v", toolMsg)
	}
	if toolMsg["content"] != "晴 25°C" {
		t.Errorf("工具结果未保留: %v", toolMsg["content"])
	}
}

// TestToOpenAITools 工具定义翻译（Anthropic input_schema → parameters）。
func TestToOpenAITools(t *testing.T) {
	req := map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
		"tools": []any{map[string]any{
			"name":        "search",
			"description": "联网搜索",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"q": map[string]any{"type": "string"}},
			},
		}},
	}
	out := ToOpenAI(req)
	tools, ok := out["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools 未翻译: %v", out["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "search" || fn["description"] != "联网搜索" {
		t.Errorf("工具元信息丢失: %v", fn)
	}
	if fn["parameters"] == nil {
		t.Error("parameters 不应为空")
	}
}

// TestToOpenAIMaxTokens 关键参数必须原样带过去。
func TestToOpenAIMaxTokens(t *testing.T) {
	out := ToOpenAI(map[string]any{
		"model": "m", "max_tokens": 4096, "temperature": 0.7, "top_p": 0.9,
		"stream":         true,
		"messages":       []any{map[string]any{"role": "user", "content": "x"}},
		"stop_sequences": []any{"\n\n"},
	})
	if out["max_tokens"] != 4096 {
		t.Errorf("max_tokens 丢失: %v", out["max_tokens"])
	}
	if out["temperature"] != 0.7 || out["top_p"] != 0.9 {
		t.Error("采样参数丢失")
	}
	if out["stream"] != true {
		t.Error("stream 丢失")
	}
	if out["stop"] == nil {
		t.Error("stop_sequences 应映射成 stop")
	}
}

// TestToAnthropicText 非流式响应翻译。
func TestToAnthropicText(t *testing.T) {
	obj := map[string]any{
		"id": "chatcmpl-1", "model": "imodel/spark-x", "created": 1700000000,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role": "assistant", "content": "你好",
				"reasoning_content": "先想一想",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "points_consumed": 3},
	}
	out := ToAnthropic(obj, "imodel/spark-x")
	content := out["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("期望 2 个块（thinking + text），实际 %d: %v", len(content), content)
	}
	thinking := content[0].(map[string]any)
	if thinking["type"] != "thinking" || thinking["thinking"] != "先想一想" {
		t.Errorf("思考块不正确: %v", thinking)
	}
	text := content[1].(map[string]any)
	if text["type"] != "text" || text["text"] != "你好" {
		t.Errorf("文本块不正确: %v", text)
	}
	if out["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", out["stop_reason"])
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 10 || usage["output_tokens"] != 20 {
		t.Errorf("usage 未映射: %v", usage)
	}
	if usage["points_consumed"] != 3 {
		t.Error("points_consumed 应透出")
	}
}

// TestToAnthropicToolUse finish_reason=tool_calls 应映射为 tool_use。
func TestToAnthropicToolUse(t *testing.T) {
	obj := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "f", "arguments": `{"a":1}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	}
	out := ToAnthropic(obj, "m")
	if out["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v，期望 tool_use", out["stop_reason"])
	}
	block := out["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "f" {
		t.Errorf("工具块不正确: %v", block)
	}
	input := block["input"].(map[string]any)
	if input["a"] != float64(1) {
		t.Errorf("参数未解析: %v", input)
	}
}

// TestStopReasonMapping finish_reason 全覆盖。
func TestStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"length":        "max_tokens",
		"tool_calls":    "tool_use",
		"function_call": "tool_use",
		"stop":          "end_turn",
		"":              "end_turn",
	}
	for in, want := range cases {
		if got := StopReason(in); got != want {
			t.Errorf("StopReason(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestStreamTranslatorText 流式文本增量翻译成 text 块。
func TestStreamTranslatorText(t *testing.T) {
	st := NewStreamTranslator("m", "msg_1")
	events := st.Start()
	if len(events) != 1 || events[0].Name != "message_start" {
		t.Fatalf("首个事件应是 message_start: %v", events)
	}

	var out []Event
	out = append(out, st.Feed(map[string]any{
		"choices": []any{map[string]any{
			"delta":         map[string]any{"content": "你"},
			"finish_reason": nil,
		}},
	})...)
	out = append(out, st.Feed(map[string]any{
		"choices": []any{map[string]any{
			"delta":         map[string]any{"content": "好"},
			"finish_reason": "stop",
		}},
	})...)
	out = append(out, st.Finish()...)

	var names []string
	var text strings.Builder
	for _, ev := range out {
		names = append(names, ev.Name)
		if ev.Name == "content_block_delta" {
			d := ev.Data["delta"].(map[string]any)
			if d["type"] == "text_delta" {
				text.WriteString(d["text"].(string))
			}
		}
	}
	if text.String() != "你好" {
		t.Errorf("聚合文本 = %q，期望 你好", text.String())
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "content_block_start") {
		t.Error("缺少 content_block_start")
	}
	if !strings.Contains(joined, "message_delta") || !strings.Contains(joined, "message_stop") {
		t.Errorf("缺少收尾事件: %s", joined)
	}
	// content_block_start 只应出现一次（不能每帧都开新块）
	if strings.Count(joined, "content_block_start") != 1 {
		t.Errorf("content_block_start 次数 = %d，期望 1", strings.Count(joined, "content_block_start"))
	}
}

// TestStreamTranslatorThinking 思考链应先于文本开启，且切换时关闭前一块。
func TestStreamTranslatorThinking(t *testing.T) {
	st := NewStreamTranslator("m", "msg_1")
	var out []Event
	out = append(out, st.Feed(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": "想"}}},
	})...)
	out = append(out, st.Feed(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": "答"}}},
	})...)
	out = append(out, st.Finish()...)

	var idxThinking, idxText = -1, -1
	stops := 0
	for _, ev := range out {
		switch ev.Name {
		case "content_block_start":
			block := ev.Data["content_block"].(map[string]any)
			if block["type"] == "thinking" && idxThinking < 0 {
				idxThinking = ev.Data["index"].(int)
			}
			if block["type"] == "text" && idxText < 0 {
				idxText = ev.Data["index"].(int)
			}
		case "content_block_stop":
			stops++
		}
	}
	if idxThinking < 0 || idxText < 0 {
		t.Fatalf("应同时出现 thinking 与 text 块: %d / %d", idxThinking, idxText)
	}
	if idxThinking >= idxText {
		t.Errorf("thinking 块应在 text 之前（index %d vs %d）", idxThinking, idxText)
	}
	if stops != 2 {
		t.Errorf("两个块都应收尾，实际 %d 次 stop", stops)
	}
}

// TestStreamTranslatorToolArgsAccumulated 分帧发送的工具参数要被拼接完整。
func TestStreamTranslatorToolArgsAccumulated(t *testing.T) {
	st := NewStreamTranslator("m", "msg_1")
	st.Start()
	st.Feed(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_1",
				"function": map[string]any{"name": "search", "arguments": `{"q":`},
			}},
		}}},
	})
	st.Feed(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    0,
					"function": map[string]any{"arguments": `"天气"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	})
	events := st.Finish()

	var partial string
	var stopReason string
	for _, ev := range events {
		if ev.Name == "content_block_delta" {
			d := ev.Data["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				partial = d["partial_json"].(string)
			}
		}
		if ev.Name == "message_delta" {
			d := ev.Data["delta"].(map[string]any)
			stopReason = d["stop_reason"].(string)
		}
	}
	if partial != `{"q":"天气"}` {
		t.Errorf("参数拼接 = %q", partial)
	}
	if stopReason != "tool_use" {
		t.Errorf("stop_reason = %q，期望 tool_use", stopReason)
	}
}

// TestResponsesToChatMessage 基础 input 翻译。
func TestResponsesToChatMessage(t *testing.T) {
	out, err := ResponsesToChat(map[string]any{
		"model":        "m",
		"instructions": "你是助手",
		"input":        "你好",
	})
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息，实际 %d", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Error("instructions 应变成 system")
	}
	if msgs[1].(map[string]any)["content"] != "你好" {
		t.Error("input 字符串应变成 user 消息")
	}
}

// TestResponsesToChatFunctionCall function_call / output 条目翻译。
func TestResponsesToChatFunctionCall(t *testing.T) {
	out, err := ResponsesToChat(map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "查天气"}}},
			map[string]any{"type": "function_call", "call_id": "call_1",
				"name": "get_weather", "arguments": `{"city":"北京"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1",
				"output": "晴"},
		},
	})
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("期望 3 条消息，实际 %d", len(msgs))
	}
	assistant := msgs[1].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "call_1" {
		t.Errorf("function_call 未正确翻译: %v", assistant)
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Errorf("function_call_output 未正确翻译: %v", tool)
	}
}

// TestResponsesToChatEmptyInput 空 input 必须报错而非产生空请求。
func TestResponsesToChatEmptyInput(t *testing.T) {
	if _, err := ResponsesToChat(map[string]any{"model": "m"}); err == nil {
		t.Error("空 input 应返回错误")
	}
}

// TestChatToResponse 非流式响应翻译。
func TestChatToResponse(t *testing.T) {
	obj := map[string]any{
		"id": "chatcmpl-1", "model": "m", "created": 1700000000,
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "答"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 7},
	}
	out := ChatToResponse(obj, "m")
	if out["object"] != "response" || out["status"] != "completed" {
		t.Errorf("响应对象形态不对: %v", out)
	}
	output := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("期望 1 个 output 条目，实际 %d", len(output))
	}
	msg := output[0].(map[string]any)
	if msg["type"] != "message" {
		t.Errorf("output 类型 = %v", msg["type"])
	}
	content := msg["content"].([]any)[0].(map[string]any)
	if content["text"] != "答" {
		t.Errorf("文本丢失: %v", content)
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 5 || usage["output_tokens"] != 7 {
		t.Errorf("usage 未映射: %v", usage)
	}
	if usage["total_tokens"] != 12 {
		t.Errorf("total_tokens = %v，期望 12", usage["total_tokens"])
	}
}

// TestResponsesStreamerSequence 流式事件序列完整（created → delta → completed）。
func TestResponsesStreamerSequence(t *testing.T) {
	s := NewResponsesStreamer("m", "resp_1")
	var names []string
	for _, ev := range s.Start() {
		names = append(names, ev.Name)
	}
	for _, ev := range s.Feed(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": "你好"}}},
	}) {
		names = append(names, ev.Name)
	}
	for _, ev := range s.Feed(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}},
	}) {
		names = append(names, ev.Name)
	}
	for _, ev := range s.Finish() {
		names = append(names, ev.Name)
	}

	joined := strings.Join(names, ",")
	for _, want := range []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done",
		"response.completed",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少事件 %s，实际序列: %s", want, joined)
		}
	}
	// sequence_number 必须单调递增
	seq := -1
	for _, ev := range s.Finish() {
		if n, ok := ev.Data["sequence_number"].(int); ok {
			if n <= seq {
				t.Errorf("sequence_number 未递增: %d after %d", n, seq)
			}
			seq = n
		}
	}
}
