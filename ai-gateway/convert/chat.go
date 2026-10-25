package convert

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/weaming/ai-xyz/ai-gateway/ir"
)

// ChatCodec 是 OpenAI Chat Completions 与 IR 的编解码器。
type ChatCodec struct{}

// Protocol 返回 Chat Completions 协议标识。
func (ChatCodec) Protocol() Protocol { return ProtocolChatCompletions }

var chatRequestKeys = map[string]struct{}{
	"model": {}, "messages": {}, "tools": {}, "tool_choice": {}, "response_format": {},
	"stream": {}, "stream_options": {}, "reasoning_effort": {}, "max_tokens": {},
	"max_completion_tokens": {}, "temperature": {}, "top_p": {}, "stop": {},
	"parallel_tool_calls": {}, "store": {}, "presence_penalty": {}, "frequency_penalty": {},
	"service_tier": {}, "metadata": {}, "n": {}, "logprobs": {}, "top_logprobs": {},
	"audio": {}, "modalities": {}, "functions": {}, "function_call": {},
	"user": {}, "seed": {}, "logit_bias": {}, "prediction": {},
	"prompt_cache_key": {}, "prompt_cache_retention": {}, "safety_identifier": {}, "web_search_options": {},
}

// ParseRequest 把 Chat Completions 请求解析成 IR。
func (ChatCodec) ParseRequest(body []byte, options Options) (*ir.Request, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, fmt.Errorf("解析 Chat 请求: %w", err)
	}
	if err := validateChatRequest(source); err != nil {
		return nil, err
	}
	messages, err := decodeRawList(source["messages"])
	if err != nil {
		return nil, fmt.Errorf("解析 Chat messages: %w", err)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("messages 不能为空")
	}

	request := &ir.Request{}
	request.Model = stringValue(source["model"])
	request.Temperature = floatPtr(source["temperature"])
	request.TopP = floatPtr(source["top_p"])
	request.PresencePenalty = floatPtr(source["presence_penalty"])
	request.FrequencyPenalty = floatPtr(source["frequency_penalty"])
	request.ParallelToolCalls = boolPtr(source["parallel_tool_calls"])
	request.Store = boolPtr(source["store"])
	request.ServiceTier = stringValue(source["service_tier"])
	request.Metadata = cloneRaw(source["metadata"])
	request.Stream = boolValue(source["stream"])
	request.Stop = rawListStrings(source["stop"])
	if tokens := chatMaxTokens(source); tokens != nil {
		request.MaxOutputTokens = tokens
	}
	if effort := stringValue(source["reasoning_effort"]); effort != "" {
		request.Reasoning = &ir.ReasoningConfig{Effort: effort}
	}
	if options.EmitReasoning {
		request.Reasoning = ensureReasoning(request.Reasoning)
		request.Reasoning.IncludeThoughts = boolPointer(true)
	}
	if raw := source["stream_options"]; hasJSONValue(raw) {
		options, err := objectAnys(raw)
		if err != nil {
			return nil, fmt.Errorf("解析 Chat stream_options: %w", err)
		}
		request.StreamOptions = options
	}
	if raw := source["response_format"]; hasJSONValue(raw) {
		format, err := chatFormatToIR(raw)
		if err != nil {
			return nil, err
		}
		request.Text = &ir.TextConfig{Format: format}
	}
	if raw := source["tools"]; hasJSONValue(raw) {
		tools, builtIn, err := chatToolsToIR(raw)
		if err != nil {
			return nil, err
		}
		request.Tools = tools
		for _, toolType := range builtIn {
			request.Dropped = append(request.Dropped, "tools."+toolType)
		}
	}
	if raw := source["tool_choice"]; hasJSONValue(raw) {
		choice, err := chatChoiceToIR(raw)
		if err != nil {
			return nil, err
		}
		request.ToolChoice = choice
	}

	instructions, items, err := chatMessagesToIR(messages)
	if err != nil {
		return nil, err
	}
	request.Instructions = instructions
	request.Input = items
	request.Dropped = append(request.Dropped, collectDropped(source, chatDroppableKeys)...)
	request.Unknown = unknownKeys(source, chatRequestKeys)
	if err := rejectUnknown(request.Unknown, "Chat"); err != nil {
		return nil, err
	}
	return request, nil
}

