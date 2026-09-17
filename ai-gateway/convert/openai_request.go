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
	if err := validateChatRequest(source); err != nil {
		return nil, err
	}

	target := make(map[string]json.RawMessage)
	copyKnown(source, target, "model", "temperature", "top_p", "stop", "stream", "tools", "tool_choice", "parallel_tool_calls", "store", "presence_penalty", "frequency_penalty", "service_tier", "metadata")
	copyIfPresent(source, target, "max_completion_tokens", "max_output_tokens")
	if raw := source["response_format"]; len(raw) > 0 {
		text, err := chatResponseFormatToResponses(raw)
		if err != nil {
			return nil, err
		}
		target["text"] = text
	}
	if raw := source["reasoning_effort"]; len(raw) > 0 {
		target["reasoning"] = mustJSON(map[string]any{"effort": stringValue(raw)})
	}
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
	if err := normalizeChatToolsToResponses(target); err != nil {
		return nil, err
	}
	if err := normalizeChatChoiceToResponses(target); err != nil {
		return nil, err
	}
	normalizeChatGenerationToResponses(source, target)
	return json.Marshal(target)
}

func convertResponsesRequestToChat(input []byte, defaultModel string, mode Mode) ([]byte, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(input, &source); err != nil {
		return nil, fmt.Errorf("解析 Responses 请求: %w", err)
	}
	if err := validateResponsesRequest(source); err != nil {
		return nil, err
	}

	target := make(map[string]json.RawMessage)
	copyKnown(source, target, "temperature", "top_p", "stop", "stream", "tools", "tool_choice", "parallel_tool_calls", "store", "presence_penalty", "frequency_penalty", "service_tier", "metadata")
	copyIfPresent(source, target, "max_output_tokens", "max_completion_tokens")
	if raw := source["text"]; len(raw) > 0 {
		responseFormat, err := responsesTextToChat(raw)
		if err != nil {
			return nil, err
		}
		target["response_format"] = responseFormat
	}
	if reasoning := objectMap(source["reasoning"]); len(reasoning) > 0 {
		target["reasoning_effort"] = rawString(stringValue(reasoning["effort"]))
	}
	if model, exists := source["model"]; exists {
		target["model"] = bytes.Clone(model)
	} else if defaultModel != "" {
		target["model"] = rawString(defaultModel)
	}

	instructionMessages, err := responseInstructions(source["instructions"])
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
	if len(instructionMessages) > 0 {
		messages = append(instructionMessages, messages...)
	}
	target["messages"] = mustJSON(messages)
	if err := normalizeResponsesToolsToChat(target); err != nil {
		return nil, err
	}
	if err := normalizeResponsesChoiceToChat(target); err != nil {
		return nil, err
	}
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
			if hasOnlyTextContent(content) {
				instructions = append(instructions, contentText(content))
				break
			}
			items = append(items, map[string]any{"type": "message", "role": role, "content": content})
		case "tool":
			items = append(items, map[string]any{"type": "function_call_output", "call_id": stringValue(message["tool_call_id"]), "output": responseOutputFromChatContent(content)})
		default:
			item := map[string]any{"type": "message", "role": role, "content": content}
			if name := stringValue(message["name"]); name != "" {
				item["name"] = name
			}
			items = append(items, item)
		}
		for _, call := range rawList(message["tool_calls"]) {
			item, err := chatToolCallToResponses(call)
			if err != nil {
				return nil, nil, err
			}
			items = append(items, item)
		}
	}
	return items, instructions, nil
}

