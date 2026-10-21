// Package bridge OpenAI Responses API（Codex）↔ Chat Completions 翻译。
//
// Codex 只说 /v1/responses，上游只有 /chat/completions。这里把请求翻译过去，
// 再把结果翻译成 Responses 的 response 对象与 SSE 事件流。
package bridge

import (
	"fmt"
	"strings"

	"github.com/yuanjie-ai/loomy2api/internal/account"
)

// ResponsesToChat 把 Responses 请求翻成 Chat Completions 请求。
//
// 支持 input 为字符串或条目数组（message / function_call / function_call_output），
// 以及 instructions（→ system）、tools（扁平 function 形态）、max_output_tokens。
func ResponsesToChat(req map[string]any) (map[string]any, error) {
	messages := []any{}

	if ins := str(req["instructions"]); ins != "" {
		messages = append(messages, map[string]any{"role": "system", "content": ins})
	}

	switch input := req["input"].(type) {
	case string:
		if input != "" {
			messages = append(messages, map[string]any{"role": "user", "content": input})
		}
	case []any:
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch str(item["type"]) {
			case "message", "":
				role := str(item["role"])
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					role = "system"
				}
				content := ContentToText(item["content"])
				msg := map[string]any{"role": role, "content": content}
				if calls, ok := item["tool_calls"].([]any); ok && len(calls) > 0 {
					msg["tool_calls"] = calls
				}
				messages = append(messages, msg)
			case "function_call":
				args := str(item["arguments"])
				if args == "" {
					args = "{}"
				}
				messages = append(messages, map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{map[string]any{
						"id":   str(item["call_id"]),
						"type": "function",
						"function": map[string]any{
							"name":      str(item["name"]),
							"arguments": args,
						},
					}},
				})
			case "function_call_output":
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": str(item["call_id"]),
					"content":      ContentToText(item["output"]),
				})
			}
		}
	case nil:
		// 无 input：留空让上层报错
	default:
		messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprint(input)})
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("input 不能为空")
	}

	out := map[string]any{
		"model":    req["model"],
		"messages": messages,
		"stream":   truthy(req["stream"]),
	}
	if v, ok := req["max_output_tokens"]; ok {
		out["max_tokens"] = v
	} else if v, ok := req["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := req["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := req["top_p"]; ok {
		out["top_p"] = v
	}
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, rawTool := range tools {
			t, ok := rawTool.(map[string]any)
			if !ok {
				continue
			}
			// Responses 的工具是扁平形态 {type:"function", name, parameters}；
			// 也兼容已嵌套的 {type:"function", function:{...}}。
			name := str(t["name"])
			params := t["parameters"]
			desc := str(t["description"])
			if fn, ok := t["function"].(map[string]any); ok {
				name = str(fn["name"])
				params = fn["parameters"]
				desc = str(fn["description"])
			}
			if name == "" {
				continue
			}
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			converted = append(converted, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": name, "description": desc, "parameters": params,
				},
			})
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}
	if v, ok := req["tool_choice"]; ok {
		out["tool_choice"] = v
	}
	if v, ok := req["parallel_tool_calls"]; ok {
		out["parallel_tool_calls"] = v
	}
	return out, nil
}

// ContentToText 把 Responses 的 content（字符串或条目数组）压成纯文本。
func ContentToText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, raw := range c {
			switch item := raw.(type) {
			case string:
				parts = append(parts, item)
			case map[string]any:
				switch str(item["type"]) {
				case "input_text", "output_text", "text", "":
					if t := str(item["text"]); t != "" {
						parts = append(parts, t)
					}
				case "input_image":
					parts = append(parts, "[image]")
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return fmt.Sprint(content)
}

// ChatToResponse 把非流式 Chat Completion 翻成 Responses 的 response 对象。
func ChatToResponse(obj map[string]any, model string) map[string]any {
	choices, _ := obj["choices"].([]any)
	var choice map[string]any
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	message, _ := choice["message"].(map[string]any)

	output := []any{}
	if r := str(message["reasoning_content"]); r != "" {
		output = append(output, map[string]any{
			"type":    "reasoning",
			"id":      "rs_" + account.RandomHex(8),
			"summary": []any{map[string]any{"type": "summary_text", "text": r}},
		})
	}
	if t := str(message["content"]); t != "" {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     "msg_" + account.RandomHex(8),
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": t, "annotations": []any{},
			}},
		})
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			args := str(fn["arguments"])
			if args == "" {
				args = "{}"
			}
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        "fc_" + account.RandomHex(8),
				"call_id":   str(call["id"]),
				"name":      str(fn["name"]),
				"arguments": args,
				"status":    "completed",
			})
		}
	}

	usage, _ := obj["usage"].(map[string]any)
	respUsage := map[string]any{
		"input_tokens":  intOf(usage["prompt_tokens"]),
		"output_tokens": intOf(usage["completion_tokens"]),
		"total_tokens":  intOf(usage["prompt_tokens"]) + intOf(usage["completion_tokens"]),
	}
	if d, ok := usage["completion_tokens_details"].(map[string]any); ok {
		respUsage["output_tokens_details"] = map[string]any{
			"reasoning_tokens": intOf(d["reasoning_tokens"]),
		}
	}
	if v, ok := usage["points_consumed"]; ok {
		respUsage["points_consumed"] = v
	}

	name := model
	if m := str(obj["model"]); m != "" {
		name = m
	}
	status := "completed"
	if str(choice["finish_reason"]) == "length" {
		status = "incomplete"
	}
	return map[string]any{
		"id":                 orDefault(str(obj["id"]), "resp_"+account.RandomHex(10)),
		"object":             "response",
		"created_at":         timeOf(obj["created"]),
		"status":             status,
		"model":              name,
		"output":             output,
		"usage":              respUsage,
		"error":              nil,
		"incomplete_details": nil,
	}
}