func chatMaxTokens(source map[string]json.RawMessage) *int {
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if tokens := intPtr(source[key]); tokens != nil && *tokens > 0 {
			return tokens
		}
	}
	return nil
}

func chatMessagesToIR(messages []map[string]json.RawMessage) ([]ir.ContentPart, []ir.Item, error) {
	instructions := []ir.ContentPart{}
	items := []ir.Item{}
	callKinds := map[string]string{}
	for _, message := range messages {
		role := stringValue(message["role"])
		parts, err := chatContentToIR(message["content"], role)
		if err != nil {
			return nil, nil, err
		}
		switch role {
		case "system", "developer":
			if isTextOnly(parts) {
				if text := joinParts(parts); text != "" {
					instructions = append(instructions, ir.ContentPart{Type: "text", Text: text})
				}
			} else {
				items = append(items, ir.Item{Type: "message", Role: role, Content: parts})
			}
		case "assistant":
			if reasoning := stringValue(message["reasoning_content"]); reasoning != "" {
				items = append(items, ir.Item{Type: "reasoning", Role: "assistant", Content: []ir.ContentPart{{Type: "summary_text", Text: reasoning}}})
			}
			if len(parts) > 0 {
				items = append(items, ir.Item{Type: "message", Role: "assistant", Content: parts, Name: stringValue(message["name"])})
			}
		case "tool":
			output, err := chatToolOutput(message["content"])
			if err != nil {
				return nil, nil, err
			}
			toolType := stringValue(message["tool_call_type"])
			if toolType != "" && toolType != "function" && toolType != "custom" {
				return nil, nil, fmt.Errorf("Chat tool_call_type %q 无法转换", toolType)
			}
			callID := stringValue(message["tool_call_id"])
			if toolType == "" {
				toolType = callKinds[callID]
			}
			itemType := "function_call_output"
			if toolType == "custom" {
				itemType = "custom_tool_call_output"
			}
			items = append(items, ir.Item{Type: itemType, CallID: callID, Name: stringValue(message["name"]), Output: output})
		default:
			items = append(items, ir.Item{Type: "message", Role: role, Content: parts, Name: stringValue(message["name"])})
		}
		for _, call := range rawList(message["tool_calls"]) {
			item, err := chatToolCallToIR(call)
			if err != nil {
				return nil, nil, err
			}
			if item.CallID != "" {
				if item.Type == "custom_tool_call" {
					callKinds[item.CallID] = "custom"
				} else {
					callKinds[item.CallID] = "function"
				}
			}
			items = append(items, item)
		}
	}
	return instructions, items, nil
}

func chatContentToIR(raw json.RawMessage, role string) ([]ir.ContentPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil, nil
		}
		return []ir.ContentPart{{Type: partTypeForRole(role), Text: text}}, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("解析消息 content: %w", err)
	}
	result := make([]ir.ContentPart, 0, len(parts))
	for _, part := range parts {
		converted, err := chatPartToIR(part, role)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, nil
}

func chatPartToIR(part map[string]json.RawMessage, role string) (ir.ContentPart, error) {
	switch stringValue(part["type"]) {
	case "text", "input_text", "output_text":
		return ir.ContentPart{Type: partTypeForRole(role), Text: stringValue(part["text"])}, nil
	case "refusal":
		return ir.ContentPart{Type: "refusal", Refusal: stringValue(part["refusal"])}, nil
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
		if url == "" {
			return ir.ContentPart{}, fmt.Errorf("image_url 缺少 url")
		}
		return ir.ContentPart{Type: "input_image", ImageURL: url, Detail: detail}, nil
	case "input_audio", "audio":
		audio := objectMap(part["input_audio"])
		if len(audio) == 0 {
			audio = objectMap(part["audio"])
		}
		data := stringValue(audio["data"])
		if data == "" {
			return ir.ContentPart{}, fmt.Errorf("input_audio 缺少 data")
		}
		return ir.ContentPart{Type: "input_audio", FileData: data, AudioFormat: stringValue(audio["format"])}, nil
	case "file", "input_file":
		file := objectMap(part["file"])
		if len(file) == 0 {
			file = part
		}
		return ir.ContentPart{
			Type:     "input_file",
			FileID:   stringValue(file["file_id"]),
			FileData: stringValue(file["file_data"]),
			Filename: stringValue(file["filename"]),
		}, nil
	default:
		return ir.ContentPart{Type: stringValue(part["type"]), Raw: mustJSON(rawMapToAny(part))}, nil
	}
}

