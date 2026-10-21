// Package bridge Anthropic Messages API ↔ OpenAI Chat Completions 双向翻译。
//
// Claude Code 等客户端只说 /v1/messages，而上游只有 OpenAI 形态的
// /chat/completions。这里把请求翻译过去、把响应（非流式与 SSE）翻译回来，
// 工具调用与思考链（reasoning_content）都不丢。
package bridge

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yuanjie-ai/loomy2api/internal/account"
)

// BlocksToText 把 Anthropic 的 content（字符串或块数组）压成纯文本。
func BlocksToText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, block := range c {
			switch b := block.(type) {
			case string:
				parts = append(parts, b)
			case map[string]any:
				switch str(b["type"]) {
				case "text":
					parts = append(parts, str(b["text"]))
				case "tool_result":
					parts = append(parts, BlocksToText(b["content"]))
				}
			}
		}
		var out []string
		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, "\n")
	}
	return fmt.Sprint(content)
}

// ToOpenAI 把 Anthropic /v1/messages 请求翻成 Chat Completions 请求。
func ToOpenAI(req map[string]any) map[string]any {
	messages := []any{}

	if sys := req["system"]; sys != nil {
		if text := BlocksToText(sys); text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}

	msgs, _ := req["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := str(m["role"])
		content := m["content"]
		if s, ok := content.(string); ok {
			messages = append(messages, map[string]any{"role": role, "content": s})
			continue
		}
		blocks, _ := content.([]any)
		if blocks == nil {
			messages = append(messages, map[string]any{"role": role, "content": ""})
			continue
		}

		var texts []string
		var toolCalls []any
		var toolMsgs []any
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			switch str(block["type"]) {
			case "text":
				if t := str(block["text"]); t != "" {
					texts = append(texts, t)
				}
			case "tool_use":
				id := str(block["id"])
				if id == "" {
					id = "call_" + account.RandomHex(6)
				}
				args, _ := json.Marshal(block["input"])
				if block["input"] == nil {
					args = []byte("{}")
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      str(block["name"]),
						"arguments": string(args),
					},
				})
			case "tool_result":
				toolMsgs = append(toolMsgs, map[string]any{
					"role":         "tool",
					"tool_call_id": str(block["tool_use_id"]),
					"content":      BlocksToText(block["content"]),
				})
			}
		}
		// tool_result 块出现在 user 消息里，翻译成独立的 tool 消息，
		// 且不产生空的 user 脚手架。
		if len(texts) > 0 || len(toolCalls) > 0 {
			out := map[string]any{"role": role}
			if len(texts) > 0 {
				out["content"] = strings.Join(texts, "\n")
			} else {
				out["content"] = nil
			}
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
			}
			messages = append(messages, out)
		}
		messages = append(messages, toolMsgs...)
	}

	out := map[string]any{
		"model":    req["model"],
		"messages": messages,
		"stream":   truthy(req["stream"]),
	}
	if v, ok := req["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := req["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := req["top_p"]; ok {
		out["top_p"] = v
	}
	if seq, ok := req["stop_sequences"].([]any); ok && len(seq) > 0 {
		out["stop"] = seq
	}

	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, rawTool := range tools {
			t, ok := rawTool.(map[string]any)
			if !ok || str(t["name"]) == "" {
				continue
			}
			schema := t["input_schema"]
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			converted = append(converted, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        str(t["name"]),
					"description": str(t["description"]),
					"parameters":  schema,
				},
			})
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}

	if choice, ok := req["tool_choice"].(map[string]any); ok {
		switch str(choice["type"]) {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			out["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": str(choice["name"])},
			}
		}
	}
	return out
}

// ToAnthropic 把非流式 Chat Completion 翻成 Anthropic message。
func ToAnthropic(obj map[string]any, model string) map[string]any {
	choices, _ := obj["choices"].([]any)
	var choice map[string]any
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	message, _ := choice["message"].(map[string]any)

	content := []any{}
	if r := str(message["reasoning_content"]); r != "" {
		content = append(content, map[string]any{
			"type": "thinking", "thinking": r, "signature": "",
		})
	}
	if t := str(message["content"]); t != "" {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			var input any = map[string]any{}
			if s := str(fn["arguments"]); s != "" {
				_ = json.Unmarshal([]byte(s), &input)
			}
			id := str(call["id"])
			if id == "" {
				id = "call_" + account.RandomHex(6)
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": id,
				"name": str(fn["name"]), "input": input,
			})
		}
	}

	usage, _ := obj["usage"].(map[string]any)
	out := map[string]any{
		"id":            orDefault(str(obj["id"]), "msg_"+account.RandomHex(12)),
		"type":          "message",
		"role":          "assistant",
		"model":         orDefault(model, str(obj["model"])),
		"content":       content,
		"stop_reason":   StopReason(str(choice["finish_reason"])),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  intOf(usage["prompt_tokens"]),
			"output_tokens": intOf(usage["completion_tokens"]),
		},
	}
	// 上游的积分扣费透出到响应里，客户端可观测成本。
	if pts, ok := usage["points_consumed"]; ok {
		out["usage"].(map[string]any)["points_consumed"] = pts
	}
	return out
}

// StopReason 把 OpenAI finish_reason 映射为 Anthropic stop_reason。
func StopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

