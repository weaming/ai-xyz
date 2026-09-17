package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func convertChatRequestToResponses(input []byte, defaultModel string) ([]byte, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(input, &source); err != nil {
		return nil, fmt.Errorf("解析 Chat 请求: %w", err)
	}

	target := make(map[string]json.RawMessage)
	copyKnown(source, target, "model", "temperature", "top_p", "stop", "stream", "tools", "tool_choice", "parallel_tool_calls", "store")
	copyIfPresent(source, target, "max_completion_tokens", "max_output_tokens")
	copyIfPresent(source, target, "response_format", "text")
	copyIfPresent(source, target, "reasoning_effort", "reasoning")
	if _, exists := target["model"]; !exists && defaultModel != "" {
		target["model"] = rawString(defaultModel)
	}

	messages, err := decodeRawList(source["messages"])
	if err != nil {
		return nil, fmt.Errorf("解析 Chat messages: %w", err)
	}
	inputItems, instructions, err := chatMessagesToResponses(messages)
	if err != nil {
		return nil, err
	}
	if len(instructions) > 0 {
		target["instructions"] = rawString(joinText(instructions))
	}
	target["input"] = mustJSON(inputItems)
	normalizeChatToolsToResponses(target)
	normalizeChatChoiceToResponses(target)
	normalizeChatGenerationToResponses(source, target)
	return json.Marshal(target)
}

func convertResponsesRequestToChat(input []byte, defaultModel string, mode Mode) ([]byte, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(input, &source); err != nil {
		return nil, fmt.Errorf("解析 Responses 请求: %w", err)
	}
	if raw := source["previous_response_id"]; len(raw) > 0 && string(raw) != "null" {
		if mode == ModeStrict {
			return nil, fmt.Errorf("Chat Completions 不支持 previous_response_id")
		}
	}

	target := make(map[string]json.RawMessage)
	copyKnown(source, target, "temperature", "top_p", "stop", "stream", "tools", "tool_choice", "parallel_tool_calls")
	copyIfPresent(source, target, "max_output_tokens", "max_completion_tokens")
	copyIfPresent(source, target, "text", "response_format")
	copyIfPresent(source, target, "reasoning", "reasoning_effort")
	if model, exists := source["model"]; exists {
		target["model"] = bytes.Clone(model)
	} else if defaultModel != "" {
		target["model"] = rawString(defaultModel)
	}

	instructions, err := responseInstructions(source["instructions"])
	if err != nil {
		return nil, err
	}
	items, err := decodeResponseInput(source["input"])
	if err != nil {
		return nil, err
	}
	messages, err := responsesItemsToChatMessages(items, mode)
	if err != nil {
		return nil, err
	}
	if len(instructions) > 0 {
		messages = append([]map[string]any{{"role": "system", "content": instructions}}, messages...)
	}
	target["messages"] = mustJSON(messages)
	normalizeResponsesToolsToChat(target)
	normalizeResponsesChoiceToChat(target)
	normalizeResponsesGenerationToChat(source, target)
	return json.Marshal(target)
}

func chatMessagesToResponses(messages []map[string]json.RawMessage) ([]map[string]any, []string, error) {
	items := []map[string]any{}
	instructions := []string{}
	for _, message := range messages {
		role := stringValue(message["role"])
		content, err := chatContentToResponses(message["content"], role)
		if err != nil {
			return nil, nil, err
		}
		switch role {
		case "system", "developer":
			instructions = append(instructions, contentText(content))
		case "tool":
			items = append(items, map[string]any{"type": "function_call_output", "call_id": stringValue(message["tool_call_id"]), "output": contentText(content)})
		default:
			item := map[string]any{"type": "message", "role": role, "content": content}
			if name := stringValue(message["name"]); name != "" {
				item["name"] = name
			}
			items = append(items, item)
		}
		for _, call := range rawList(message["tool_calls"]) {
			function := objectMap(call["function"])
			items = append(items, map[string]any{"type": "function_call", "call_id": stringValue(call["id"]), "name": stringValue(function["name"]), "arguments": stringValue(function["arguments"])})
		}
	}
	return items, instructions, nil
}

func responsesItemsToChatMessages(items []map[string]json.RawMessage, mode Mode) ([]map[string]any, error) {
	messages := []map[string]any{}
	for _, item := range items {
		switch stringValue(item["type"]) {
		case "message":
			message := map[string]any{"role": stringValue(item["role"]), "content": responseContentToChat(item["content"])}
			if message["role"] == "" {
				message["role"] = "user"
			}
			messages = append(messages, message)
		case "function_call":
			message := lastAssistantMessage(messages)
			if message == nil {
				message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{}}
				messages = append(messages, message)
			}
			calls, _ := message["tool_calls"].([]any)
			calls = append(calls, map[string]any{"id": stringValue(item["call_id"]), "type": "function", "function": map[string]any{"name": stringValue(item["name"]), "arguments": stringValue(item["arguments"])}})
			message["tool_calls"] = calls
		case "function_call_output":
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": stringValue(item["call_id"]), "content": stringValue(item["output"])})
		case "reasoning":
			if mode == ModeStrict {
				return nil, fmt.Errorf("Chat Completions 无法表达 Responses reasoning item")
			}
		case "item_reference":
			if mode == ModeStrict {
				return nil, fmt.Errorf("Chat Completions 无法表达 Responses item_reference")
			}
		default:
			if mode == ModeStrict {
				return nil, fmt.Errorf("Chat Completions 无法表达 Responses item type %q", stringValue(item["type"]))
			}
		}
	}
	return messages, nil
}

