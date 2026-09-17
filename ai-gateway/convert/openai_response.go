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
	response, err := responseMapToChat(source)
	if err != nil {
		return nil, err
	}
	return json.Marshal(response)
}

// ChatResponseToResponses 将 Chat Completion 响应转换为 Responses 响应。
func convertChatResponseToResponses(input []byte) ([]byte, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(input, &source); err != nil {
		return nil, fmt.Errorf("解析 Chat 响应: %w", err)
	}
	response, err := chatMapToResponses(source)
	if err != nil {
		return nil, err
	}
	return json.Marshal(response)
}

func responseMapToChat(source map[string]json.RawMessage) (map[string]any, error) {
	message := map[string]any{"role": "assistant"}
	content := ""
	refusal := ""
	reasoning := ""
	toolCalls := []any{}
	finish := responseFinishReason(source)
	for _, rawItem := range rawList(source["output"]) {
		typeName := stringValue(rawItem["type"])
		switch typeName {
		case "message":
			for _, part := range rawList(rawItem["content"]) {
				switch stringValue(part["type"]) {
				case "output_text":
					if len(rawList(part["annotations"])) > 0 {
						return nil, fmt.Errorf("Chat Completions 无法表达 Responses output_text.annotations")
					}
					content += stringValue(part["text"])
				case "refusal":
					refusal += stringValue(part["refusal"])
				default:
					return nil, fmt.Errorf("Chat Completions 无法表达 Responses message content type %q", stringValue(part["type"]))
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{"id": stringValue(rawItem["call_id"]), "type": "function", "function": map[string]any{"name": stringValue(rawItem["name"]), "arguments": stringValue(rawItem["arguments"])}})
			finish = "tool_calls"
		case "custom_tool_call":
			toolCalls = append(toolCalls, map[string]any{"id": stringValue(rawItem["call_id"]), "type": "custom", "custom": map[string]any{"name": stringValue(rawItem["name"]), "input": rawToAny(rawItem["input"])}})
			finish = "tool_calls"
		case "reasoning":
			if encrypted := stringValue(rawItem["encrypted_content"]); encrypted != "" {
				return nil, fmt.Errorf("Chat Completions 无法表达加密 reasoning")
			}
			reasoning += responseReasoningText(rawItem)
		case "web_search_call", "file_search_call", "code_interpreter_call", "computer_call", "computer_call_output", "image_generation_call", "mcp_call", "mcp_call_output":
			return nil, fmt.Errorf("Chat Completions 暂不支持 Responses item type %q", typeName)
		case "":
			return nil, fmt.Errorf("Responses output item 缺少 type")
		default:
			return nil, fmt.Errorf("Chat Completions 无法表达 Responses item type %q", typeName)
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
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	choice := map[string]any{"index": 0, "message": message, "finish_reason": finish}
	if finish == "" {
		finish = "stop"
	}
	choice["finish_reason"] = finish
	result := map[string]any{"id": stringValue(source["id"]), "object": "chat.completion", "created": intValue(source["created_at"]), "model": stringValue(source["model"]), "choices": []any{choice}}
	if usage := usageFromResponses(source["usage"]); usage != nil {
		result["usage"] = chatUsage(usage)
	}
	return result, nil
}

func chatMapToResponses(source map[string]json.RawMessage) (map[string]any, error) {
	choices := rawList(source["choices"])
	output := []any{}
	status := "completed"
	var incompleteDetails map[string]any
	for _, choice := range choices {
		if finish := stringValue(choice["finish_reason"]); finish != "" {
			status, incompleteDetails = responsesStatusFromChatFinish(finish)
		}
		message := objectMap(choice["message"])
		content, err := chatResponseContentToResponses(message["content"])
		if err != nil {
			return nil, err
		}
		if refusal := stringValue(message["refusal"]); refusal != "" {
			content = append(content, map[string]any{"type": "refusal", "refusal": refusal})
		}
		if reasoning := stringValue(message["reasoning_content"]); reasoning != "" {
			output = append(output, map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": reasoning}}, "status": "completed"})
		}
		if len(content) > 0 {
			output = append(output, map[string]any{"id": "msg_" + stringValue(source["id"]), "type": "message", "role": "assistant", "content": content, "status": status})
		}
		for _, toolCall := range rawList(message["tool_calls"]) {
			switch stringValue(toolCall["type"]) {
			case "", "function":
				function := objectMap(toolCall["function"])
				output = append(output, map[string]any{"id": stringValue(toolCall["id"]), "type": "function_call", "call_id": stringValue(toolCall["id"]), "name": stringValue(function["name"]), "arguments": stringValue(function["arguments"]), "status": status})
			case "custom":
				custom := objectMap(toolCall["custom"])
				output = append(output, map[string]any{"id": stringValue(toolCall["id"]), "type": "custom_tool_call", "call_id": stringValue(toolCall["id"]), "name": stringValue(custom["name"]), "input": rawToAny(custom["input"]), "status": status})
			default:
				return nil, fmt.Errorf("Chat tool_call.type %q 无法转换为 Responses", stringValue(toolCall["type"]))
			}
		}
	}
	result := map[string]any{"id": "resp_" + stringValue(source["id"]), "object": "response", "created_at": intValue(source["created"]), "model": stringValue(source["model"]), "status": status, "output": output}
	if incompleteDetails != nil {
		result["incomplete_details"] = incompleteDetails
	}
	if usage := usageFromChat(source["usage"]); usage != nil {
		result["usage"] = responsesUsage(usage)
	}
	return result, nil
}

func responseMessageToChat(item map[string]json.RawMessage) (map[string]any, error) {
	content, err := responseContentToChat(item["content"])
	if err != nil {
		return nil, err
	}
	role := stringValue(item["role"])
	if role == "" {
		role = "user"
	}
	return map[string]any{"role": role, "content": content}, nil
}

func responseContentToChat(raw json.RawMessage) (any, error) {
	if text := stringValue(raw); text != "" {
		return text, nil
	}
	parts := rawList(raw)
	chatParts := make([]any, 0, len(parts))
	textOnly := true
	for _, part := range parts {
		switch stringValue(part["type"]) {
		case "output_text", "input_text", "text":
			chatParts = append(chatParts, map[string]any{"type": "text", "text": stringValue(part["text"])})
		case "input_image":
			url := stringValue(part["image_url"])
			if url == "" || stringValue(part["file_id"]) != "" {
				return nil, fmt.Errorf("Chat Completions 图片输入必须使用 image_url")
			}
			imageURL := map[string]any{"url": url}
			if detail := stringValue(part["detail"]); detail != "" {
				imageURL["detail"] = detail
			}
			chatParts = append(chatParts, map[string]any{"type": "image_url", "image_url": imageURL})
			textOnly = false
		case "input_file":
			file, err := responseFileToChat(part)
			if err != nil {
				return nil, err
			}
			chatParts = append(chatParts, file)
			textOnly = false
		case "input_audio":
			audio := rawToAny(part["input_audio"])
			if audio == nil {
				return nil, fmt.Errorf("Responses input_audio 缺少 input_audio 内容")
			}
			chatParts = append(chatParts, map[string]any{"type": "input_audio", "input_audio": audio})
			textOnly = false
		default:
			return nil, fmt.Errorf("Chat Completions 无法表达 Responses content type %q", stringValue(part["type"]))
		}
	}
	if textOnly {
		var builder strings.Builder
		for _, part := range chatParts {
			builder.WriteString(stringValueAny(part.(map[string]any)["text"]))
		}
		return builder.String(), nil
	}
	return chatParts, nil
}

func responseFileToChat(part map[string]json.RawMessage) (map[string]any, error) {
	if hasJSONValue(part["detail"]) {
		return nil, fmt.Errorf("Chat Completions file content 不支持 Responses input_file.detail")
	}
	file := map[string]any{}
	if fileID := stringValue(part["file_id"]); fileID != "" {
		file["file_id"] = fileID
	}
	if fileData := stringValue(part["file_data"]); fileData != "" {
		file["file_data"] = fileData
	}
	if filename := stringValue(part["filename"]); filename != "" {
		file["filename"] = filename
	}
	if stringValue(part["file_url"]) != "" {
		return nil, fmt.Errorf("Chat Completions 不支持 Responses input_file.file_url")
	}
	if len(file) == 0 {
		return nil, fmt.Errorf("Responses input_file 缺少 file_id 或 file_data")
	}
	return map[string]any{"type": "file", "file": file}, nil
}

func responseToolOutputToChat(raw json.RawMessage) (any, error) {
	if text := stringValue(raw); text != "" {
		return text, nil
	}
	return responseContentToChat(raw)
}

func chatResponseContentToResponses(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if text := stringValue(raw); text != "" {
		return []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}, nil
	}
	parts := rawList(raw)
	result := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch stringValue(part["type"]) {
		case "text", "output_text":
			result = append(result, map[string]any{"type": "output_text", "text": stringValue(part["text"]), "annotations": []any{}})
		case "refusal":
			result = append(result, map[string]any{"type": "refusal", "refusal": stringValue(part["refusal"])})
		default:
			return nil, fmt.Errorf("Responses 不支持 Chat 响应 content type %q", stringValue(part["type"]))
		}
	}
	return result, nil
}

func responseReasoningText(item map[string]json.RawMessage) string {
	var builder strings.Builder
	for _, part := range rawList(item["summary"]) {
		if stringValue(part["type"]) == "summary_text" {
			builder.WriteString(stringValue(part["text"]))
		}
	}
	for _, part := range rawList(item["content"]) {
		if stringValue(part["type"]) == "reasoning_text" {
			builder.WriteString(stringValue(part["text"]))
		}
	}
	return builder.String()
}

func usageFromResponses(raw json.RawMessage) *ir.Usage {
	object := objectMap(raw)
	if len(object) == 0 {
		return nil
	}
	inputDetails := objectMap(object["input_tokens_details"])
	outputDetails := objectMap(object["output_tokens_details"])
	reasoningTokens := intValue(outputDetails["reasoning_tokens"])
	if reasoningTokens == 0 {
		reasoningTokens = intValue(object["reasoning_tokens"])
	}
	cachedTokens := intValue(inputDetails["cached_tokens"])
	if cachedTokens == 0 {
		cachedTokens = intValue(object["cached_tokens"])
	}
	return &ir.Usage{InputTokens: int(intValue(object["input_tokens"])), OutputTokens: int(intValue(object["output_tokens"])), TotalTokens: int(intValue(object["total_tokens"])), ReasoningTokens: int(reasoningTokens), CachedInputTokens: int(cachedTokens)}
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

func responseFinishReason(source map[string]json.RawMessage) string {
	switch stringValue(source["status"]) {
	case "failed":
		return "error"
	case "cancelled", "queued", "in_progress":
		return "error"
	case "incomplete":
		details := objectMap(source["incomplete_details"])
		switch stringValue(details["reason"]) {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		default:
			return "error"
		}
	case "", "completed":
		return "stop"
	default:
		return "error"
	}
}

func responsesStatusFromChatFinish(finish string) (string, map[string]any) {
	switch finish {
	case "length":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	case "error":
		return "failed", nil
	default:
		return "completed", nil
	}
}
