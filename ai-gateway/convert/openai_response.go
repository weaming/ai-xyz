package convert

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/weaming/ai-xyz/ai-gateway/ir"
)

// ResponsesResponseToChat 将 Responses 响应投影为 Chat Completion 响应。
func convertResponsesResponseToChat(input []byte) ([]byte, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(input, &source); err != nil {
		return nil, fmt.Errorf("解析 Responses 响应: %w", err)
	}
	response := responseMapToChat(source)
	return json.Marshal(response)
}

// ChatResponseToResponses 将 Chat Completion 响应转换为 Responses 响应。
func convertChatResponseToResponses(input []byte) ([]byte, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(input, &source); err != nil {
		return nil, fmt.Errorf("解析 Chat 响应: %w", err)
	}
	response := chatMapToResponses(source)
	return json.Marshal(response)
}

func responseMapToChat(source map[string]json.RawMessage) map[string]any {
	message := map[string]any{"role": "assistant"}
	content := ""
	refusal := ""
	toolCalls := []any{}
	finish := "stop"
	for _, rawItem := range rawList(source["output"]) {
		typeName := stringValue(rawItem["type"])
		switch typeName {
		case "message":
			for _, part := range rawList(rawItem["content"]) {
				switch stringValue(part["type"]) {
				case "output_text":
					content += stringValue(part["text"])
				case "refusal":
					refusal += stringValue(part["refusal"])
				}
			}
		case "function_call", "custom_tool_call":
			toolCalls = append(toolCalls, map[string]any{"id": stringValue(rawItem["call_id"]), "type": "function", "function": map[string]any{"name": stringValue(rawItem["name"]), "arguments": stringValue(rawItem["arguments"])}})
			finish = "tool_calls"
		}
	}
	if content != "" {
		message["content"] = content
	} else {
		message["content"] = nil
	}
	if refusal != "" {
		message["refusal"] = refusal
		finish = "refusal"
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	choice := map[string]any{"index": 0, "message": message, "finish_reason": finish}
	if stringValue(source["status"]) == "incomplete" {
		if details := objectMap(source["incomplete_details"]); stringValue(details["reason"]) == "max_output_tokens" {
			choice["finish_reason"] = "length"
		}
	}
	result := map[string]any{"id": stringValue(source["id"]), "object": "chat.completion", "created": intValue(source["created_at"]), "model": stringValue(source["model"]), "choices": []any{choice}}
	if usage := usageFromResponses(source["usage"]); usage != nil {
		result["usage"] = chatUsage(usage)
	}
	return result
}

func chatMapToResponses(source map[string]json.RawMessage) map[string]any {
	choices := rawList(source["choices"])
	output := []any{}
	for _, choice := range choices {
		message := objectMap(choice["message"])
		content := []any{}
		if text := stringValue(message["content"]); text != "" {
			content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
		}
		if refusal := stringValue(message["refusal"]); refusal != "" {
			content = append(content, map[string]any{"type": "refusal", "refusal": refusal})
		}
		if len(content) > 0 {
			output = append(output, map[string]any{"id": "msg_" + stringValue(source["id"]), "type": "message", "role": "assistant", "content": content, "status": "completed"})
		}
		for _, toolCall := range rawList(message["tool_calls"]) {
			function := objectMap(toolCall["function"])
			output = append(output, map[string]any{"id": stringValue(toolCall["id"]), "type": "function_call", "call_id": stringValue(toolCall["id"]), "name": stringValue(function["name"]), "arguments": stringValue(function["arguments"]), "status": "completed"})
		}
	}
	result := map[string]any{"id": "resp_" + stringValue(source["id"]), "object": "response", "created_at": intValue(source["created"]), "model": stringValue(source["model"]), "status": "completed", "output": output}
	if usage := usageFromChat(source["usage"]); usage != nil {
		result["usage"] = responsesUsage(usage)
	}
	return result
}

func responseContentToChat(raw json.RawMessage) string {
	if text := stringValue(raw); text != "" {
		return text
	}
	parts := rawList(raw)
	var builder strings.Builder
	for _, part := range parts {
		if stringValue(part["type"]) == "output_text" || stringValue(part["type"]) == "input_text" {
			builder.WriteString(stringValue(part["text"]))
		}
	}
	return builder.String()
}

func contentTextValue(raw json.RawMessage) string {
	if text := stringValue(raw); text != "" {
		return text
	}
	return contentTextRaw(rawList(raw))
}

func usageFromResponses(raw json.RawMessage) *ir.Usage {
	object := objectMap(raw)
	if len(object) == 0 {
		return nil
	}
	return &ir.Usage{InputTokens: int(intValue(object["input_tokens"])), OutputTokens: int(intValue(object["output_tokens"])), TotalTokens: int(intValue(object["total_tokens"])), ReasoningTokens: int(intValue(object["reasoning_tokens"])), CachedInputTokens: int(intValue(object["cached_tokens"]))}
}

func usageFromChat(raw json.RawMessage) *ir.Usage {
	object := objectMap(raw)
	if len(object) == 0 {
		return nil
	}
	details := objectMap(object["prompt_tokens_details"])
	completion := objectMap(object["completion_tokens_details"])
	return &ir.Usage{InputTokens: int(intValue(object["prompt_tokens"])), OutputTokens: int(intValue(object["completion_tokens"])), TotalTokens: int(intValue(object["total_tokens"])), ReasoningTokens: int(intValue(completion["reasoning_tokens"])), CachedInputTokens: int(intValue(details["cached_tokens"]))}
}

func responsesUsage(usage *ir.Usage) map[string]any {
	return map[string]any{"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens, "total_tokens": usage.TotalTokens, "output_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens}, "input_tokens_details": map[string]any{"cached_tokens": usage.CachedInputTokens}}
}

func chatUsage(usage *ir.Usage) map[string]any {
	return map[string]any{"prompt_tokens": usage.InputTokens, "completion_tokens": usage.OutputTokens, "total_tokens": usage.TotalTokens, "prompt_tokens_details": map[string]any{"cached_tokens": usage.CachedInputTokens}, "completion_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens}}
}