func partTypeForRole(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "text"
}

func chatToolOutput(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return mustJSON(""), nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return mustJSON(text), nil
	}
	parts, err := chatContentToIR(raw, "tool")
	if err != nil {
		return nil, err
	}
	if isTextOnly(parts) {
		return mustJSON(joinParts(parts)), nil
	}
	return raw, nil
}

func chatToolCallToIR(call map[string]json.RawMessage) (ir.Item, error) {
	callType := stringValue(call["type"])
	callID := stringValue(call["id"])
	switch callType {
	case "", "function":
		function := objectMap(call["function"])
		item := ir.Item{Type: "function_call", CallID: callID, Name: stringValue(function["name"]), Arguments: stringValue(function["arguments"])}
		if signature := chatThoughtSignature(call); signature != "" {
			item.Extensions = map[string]json.RawMessage{"thought_signature": mustJSON(signature)}
		}
		return item, nil
	case "custom":
		custom := objectMap(call["custom"])
		return ir.Item{Type: "custom_tool_call", CallID: callID, Name: stringValue(custom["name"]), Input: stringValueAnyString(custom["input"])}, nil
	default:
		return ir.Item{}, fmt.Errorf("Chat tool_call.type %q 无法转换", callType)
	}
}

// chatThoughtSignature 兼容 Gemini 风格的 snake_case 与 camelCase 字段名。
func chatThoughtSignature(call map[string]json.RawMessage) string {
	if signature := stringValue(call["thought_signature"]); signature != "" {
		return signature
	}
	return stringValue(call["thoughtSignature"])
}

func chatToolsToIR(raw json.RawMessage) ([]ir.Tool, []string, error) {
	tools := rawList(raw)
	result := make([]ir.Tool, 0, len(tools))
	builtIn := []string{}
	for _, tool := range tools {
		toolType := stringValue(tool["type"])
		switch toolType {
		case "function":
			function := objectMap(tool["function"])
			if len(function) == 0 || stringValue(function["name"]) == "" {
				return nil, nil, fmt.Errorf("Chat function tool 缺少 function.name")
			}
			result = append(result, ir.Tool{
				Type:        "function",
				Name:        stringValue(function["name"]),
				Description: stringValue(function["description"]),
				Parameters:  cloneRaw(function["parameters"]),
				Strict:      boolPtr(function["strict"]),
			})
		case "custom":
			custom := objectMap(tool["custom"])
			if len(custom) == 0 || stringValue(custom["name"]) == "" {
				return nil, nil, fmt.Errorf("Chat custom tool 缺少 custom.name")
			}
			result = append(result, ir.Tool{
				Type:        "custom",
				Name:        stringValue(custom["name"]),
				Description: stringValue(custom["description"]),
				Format:      cloneRaw(custom["format"]),
			})
		default:
			if isBuiltInToolType(toolType) {
				builtIn = append(builtIn, toolType)
				continue
			}
			return nil, nil, fmt.Errorf("Chat tool.type %q 无法转换", toolType)
		}
	}
	return result, builtIn, nil
}

func chatChoiceToIR(raw json.RawMessage) (json.RawMessage, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		switch text {
		case "auto", "none", "required":
			return mustJSON(text), nil
		}
		return nil, fmt.Errorf("不支持 tool_choice %q", text)
	}
	choice := objectMap(raw)
	switch stringValue(choice["type"]) {
	case "function":
		function := objectMap(choice["function"])
		name := stringValue(function["name"])
		if name == "" {
			return nil, fmt.Errorf("tool_choice.function 缺少 name")
		}
		return mustJSON(map[string]any{"type": "function", "name": name}), nil
	case "custom":
		custom := objectMap(choice["custom"])
		name := stringValue(custom["name"])
		if name == "" {
			return nil, fmt.Errorf("tool_choice.custom 缺少 name")
		}
		return mustJSON(map[string]any{"type": "custom", "name": name}), nil
	case "allowed_tools":
		allowed := objectMap(choice["allowed_tools"])
		tools, err := allowedToolsToIR(allowed["tools"])
		if err != nil {
			return nil, err
		}
		result := map[string]any{"type": "allowed_tools", "tools": tools}
		if mode := stringValue(allowed["mode"]); mode != "" {
			result["mode"] = mode
		}
		return mustJSON(result), nil
	default:
		return nil, fmt.Errorf("Chat tool_choice 无法转换")
	}
}