func responseInstructions(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if text := stringValue(raw); text != "" {
		return text, nil
	}
	items := rawList(raw)
	parts := []string{}
	for _, item := range items {
		if stringValue(item["type"]) == "message" {
			parts = append(parts, contentTextValue(item["content"]))
		}
	}
	return joinText(parts), nil
}

func chatContentToResponses(raw json.RawMessage, role string) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []map[string]any{}, nil
	}
	if text := stringValue(raw); text != "" {
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		return []map[string]any{{"type": contentType, "text": text}}, nil
	}
	parts := []map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("解析消息 content: %w", err)
	}
	result := make([]map[string]any, 0, len(parts))
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	for _, part := range parts {
		switch stringValue(part["type"]) {
		case "text", "input_text":
			result = append(result, map[string]any{"type": contentType, "text": stringValue(part["text"])})
		case "output_text":
			result = append(result, map[string]any{"type": contentType, "text": stringValue(part["text"])})
		case "image_url", "input_image":
			image := objectMap(part["image_url"])
			url := stringValue(part["image_url"])
			if url == "" {
				url = stringValue(image["url"])
			}
			detail := stringValue(part["detail"])
			if detail == "" {
				detail = stringValue(image["detail"])
			}
			imagePart := map[string]any{"type": "input_image", "image_url": url}
			if detail != "" {
				imagePart["detail"] = detail
			}
			result = append(result, imagePart)
		case "input_file", "file":
			result = append(result, rawMapToAny(part))
		default:
			result = append(result, rawMapToAny(part))
		}
	}
	return result, nil
}

func normalizeChatToolsToResponses(target map[string]json.RawMessage) {
	tools := rawList(target["tools"])
	for _, tool := range tools {
		function := objectMap(tool["function"])
		if len(function) == 0 {
			continue
		}
		for key, value := range function {
			if key != "type" {
				tool[key] = value
			}
		}
		delete(tool, "function")
	}
	target["tools"] = mustJSON(tools)
}

func normalizeResponsesToolsToChat(target map[string]json.RawMessage) {
	tools := rawList(target["tools"])
	for _, tool := range tools {
		if stringValue(tool["type"]) != "function" {
			continue
		}
		function := map[string]any{}
		for _, key := range []string{"name", "description", "parameters", "strict"} {
			if value, exists := tool[key]; exists {
				function[key] = rawToAny(value)
			}
		}
		tool["function"] = mustJSON(function)
		delete(tool, "name")
		delete(tool, "description")
		delete(tool, "parameters")
		delete(tool, "strict")
	}
	target["tools"] = mustJSON(tools)
}

func normalizeChatChoiceToResponses(target map[string]json.RawMessage) {
	if choice := target["tool_choice"]; len(choice) > 0 {
		var value any
		if json.Unmarshal(choice, &value) == nil {
			if object, ok := value.(map[string]any); ok {
				if function, ok := object["function"].(map[string]any); ok {
					target["tool_choice"] = mustJSON(map[string]any{"type": "function", "name": function["name"]})
				}
			}
		}
	}
}

func normalizeResponsesChoiceToChat(target map[string]json.RawMessage) {
	if choice := target["tool_choice"]; len(choice) > 0 {
		var value any
		if json.Unmarshal(choice, &value) == nil {
			if object, ok := value.(map[string]any); ok && object["type"] == "function" {
				target["tool_choice"] = mustJSON(map[string]any{"type": "function", "function": map[string]any{"name": object["name"]}})
			}
		}
	}
}

func normalizeChatGenerationToResponses(source, target map[string]json.RawMessage) {
	if _, exists := target["max_output_tokens"]; !exists {
		copyIfPresent(source, target, "max_tokens", "max_output_tokens")
	}
	if _, exists := target["reasoning"]; !exists {
		if effort := source["reasoning_effort"]; len(effort) > 0 {
			target["reasoning"] = mustJSON(map[string]any{"effort": stringValue(effort)})
		}
	}
}

func normalizeResponsesGenerationToChat(source, target map[string]json.RawMessage) {
	if _, exists := target["max_completion_tokens"]; !exists {
		copyIfPresent(source, target, "max_output_tokens", "max_completion_tokens")
	}
	if reasoning := objectMap(source["reasoning"]); len(reasoning) > 0 {
		target["reasoning_effort"] = rawString(stringValue(reasoning["effort"]))
	}
}

func lastAssistantMessage(messages []map[string]any) map[string]any {
	if len(messages) == 0 {
		return nil
	}
	message := messages[len(messages)-1]
	if message["role"] == "assistant" {
		return message
	}
	return nil
}