// StreamTranslator 把 OpenAI SSE 增量翻成 Anthropic SSE 事件。
//
// 用法：
//
//	st := NewStreamTranslator(model, session)
//	for _, ev := range st.Start() { emit(ev) }
//	for _, chunk := range openaiChunks { for _, ev := range st.Feed(chunk) { emit(ev) } }
//	for _, ev := range st.Finish() { emit(ev) }
type StreamTranslator struct {
	Model     string
	MessageID string

	usage        map[string]any
	finishReason string

	textOpen  bool
	thinkOpen bool
	blockIdx  int

	// 工具调用按 index 累积（上游常把参数分多帧发）。
	tools     map[int]*toolAccum
	toolOrder []int
	toolsSent bool
}

type toolAccum struct {
	ID   string
	Name string
	Args strings.Builder
}

// NewStreamTranslator 构造翻译器。
func NewStreamTranslator(model, messageID string) *StreamTranslator {
	if messageID == "" {
		messageID = "msg_" + account.RandomHex(12)
	}
	return &StreamTranslator{
		Model:     model,
		MessageID: messageID,
		usage:     map[string]any{},
		tools:     map[int]*toolAccum{},
	}
}

// Event Anthropic SSE 事件。
type Event struct {
	Name string
	Data map[string]any
}

// Start 发出 message_start。
func (s *StreamTranslator) Start() []Event {
	return []Event{{Name: "message_start", Data: map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.MessageID,
			"type":          "message",
			"role":          "assistant",
			"model":         s.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}}}
}

// Feed 处理一个 OpenAI SSE 增量对象，返回要发出的事件。
func (s *StreamTranslator) Feed(obj map[string]any) []Event {
	var out []Event

	if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
		for k, v := range u {
			s.usage[k] = v
		}
	}

	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return out
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)

	thinking := str(delta["reasoning_content"])
	if thinking != "" {
		if !s.thinkOpen {
			s.thinkOpen = true
			out = append(out, Event{Name: "content_block_start", Data: map[string]any{
				"type": "content_block_start", "index": s.blockIdx,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}})
		}
		out = append(out, Event{Name: "content_block_delta", Data: map[string]any{
			"type": "content_block_delta", "index": s.blockIdx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": thinking},
		}})
	}

	if text := str(delta["content"]); text != "" {
		if s.thinkOpen {
			s.thinkOpen = false
			out = append(out, Event{Name: "content_block_stop", Data: map[string]any{
				"type": "content_block_stop", "index": s.blockIdx,
			}})
			s.blockIdx++
		}
		if !s.textOpen {
			s.textOpen = true
			out = append(out, Event{Name: "content_block_start", Data: map[string]any{
				"type": "content_block_start", "index": s.blockIdx,
				"content_block": map[string]any{"type": "text", "text": ""},
			}})
		}
		out = append(out, Event{Name: "content_block_delta", Data: map[string]any{
			"type": "content_block_delta", "index": s.blockIdx,
			"delta": map[string]any{"type": "text_delta", "text": text},
		}})
	}

	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			idx := intOf(call["index"])
			fn, _ := call["function"].(map[string]any)
			acc, exists := s.tools[idx]
			if !exists {
				acc = &toolAccum{}
				s.tools[idx] = acc
				s.toolOrder = append(s.toolOrder, idx)
			}
			if id := str(call["id"]); id != "" {
				acc.ID = id
			}
			if name := str(fn["name"]); name != "" {
				acc.Name = name
			}
			if args := str(fn["arguments"]); args != "" {
				acc.Args.WriteString(args)
			}
		}
	}

	if fr := str(choice["finish_reason"]); fr != "" {
		s.finishReason = fr
	}
	return out
}

// Finish 收尾：把累积的工具调用整块发出，再发 message_delta + message_stop。
func (s *StreamTranslator) Finish() []Event {
	var out []Event

	if s.thinkOpen {
		s.thinkOpen = false
		out = append(out, Event{Name: "content_block_stop", Data: map[string]any{
			"type": "content_block_stop", "index": s.blockIdx,
		}})
		s.blockIdx++
	}
	if s.textOpen {
		s.textOpen = false
		out = append(out, Event{Name: "content_block_stop", Data: map[string]any{
			"type": "content_block_stop", "index": s.blockIdx,
		}})
		s.blockIdx++
	}

	for _, idx := range s.toolOrder {
		acc := s.tools[idx]
		if acc == nil || acc.Name == "" {
			continue
		}
		id := acc.ID
		if id == "" {
			id = "call_" + account.RandomHex(6)
		}
		out = append(out, Event{Name: "content_block_start", Data: map[string]any{
			"type": "content_block_start", "index": s.blockIdx,
			"content_block": map[string]any{
				"type": "tool_use", "id": id, "name": acc.Name, "input": map[string]any{},
			},
		}})
		partial := acc.Args.String()
		if partial == "" {
			partial = "{}"
		}
		out = append(out, Event{Name: "content_block_delta", Data: map[string]any{
			"type": "content_block_delta", "index": s.blockIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
		}})
		out = append(out, Event{Name: "content_block_stop", Data: map[string]any{
			"type": "content_block_stop", "index": s.blockIdx,
		}})
		s.blockIdx++
	}

	usage := map[string]any{"output_tokens": intOf(s.usage["completion_tokens"])}
	if v, ok := s.usage["points_consumed"]; ok {
		usage["points_consumed"] = v
	}
	out = append(out,
		Event{Name: "message_delta", Data: map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": StopReason(s.finishReason), "stop_sequence": nil},
			"usage": usage,
		}},
		Event{Name: "message_stop", Data: map[string]any{"type": "message_stop"}},
	)
	return out
}

// Usage 返回累积到的 usage（供记账）。
func (s *StreamTranslator) Usage() map[string]any { return s.usage }

// ---------------------------------------------------------------- helpers

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	}
	return ""
}

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	}
	return 0
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	}
	return false
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