func allowedToolsToIR(raw json.RawMessage) ([]any, error) {
	result := []any{}
	for _, tool := range rawList(raw) {
		toolType := stringValue(tool["type"])
		if toolType != "function" && toolType != "custom" {
			return nil, fmt.Errorf("allowed_tools 暂不支持工具类型 %q", toolType)
		}
		name := stringValue(tool["name"])
		if name == "" {
			nested := objectMap(tool[toolType])
			name = stringValue(nested["name"])
		}
		if name == "" {
			return nil, fmt.Errorf("allowed_tools 的 %s 工具缺少 name", toolType)
		}
		result = append(result, map[string]any{"type": toolType, "name": name})
	}
	return result, nil
}

func chatFormatToIR(raw json.RawMessage) (json.RawMessage, error) {
	format := objectMap(raw)
	typeName := stringValue(format["type"])
	switch typeName {
	case "text", "json_object":
		return mustJSON(map[string]any{"type": typeName}), nil
	case "json_schema":
		schema := objectMap(format["json_schema"])
		if len(schema) == 0 || stringValue(schema["name"]) == "" || len(schema["schema"]) == 0 {
			return nil, fmt.Errorf("Chat response_format.json_schema 缺少 name 或 schema")
		}
		result := map[string]any{"type": "json_schema", "name": stringValue(schema["name"]), "schema": rawToAny(schema["schema"])}
		if description := stringValue(schema["description"]); description != "" {
			result["description"] = description
		}
		if raw := schema["strict"]; hasJSONValue(raw) {
			result["strict"] = rawToAny(raw)
		}
		return mustJSON(result), nil
	default:
		return nil, fmt.Errorf("Chat response_format.type %q 不受支持", typeName)
	}
}

// WriteRequest 把 IR 请求写成 Chat Completions 请求。
func (ChatCodec) WriteRequest(request *ir.Request, options Options) ([]byte, error) {
	target := map[string]any{}
	if request.Model != "" {
		target["model"] = request.Model
	}
	target["stream"] = request.Stream
	messages, err := irMessagesToChat(request, options)
	if err != nil {
		return nil, err
	}
	target["messages"] = messages
	if len(request.Tools) > 0 {
		tools, err := irToolsToChat(request.Tools)
		if err != nil {
			return nil, err
		}
		target["tools"] = tools
	}
	if hasJSONValue(request.ToolChoice) {
		choice, err := irChoiceToChat(request.ToolChoice)
		if err != nil {
			return nil, err
		}
		target["tool_choice"] = choice
	}
	if request.Text != nil && hasJSONValue(request.Text.Format) {
		format, err := irFormatToChat(request.Text.Format)
		if err != nil {
			return nil, err
		}
		target["response_format"] = format
	}
	if request.Reasoning != nil && request.Reasoning.Effort != "" {
		target["reasoning_effort"] = request.Reasoning.Effort
	}
	setAny(target, "temperature", request.Temperature)
	setAny(target, "top_p", request.TopP)
	setAny(target, "presence_penalty", request.PresencePenalty)
	setAny(target, "frequency_penalty", request.FrequencyPenalty)
	setAny(target, "parallel_tool_calls", request.ParallelToolCalls)
	setAny(target, "store", request.Store)
	if request.ServiceTier != "" {
		target["service_tier"] = request.ServiceTier
	}
	if hasJSONValue(request.Metadata) {
		target["metadata"] = rawToAny(request.Metadata)
	}
	if request.MaxOutputTokens != nil {
		target["max_completion_tokens"] = *request.MaxOutputTokens
	}
	if len(request.Stop) > 0 {
		target["stop"] = request.Stop
	}
	if len(request.StreamOptions) > 0 {
		target["stream_options"] = request.StreamOptions
	}
	return json.Marshal(target)
}