func responsesItemsToChatMessages(items []map[string]json.RawMessage, mode Mode) ([]map[string]any, error) {
	messages := []map[string]any{}
	for _, item := range items {
		switch stringValue(item["type"]) {
		case "message":
			message, err := responseMessageToChat(item)
			if err != nil {
				return nil, err
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
		case "custom_tool_call":
			message := lastAssistantMessage(messages)
			if message == nil {
				message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{}}
				messages = append(messages, message)
			}
			calls, _ := message["tool_calls"].([]any)
			calls = append(calls, map[string]any{"id": stringValue(item["call_id"]), "type": "custom", "custom": map[string]any{"name": stringValue(item["name"]), "input": rawToAny(item["input"])}})
			message["tool_calls"] = calls
		case "function_call_output":
			output, err := responseToolOutputToChat(item["output"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": stringValue(item["call_id"]), "content": output})
		case "reasoning":
			if stringValue(item["encrypted_content"]) != "" {
				return nil, fmt.Errorf("Chat Completions 无法表达加密 reasoning")
			}
			text := responseReasoningText(item)
			if text == "" {
				continue
			}
			message := lastAssistantMessage(messages)
			if message == nil {
				message = map[string]any{"role": "assistant", "content": nil}
				messages = append(messages, message)
			}
			message["reasoning_content"] = text
		default:
			return nil, unsupportedResponsesItem(mode, stringValue(item["type"]))
		}
	}
	return messages, nil
}

func responseInstructions(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if text := stringValue(raw); text != "" {
		return []map[string]any{{"role": "system", "content": text}}, nil
	}
	items := rawList(raw)
	messages := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if stringValue(item["type"]) != "message" {
			return nil, unsupportedResponsesItem(ModeStrict, stringValue(item["type"]))
		}
		message, err := responseMessageToChat(item)
		if err != nil {
			return nil, err
		}
		if message["role"] == "assistant" {
			message["role"] = "system"
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func validateChatRequest(source map[string]json.RawMessage) error {
	if raw := source["n"]; len(raw) > 0 && string(raw) != "null" {
		if intValue(raw) > 1 {
			return fmt.Errorf("Chat -> Responses 不支持 n 大于 1")
		}
	}
	if boolValue(source["logprobs"]) {
		return fmt.Errorf("Chat -> Responses 不支持 logprobs")
	}
	if hasJSONValue(source["top_logprobs"]) {
		return fmt.Errorf("Chat -> Responses 不支持 top_logprobs")
	}
	if hasJSONValue(source["audio"]) || hasAudioModality(source["modalities"]) {
		return fmt.Errorf("Chat -> Responses 暂不支持音频输出")
	}
	return rejectFields(source, "Chat -> Responses", "functions", "function_call", "stream_options", "seed", "logit_bias", "user", "prediction")
}

func validateResponsesRequest(source map[string]json.RawMessage) error {
	return rejectFields(source, "Responses -> Chat", "previous_response_id", "conversation", "background", "prompt", "include", "moderation", "context_management", "max_tool_calls", "prompt_cache_key", "prompt_cache_options", "safety_identifier")
}

func rejectFields(source map[string]json.RawMessage, direction string, fields ...string) error {
	for _, field := range fields {
		if hasJSONValue(source[field]) {
			return fmt.Errorf("%s 不支持字段 %q，避免静默丢失语义", direction, field)
		}
	}
	return nil
}

func hasJSONValue(raw json.RawMessage) bool {
	value := string(raw)
	return len(raw) > 0 && value != "null" && value != "false" && value != "0" && value != `""` && value != "{}" && value != "[]"
}

func boolValue(raw json.RawMessage) bool {
	var value bool
	return json.Unmarshal(raw, &value) == nil && value
}

func hasAudioModality(raw json.RawMessage) bool {
	for _, value := range rawListStrings(raw) {
		if value == "audio" {
			return true
		}
	}
	return false
}

func rawListStrings(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
}

func chatResponseFormatToResponses(raw json.RawMessage) (json.RawMessage, error) {
	format := objectMap(raw)
	if len(format) == 0 {
		return nil, fmt.Errorf("Chat response_format 必须是 JSON 对象")
	}
	typeName := stringValue(format["type"])
	if typeName != "text" && typeName != "json_object" && typeName != "json_schema" {
		return nil, fmt.Errorf("Chat response_format.type %q 不受支持", typeName)
	}
	if typeName != "json_schema" {
		return mustJSON(map[string]any{"format": map[string]any{"type": typeName}}), nil
	}

	schema := objectMap(format["json_schema"])
	if len(schema) == 0 || stringValue(schema["name"]) == "" || len(schema["schema"]) == 0 {
		return nil, fmt.Errorf("Chat response_format.json_schema 缺少 name 或 schema")
	}
	textFormat := map[string]any{
		"type":   "json_schema",
		"name":   stringValue(schema["name"]),
		"schema": rawToAny(schema["schema"]),
	}
	if description := stringValue(schema["description"]); description != "" {
		textFormat["description"] = description
	}
	if strict := schema["strict"]; len(strict) > 0 {
		textFormat["strict"] = rawToAny(strict)
	}
	return mustJSON(map[string]any{"format": textFormat}), nil
}

func responsesTextToChat(raw json.RawMessage) (json.RawMessage, error) {
	text := objectMap(raw)
	format := objectMap(text["format"])
	if len(format) == 0 {
		return nil, fmt.Errorf("Responses text.format 缺失或格式无效")
	}
	typeName := stringValue(format["type"])
	if typeName != "text" && typeName != "json_object" && typeName != "json_schema" {
		return nil, fmt.Errorf("Responses text.format.type %q 无法转换为 Chat", typeName)
	}
	if typeName != "json_schema" {
		return mustJSON(map[string]any{"type": typeName}), nil
	}
	result := map[string]any{"type": "json_schema", "json_schema": map[string]any{
		"name":   stringValue(format["name"]),
		"schema": rawToAny(format["schema"]),
	}}
	schema := result["json_schema"].(map[string]any)
	if description := stringValue(format["description"]); description != "" {
		schema["description"] = description
	}
	if strict := format["strict"]; len(strict) > 0 {
		schema["strict"] = rawToAny(strict)
	}
	return mustJSON(result), nil
}

func chatToolCallToResponses(call map[string]json.RawMessage) (map[string]any, error) {
	callType := stringValue(call["type"])
	callID := stringValue(call["id"])
	switch callType {
	case "", "function":
		function := objectMap(call["function"])
		return map[string]any{"type": "function_call", "call_id": callID, "name": stringValue(function["name"]), "arguments": stringValue(function["arguments"])}, nil
	case "custom":
		custom := objectMap(call["custom"])
		return map[string]any{"type": "custom_tool_call", "call_id": callID, "name": stringValue(custom["name"]), "input": rawToAny(custom["input"])}, nil
	default:
		return nil, fmt.Errorf("Chat tool_call.type %q 无法转换为 Responses", callType)
	}
}

func responseOutputFromChatContent(content []map[string]any) any {
	if hasOnlyTextContent(content) {
		return contentText(content)
	}
	return content
}

func hasOnlyTextContent(content []map[string]any) bool {
	for _, part := range content {
		if part["type"] != "input_text" && part["type"] != "output_text" {
			return false
		}
	}
	return true
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

func normalizeChatToolsToResponses(target map[string]json.RawMessage) error {
	tools := rawList(target["tools"])
	for _, tool := range tools {
		switch stringValue(tool["type"]) {
		case "function":
			function := objectMap(tool["function"])
			if len(function) == 0 {
				return fmt.Errorf("Chat function tool 缺少 function 配置")
			}
			for key, value := range function {
				if key != "type" {
					tool[key] = value
				}
			}
			delete(tool, "function")
		case "custom":
			custom := objectMap(tool["custom"])
			if len(custom) == 0 {
				return fmt.Errorf("Chat custom tool 缺少 custom 配置")
			}
			for key, value := range custom {
				if key != "type" {
					tool[key] = value
				}
			}
			delete(tool, "custom")
		default:
			return fmt.Errorf("Chat tool.type %q 无法转换为 Responses", stringValue(tool["type"]))
		}
	}
	target["tools"] = mustJSON(tools)
	return nil
}

func normalizeResponsesToolsToChat(target map[string]json.RawMessage) error {
	tools := rawList(target["tools"])
	for _, tool := range tools {
		switch stringValue(tool["type"]) {
		case "function":
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
		case "custom":
			custom := map[string]any{}
			for _, key := range []string{"name", "description", "format"} {
				if value, exists := tool[key]; exists {
					custom[key] = rawToAny(value)
				}
			}
			tool["custom"] = mustJSON(custom)
			delete(tool, "name")
			delete(tool, "description")
			delete(tool, "format")
		default:
			return fmt.Errorf("Responses tool.type %q 无法转换为 Chat", stringValue(tool["type"]))
		}
	}
	target["tools"] = mustJSON(tools)
	return nil
}

func normalizeChatChoiceToResponses(target map[string]json.RawMessage) error {
	if choice := target["tool_choice"]; len(choice) > 0 {
		var value any
		if json.Unmarshal(choice, &value) == nil {
			if object, ok := value.(map[string]any); ok {
				switch object["type"] {
				case "allowed_tools":
					allowedTools, ok := object["allowed_tools"].(map[string]any)
					if !ok {
						return fmt.Errorf("Chat allowed_tools 缺少 allowed_tools 配置")
					}
					tools, err := normalizeAllowedTools(allowedTools["tools"], "Chat")
					if err != nil {
						return err
					}
					result := map[string]any{"type": "allowed_tools", "tools": tools}
					if mode, exists := allowedTools["mode"]; exists {
						result["mode"] = mode
					}
					target["tool_choice"] = mustJSON(result)
				case "function":
					function, ok := object["function"].(map[string]any)
					if !ok {
						return fmt.Errorf("Chat function tool_choice 缺少 function 配置")
					}
					target["tool_choice"] = mustJSON(map[string]any{"type": "function", "name": function["name"]})
				case "custom":
					custom, ok := object["custom"].(map[string]any)
					if !ok {
						return fmt.Errorf("Chat custom tool_choice 缺少 custom 配置")
					}
					target["tool_choice"] = mustJSON(map[string]any{"type": "custom", "name": custom["name"]})
				default:
					return fmt.Errorf("Chat tool_choice 无法转换为 Responses")
				}
			}
		}
	}
	return nil
}

func normalizeResponsesChoiceToChat(target map[string]json.RawMessage) error {
	if choice := target["tool_choice"]; len(choice) > 0 {
		var value any
		if json.Unmarshal(choice, &value) == nil {
			if object, ok := value.(map[string]any); ok {
				switch object["type"] {
				case "allowed_tools":
					tools, err := normalizeAllowedTools(object["tools"], "Responses")
					if err != nil {
						return err
					}
					allowedTools := map[string]any{"tools": tools}
					if mode, exists := object["mode"]; exists {
						allowedTools["mode"] = mode
					}
					target["tool_choice"] = mustJSON(map[string]any{"type": "allowed_tools", "allowed_tools": allowedTools})
				case "function":
					target["tool_choice"] = mustJSON(map[string]any{"type": "function", "function": map[string]any{"name": object["name"]}})
				case "custom":
					target["tool_choice"] = mustJSON(map[string]any{"type": "custom", "custom": map[string]any{"name": object["name"]}})
				default:
					return fmt.Errorf("Responses tool_choice.type %q 无法转换为 Chat", object["type"])
				}
			}
		}
	}
	return nil
}

func normalizeAllowedTools(raw any, direction string) ([]any, error) {
	tools, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s allowed_tools.tools 必须是数组", direction)
	}

	result := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s allowed_tools.tools 包含无效工具", direction)
		}
		toolType := stringValueAny(tool["type"])
		if toolType != "function" && toolType != "custom" {
			return nil, fmt.Errorf("%s allowed_tools 暂不支持工具类型 %q", direction, toolType)
		}

		name := stringValueAny(tool["name"])
		if name == "" {
			nested, _ := tool[toolType].(map[string]any)
			name = stringValueAny(nested["name"])
		}
		if name == "" {
			return nil, fmt.Errorf("%s allowed_tools 的 %s 工具缺少 name", direction, toolType)
		}
		result = append(result, map[string]any{"type": toolType, "name": name})
	}
	return result, nil
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
}

func unsupportedResponsesItem(mode Mode, itemType string) error {
	if itemType == "" {
		itemType = "<empty>"
	}
	return fmt.Errorf("Chat Completions 无法表达 Responses item type %q（conversion.mode=%s）", itemType, mode)
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