// ResponsesStreamer 把 chat 增量翻译成 Responses SSE 事件。
type ResponsesStreamer struct {
	Model      string
	ResponseID string
	seq        int

	reasoningID string
	messageID   string
	toolIDs     []string

	textOpen  bool
	thinkOpen bool

	fullText strings.Builder
	tools    map[int]*toolAccum
	order    []int

	usage map[string]any
}

// NewResponsesStreamer 构造翻译器。
func NewResponsesStreamer(model, responseID string) *ResponsesStreamer {
	if responseID == "" {
		responseID = "resp_" + account.RandomHex(10)
	}
	return &ResponsesStreamer{
		Model: model, ResponseID: responseID,
		usage: map[string]any{}, tools: map[int]*toolAccum{},
	}
}

// SSEEvent 一个 Responses SSE 事件（event 名 + JSON 数据）。
type SSEEvent struct {
	Name string
	Data map[string]any
}

func (s *ResponsesStreamer) next() int { s.seq++; return s.seq }

// Start 发出 response.created 与 response.in_progress。
func (s *ResponsesStreamer) Start() []SSEEvent {
	base := s.envelope("in_progress", []any{})
	return []SSEEvent{
		{Name: "response.created", Data: map[string]any{
			"type": "response.created", "sequence_number": s.next(), "response": base,
		}},
		{Name: "response.in_progress", Data: map[string]any{
			"type": "response.in_progress", "sequence_number": s.next(), "response": base,
		}},
	}
}

func (s *ResponsesStreamer) envelope(status string, output []any) map[string]any {
	usage := map[string]any{
		"input_tokens":  intOf(s.usage["prompt_tokens"]),
		"output_tokens": intOf(s.usage["completion_tokens"]),
	}
	return map[string]any{
		"id": s.ResponseID, "object": "response", "status": status,
		"model": s.Model, "output": output, "usage": usage,
		"created_at": timeNow(),
	}
}