func irMessagesToChat(request *ir.Request, options Options) ([]any, error) {
	messages := []any{}
	if text := joinParts(request.Instructions); text != "" {
		messages = append(messages, map[string]any{"role": "system", "content": text})
	}
	for _, item := range request.Input {
		switch item.Type {
		case "message":
			content, err := irContentToChat(item.Content, item.Role)
			if err != nil {
				return nil, err
			}
			message := map[string]any{"role": item.Role, "content": content}
			if item.Name != "" {
				message["name"] = item.Name
			}
			messages = append(messages, message)
		case "reasoning":
			message := lastAssistantChatMessage(messages)
			if message == nil {
				message = map[string]any{"role": "assistant", "content": nil}
				messages = append(messages, message)
			}
			message["reasoning_content"] = joinParts(item.Content)
		case "function_call", "custom_tool_call":
			call, err := irToolCallToChat(item)
			if err != nil {
				return nil, err
			}
			message := lastAssistantChatMessage(messages)
			if message == nil {
				message = map[string]any{"role": "assistant", "content": nil}
				messages = append(messages, message)
			}
			calls, _ := message["tool_calls"].([]any)
			message["tool_calls"] = append(calls, call)
		case "function_call_output", "custom_tool_call_output":
			output, err := irToolOutputToChat(item)
			if err != nil {
				return nil, err
			}
			message := map[string]any{"role": "tool", "tool_call_id": item.CallID, "content": output}
			if item.Name != "" {
				message["name"] = item.Name
			}
			if item.Type == "custom_tool_call_output" {
				message["tool_call_type"] = "custom"
			}
			messages = append(messages, message)
		default:
			if isBuiltInItemType(item.Type) {
				continue
			}
			return nil, unsupportedItem("Chat Completions", item.Type)
		}
	}
	return messages, nil
}

func irContentToChat(parts []ir.ContentPart, role string) (any, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	if isTextOnly(parts) {
		return joinParts(parts), nil
	}
	result := make([]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text", "output_text":
			result = append(result, map[string]any{"type": "text", "text": part.Text})
		case "refusal":
			result = append(result, map[string]any{"type": "refusal", "refusal": part.Refusal})
		case "input_image":
			image := map[string]any{"url": part.ImageURL}
			if part.Detail != "" {
				image["detail"] = part.Detail
			}
			result = append(result, map[string]any{"type": "image_url", "image_url": image})
		case "input_audio":
			result = append(result, map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": part.FileData, "format": part.AudioFormat}})
		case "input_file":
			file := map[string]any{}
			if part.FileID != "" {
				file["file_id"] = part.FileID
			}
			if part.FileData != "" {
				file["file_data"] = part.FileData
			}
			if part.Filename != "" {
				file["filename"] = part.Filename
			}
			result = append(result, map[string]any{"type": "file", "file": file})
		default:
			return nil, fmt.Errorf("Chat Completions 无法表达 content type %q", part.Type)
		}
	}
	return result, nil
}

func irToolCallToChat(item ir.Item) (map[string]any, error) {
	if item.Type == "custom_tool_call" {
		return map[string]any{"id": item.CallID, "type": "custom", "custom": map[string]any{"name": item.Name, "input": item.Input}}, nil
	}
	call := map[string]any{"id": item.CallID, "type": "function", "function": map[string]any{"name": item.Name, "arguments": item.Arguments}}
	if signature := item.Extensions["thought_signature"]; len(signature) > 0 {
		if text := stringValue(signature); text != "" {
			call["thought_signature"] = text
		}
	}
	return call, nil
}

func irToolOutputToChat(item ir.Item) (any, error) {
	output := item.Output
	if len(output) == 0 || string(output) == "null" {
		return "", nil
	}
	var text string
	if json.Unmarshal(output, &text) == nil {
		return text, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(output, &parts); err != nil {
		return rawToAny(output), nil
	}
	converted, err := chatContentToIR(output, "tool")
	if err != nil {
		return nil, err
	}
	return irContentToChat(converted, "tool")
}

func irToolsToChat(tools []ir.Tool) ([]any, error) {
	result := make([]any, 0, len(tools))
	for _, tool := range tools {
		switch tool.Type {
		case "function":
			function := map[string]any{"name": tool.Name}
			if tool.Description != "" {
				function["description"] = tool.Description
			}
			if hasJSONValue(tool.Parameters) {
				function["parameters"] = rawToAny(tool.Parameters)
			}
			if tool.Strict != nil {
				function["strict"] = *tool.Strict
			}
			result = append(result, map[string]any{"type": "function", "function": function})
		case "custom":
			custom := map[string]any{"name": tool.Name}
			if tool.Description != "" {
				custom["description"] = tool.Description
			}
			if hasJSONValue(tool.Format) {
				custom["format"] = rawToAny(tool.Format)
			}
			result = append(result, map[string]any{"type": "custom", "custom": custom})
		default:
			if isBuiltInItemType(tool.Type) || isBuiltInToolType(tool.Type) {
				continue
			}
			return nil, fmt.Errorf("Chat Completions 无法表达 tool type %q", tool.Type)
		}
	}
	return result, nil
}

func irChoiceToChat(raw json.RawMessage) (any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	choice := objectMap(raw)
	switch stringValue(choice["type"]) {
	case "function":
		return map[string]any{"type": "function", "function": map[string]any{"name": stringValue(choice["name"])}}, nil
	case "custom":
		return map[string]any{"type": "custom", "custom": map[string]any{"name": stringValue(choice["name"])}}, nil
	case "allowed_tools":
		tools := []any{}
		var allowed []map[string]any
		if err := json.Unmarshal(choice["tools"], &allowed); err != nil {
			return nil, fmt.Errorf("allowed_tools.tools 必须是数组: %w", err)
		}
		for _, tool := range allowed {
			toolType := stringValueAny(tool["type"])
			name := stringValueAny(tool["name"])
			tools = append(tools, map[string]any{"type": toolType, toolType: map[string]any{"name": name}})
		}
		allowedTools := map[string]any{"tools": tools}
		if mode := stringValue(choice["mode"]); mode != "" {
			allowedTools["mode"] = mode
		}
		return map[string]any{"type": "allowed_tools", "allowed_tools": allowedTools}, nil
	default:
		return nil, fmt.Errorf("Chat Completions 无法表达 tool_choice")
	}
}

func irFormatToChat(raw json.RawMessage) (any, error) {
	format := objectMap(raw)
	typeName := stringValue(format["type"])
	switch typeName {
	case "text", "json_object":
		return map[string]any{"type": typeName}, nil
	case "json_schema":
		schema := map[string]any{"name": stringValue(format["name"]), "schema": rawToAny(format["schema"])}
		if description := stringValue(format["description"]); description != "" {
			schema["description"] = description
		}
		if raw := format["strict"]; hasJSONValue(raw) {
			schema["strict"] = rawToAny(raw)
		}
		return map[string]any{"type": "json_schema", "json_schema": schema}, nil
	default:
		return nil, fmt.Errorf("Chat Completions 无法表达 text format %q", typeName)
	}
}

func lastAssistantChatMessage(messages []any) map[string]any {
	if len(messages) == 0 {
		return nil
	}
	message, ok := messages[len(messages)-1].(map[string]any)
	if !ok || stringValueAny(message["role"]) != "assistant" {
		return nil
	}
	return message
}

// WriteResponse 把 IR 响应写成 Chat Completions 响应。
func (ChatCodec) WriteResponse(response *ir.Response, options Options) ([]byte, error) {
	message := map[string]any{"role": "assistant"}
	content := []string{}
	refusals := []string{}
	reasoning := []string{}
	toolCalls := []any{}
	for _, item := range response.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				switch part.Type {
				case "output_text", "text":
					if len(part.Annotations) > 0 {
						return nil, fmt.Errorf("Chat Completions 无法表达 output_text.annotations")
					}
					content = append(content, part.Text)
				case "refusal":
					refusals = append(refusals, part.Refusal)
				default:
					return nil, fmt.Errorf("Chat Completions 无法表达 content type %q", part.Type)
				}
			}
		case "reasoning":
			// 加密 reasoning 只在 Responses 内可回传；转 Chat 时保留可见 summary，丢弃密文。
			reasoning = append(reasoning, joinParts(item.Content))
		case "function_call", "custom_tool_call":
			call, err := irToolCallToChat(item)
			if err != nil {
				return nil, err
			}
			toolCalls = append(toolCalls, call)
		default:
			if isBuiltInItemType(item.Type) {
				continue
			}
			return nil, unsupportedItem("Chat Completions", item.Type)
		}
	}
	if len(content) > 0 {
		message["content"] = strings.Join(content, "")
	} else {
		message["content"] = nil
	}
	if len(refusals) > 0 {
		message["refusal"] = strings.Join(refusals, "")
	}
	if len(reasoning) > 0 && options.EmitReasoning {
		message["reasoning_content"] = strings.Join(reasoning, "")
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	choice := map[string]any{
		"index":         0,
		"message":       message,
		"finish_reason": chatFinishReason(response),
	}
	result := map[string]any{
		"id":      chatResponseID(response),
		"object":  "chat.completion",
		"created": response.CreatedAt,
		"model":   response.Model,
		"choices": []any{choice},
	}
	if response.ServiceTier != "" {
		result["service_tier"] = response.ServiceTier
	}
	if response.SystemFingerprint != "" {
		result["system_fingerprint"] = response.SystemFingerprint
	}
	if response.Usage != nil {
		result["usage"] = chatUsage(response.Usage)
	}
	return json.Marshal(result)
}