// Feed 吃一个 chat 增量。
func (s *ResponsesStreamer) Feed(obj map[string]any) []SSEEvent {
	var out []SSEEvent
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
	if delta == nil {
		return out
	}

	if think := str(delta["reasoning_content"]); think != "" {
		if !s.thinkOpen {
			s.thinkOpen = true
			s.reasoningID = "rs_" + account.RandomHex(8)
			out = append(out, SSEEvent{Name: "response.output_item.added", Data: map[string]any{
				"type": "response.output_item.added", "sequence_number": s.next(),
				"output_index": 0,
				"item": map[string]any{
					"type": "reasoning", "id": s.reasoningID, "summary": []any{},
				},
			}})
			out = append(out, SSEEvent{Name: "response.reasoning_summary_part.added", Data: map[string]any{
				"type": "response.reasoning_summary_part.added", "sequence_number": s.next(),
				"item_id": s.reasoningID, "output_index": 0, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": ""},
			}})
		}
		out = append(out, SSEEvent{Name: "response.reasoning_summary_text.delta", Data: map[string]any{
			"type": "response.reasoning_summary_text.delta", "sequence_number": s.next(),
			"item_id": s.reasoningID, "output_index": 0, "summary_index": 0,
			"delta": think,
		}})
	}

	if text := str(delta["content"]); text != "" {
		if s.messageID == "" {
			s.messageID = "msg_" + account.RandomHex(8)
			s.textOpen = true
			out = append(out, SSEEvent{Name: "response.output_item.added", Data: map[string]any{
				"type": "response.output_item.added", "sequence_number": s.next(),
				"output_index": s.outputIndex(),
				"item": map[string]any{
					"type": "message", "id": s.messageID, "role": "assistant",
					"status": "in_progress", "content": []any{},
				},
			}})
			out = append(out, SSEEvent{Name: "response.content_part.added", Data: map[string]any{
				"type": "response.content_part.added", "sequence_number": s.next(),
				"item_id": s.messageID, "output_index": s.outputIndex(), "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			}})
		}
		s.fullText.WriteString(text)
		out = append(out, SSEEvent{Name: "response.output_text.delta", Data: map[string]any{
			"type": "response.output_text.delta", "sequence_number": s.next(),
			"item_id": s.messageID, "output_index": s.outputIndex(), "content_index": 0,
			"delta": text,
		}})
	}

	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			idx := intOf(call["index"])
			acc, exists := s.tools[idx]
			if !exists {
				acc = &toolAccum{}
				s.tools[idx] = acc
				s.order = append(s.order, idx)
			}
			fn, _ := call["function"].(map[string]any)
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
	return out
}

func (s *ResponsesStreamer) outputIndex() int {
	n := 0
	if s.reasoningID != "" {
		n++
	}
	return n
}

// Usage 累积到的 usage（供记账）。
func (s *ResponsesStreamer) Usage() map[string]any { return s.usage }

// Finish 收尾：发 output_item.done 与 response.completed。
func (s *ResponsesStreamer) Finish() []SSEEvent {
	var out []SSEEvent

	if s.reasoningID != "" {
		out = append(out, SSEEvent{Name: "response.reasoning_summary_text.done", Data: map[string]any{
			"type": "response.reasoning_summary_text.done", "sequence_number": s.next(),
			"item_id": s.reasoningID, "output_index": 0, "summary_index": 0,
			"text": "",
		}})
		out = append(out, SSEEvent{Name: "response.output_item.done", Data: map[string]any{
			"type": "response.output_item.done", "sequence_number": s.next(),
			"output_index": 0,
			"item": map[string]any{
				"type": "reasoning", "id": s.reasoningID,
				"summary": []any{map[string]any{"type": "summary_text", "text": ""}},
			},
		}})
	}

	finalOutput := []any{}
	if s.reasoningID != "" {
		finalOutput = append(finalOutput, map[string]any{
			"type": "reasoning", "id": s.reasoningID, "summary": []any{},
		})
	}

	if s.messageID != "" {
		text := s.fullText.String()
		out = append(out, SSEEvent{Name: "response.output_text.done", Data: map[string]any{
			"type": "response.output_text.done", "sequence_number": s.next(),
			"item_id": s.messageID, "output_index": s.outputIndex(), "content_index": 0,
			"text": text,
		}})
		out = append(out, SSEEvent{Name: "response.content_part.done", Data: map[string]any{
			"type": "response.content_part.done", "sequence_number": s.next(),
			"item_id": s.messageID, "output_index": s.outputIndex(), "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		}})
		out = append(out, SSEEvent{Name: "response.output_item.done", Data: map[string]any{
			"type": "response.output_item.done", "sequence_number": s.next(),
			"output_index": s.outputIndex(),
			"item": map[string]any{
				"type": "message", "id": s.messageID, "role": "assistant",
				"status": "completed",
				"content": []any{map[string]any{
					"type": "output_text", "text": text, "annotations": []any{},
				}},
			},
		}})
		finalOutput = append(finalOutput, map[string]any{
			"type": "message", "id": s.messageID, "role": "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": text, "annotations": []any{},
			}},
		})
	}

	for _, idx := range s.order {
		acc := s.tools[idx]
		if acc == nil || acc.Name == "" {
			continue
		}
		id := "fc_" + account.RandomHex(8)
		callID := acc.ID
		if callID == "" {
			callID = "call_" + account.RandomHex(6)
		}
		args := acc.Args.String()
		if args == "" {
			args = "{}"
		}
		item := map[string]any{
			"type": "function_call", "id": id, "call_id": callID,
			"name": acc.Name, "arguments": args, "status": "completed",
		}
		out = append(out,
			SSEEvent{Name: "response.output_item.added", Data: map[string]any{
				"type": "response.output_item.added", "sequence_number": s.next(),
				"output_index": len(finalOutput), "item": item,
			}},
			SSEEvent{Name: "response.output_item.done", Data: map[string]any{
				"type": "response.output_item.done", "sequence_number": s.next(),
				"output_index": len(finalOutput), "item": item,
			}},
		)
		finalOutput = append(finalOutput, item)
	}

	out = append(out, SSEEvent{Name: "response.completed", Data: map[string]any{
		"type": "response.completed", "sequence_number": s.next(),
		"response": s.envelope("completed", finalOutput),
	}})
	return out
}