// ParseResponse 把 Chat Completions 响应解析成 IR。
func (ChatCodec) ParseResponse(body []byte, options Options) (*ir.Response, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, fmt.Errorf("解析 Chat 响应: %w", err)
	}
	response := &ir.Response{
		ID:                stringValue(source["id"]),
		Object:            "response",
		CreatedAt:         int64(intValue(source["created"])),
		Model:             stringValue(source["model"]),
		Usage:             usageFromChat(source["usage"]),
		ServiceTier:       stringValue(source["service_tier"]),
		SystemFingerprint: stringValue(source["system_fingerprint"]),
	}
	for _, choice := range rawList(source["choices"]) {
		if index := int(intValue(choice["index"])); index != 0 {
			return nil, fmt.Errorf("Chat 响应包含 choice index %d：多 choice (n>1) 无法转换为单一响应", index)
		}
		finish := chatFinishValue(choice["finish_reason"])
		response.FinishReason = finish
		response.Status, response.IncompleteDetails = responsesStatusFromChatFinish(finish)
		message := objectMap(choice["message"])
		if reasoning := stringValue(message["reasoning_content"]); reasoning != "" {
			response.Output = append(response.Output, ir.Item{Type: "reasoning", Role: "assistant", Content: []ir.ContentPart{{Type: "summary_text", Text: reasoning}}})
		}
		content, err := chatContentToIR(message["content"], "assistant")
		if err != nil {
			return nil, err
		}
		if refusal := stringValue(message["refusal"]); refusal != "" {
			content = append(content, ir.ContentPart{Type: "refusal", Refusal: refusal})
		}
		if len(content) > 0 {
			response.Output = append(response.Output, ir.Item{Type: "message", Role: "assistant", Content: content, Status: "completed"})
		}
		for _, call := range rawList(message["tool_calls"]) {
			item, err := chatToolCallToIR(call)
			if err != nil {
				return nil, err
			}
			item.Status = "completed"
			response.Output = append(response.Output, item)
		}
	}
	return response, nil
}

// chatFinishReason 优先保留拒绝与工具调用的 Chat 语义，其余回落到响应状态。
func chatFinishReason(response *ir.Response) string {
	hasToolCalls := false
	for _, item := range response.Output {
		if item.Type == "function_call" || item.Type == "custom_tool_call" {
			hasToolCalls = true
		}
		if item.Type == "message" {
			for _, part := range item.Content {
				if part.Type == "refusal" {
					return "refusal"
				}
			}
		}
	}
	if hasToolCalls {
		return "tool_calls"
	}
	if response.FinishReason != "" {
		return response.FinishReason
	}
	if response.Status == "failed" {
		return "error"
	}
	if response.Status == "incomplete" {
		switch incompleteReason(response.IncompleteDetails) {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		default:
			return "error"
		}
	}
	return "stop"
}

func chatResponseID(response *ir.Response) string {
	if response.ID != "" {
		return response.ID
	}
	return newID("chatcmpl-")
}
