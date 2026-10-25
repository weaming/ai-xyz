package convert

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/weaming/ai-xyz/ai-gateway/ir"
)

// ResponsesCodec 是 OpenAI Responses 与 IR 的编解码器。
type ResponsesCodec struct{}

// Protocol 返回 Responses 协议标识。
func (ResponsesCodec) Protocol() Protocol { return ProtocolResponses }

var responsesRequestKeys = map[string]struct{}{
	"model": {}, "instructions": {}, "input": {}, "tools": {}, "tool_choice": {},
	"reasoning": {}, "reasoning_effort": {}, "max_output_tokens": {}, "stream": {},
	"temperature": {}, "top_p": {}, "parallel_tool_calls": {}, "store": {},
	"service_tier": {}, "metadata": {}, "text": {}, "previous_response_id": {},
	"conversation": {}, "background": {}, "prompt": {}, "include": {}, "moderation": {},
	"context_management": {}, "max_tool_calls": {}, "prompt_cache_key": {},
	"prompt_cache_options": {}, "safety_identifier": {},
	"prompt_cache_retention": {},
}

// ParseRequest 把 Responses 请求解析成 IR。
func (ResponsesCodec) ParseRequest(body []byte, options Options) (*ir.Request, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, fmt.Errorf("解析 Responses 请求: %w", err)
	}
	if err := validateResponsesRequest(source); err != nil {
		return nil, err
	}
	request := &ir.Request{}
	request.Model = stringValue(source["model"])
	request.Temperature = floatPtr(source["temperature"])
	request.TopP = floatPtr(source["top_p"])
	request.MaxOutputTokens = intPtr(source["max_output_tokens"])
	request.Stream = boolValue(source["stream"])
	request.ParallelToolCalls = boolPtr(source["parallel_tool_calls"])
	request.Store = boolPtr(source["store"])
	request.ServiceTier = stringValue(source["service_tier"])
	request.Metadata = cloneRaw(source["metadata"])

	effort := stringValue(source["reasoning_effort"])
	if reasoning := objectMap(source["reasoning"]); len(reasoning) > 0 {
		config := &ir.ReasoningConfig{Effort: stringValue(reasoning["effort"]), Summary: stringValue(reasoning["summary"])}
		request.Reasoning = config
		if effort == "" {
			effort = config.Effort
		}
	}
	if effort != "" {
		request.Reasoning = ensureReasoning(request.Reasoning)
		request.Reasoning.Effort = effort
	}
	if raw := source["text"]; hasJSONValue(raw) {
		text := objectMap(raw)
		config := &ir.TextConfig{Verbosity: stringValue(text["verbosity"])}
		if format := objectMap(text["format"]); len(format) > 0 {
			config.Format = responsesFormatToIR(format)
		}
		request.Text = config
	}
	if raw := source["tools"]; hasJSONValue(raw) {
		tools, builtIn, err := responsesToolsToIR(raw)
		if err != nil {
			return nil, err
		}
		request.Tools = tools
		for _, toolType := range builtIn {
			request.Dropped = append(request.Dropped, "tools."+toolType)
		}
	}
	if raw := source["tool_choice"]; hasJSONValue(raw) {
		choice, err := responsesChoiceToIR(raw)
		if err != nil {
			return nil, err
		}
		request.ToolChoice = choice
	}

	instructions, err := responsesInstructionsToIR(source["instructions"])
	if err != nil {
		return nil, err
	}
	request.Instructions = instructions
	items, projectedItems, err := responsesInputToIR(source["input"])
	if err != nil {
		return nil, err
	}
	items, droppedItems := sanitizeToolItems(items)
	request.Dropped = append(request.Dropped, droppedItems...)
	for _, itemType := range projectedItems {
		request.Dropped = append(request.Dropped, "items."+itemType)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("Responses input 不能为空")
	}
	request.Input = items
	request.Dropped = append(request.Dropped, collectDropped(source, responsesDroppableKeys)...)
	request.Unknown = unknownKeys(source, responsesRequestKeys)
	if err := rejectUnknown(request.Unknown, "Responses"); err != nil {
		return nil, err
	}
	return request, nil
}

func responsesFormatToIR(format map[string]json.RawMessage) json.RawMessage {
	typeName := stringValue(format["type"])
	switch typeName {
	case "", "text":
		return mustJSON(map[string]any{"type": "text"})
	case "json_object":
		return mustJSON(map[string]any{"type": "json_object"})
	case "json_schema":
		result := map[string]any{"type": "json_schema", "name": stringValue(format["name"]), "schema": rawToAny(format["schema"])}
		if description := stringValue(format["description"]); description != "" {
			result["description"] = description
		}
		if raw := format["strict"]; hasJSONValue(raw) {
			result["strict"] = rawToAny(raw)
		}
		return mustJSON(result)
	default:
		return mustJSON(rawMapToAny(format))
	}
}

func responsesInstructionsToIR(raw json.RawMessage) ([]ir.ContentPart, error) {
	if !hasJSONValue(raw) {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil, nil
		}
		return []ir.ContentPart{{Type: "text", Text: text}}, nil
	}
	instructions := []ir.ContentPart{}
	for _, item := range rawList(raw) {
		if stringValue(item["type"]) != "message" {
			return nil, fmt.Errorf("Responses instructions 只接受 message，收到 %q", stringValue(item["type"]))
		}
		parts, err := responsesContentToIR(item["content"], "system")
		if err != nil {
			return nil, err
		}
		instructions = append(instructions, parts...)
	}
	return instructions, nil
}

func responsesInputToIR(raw json.RawMessage) ([]ir.Item, []string, error) {
	if !hasJSONValue(raw) {
		return nil, nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []ir.Item{{Type: "message", Role: "user", Content: []ir.ContentPart{{Type: "text", Text: text}}}}, nil, nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, fmt.Errorf("Responses input 必须是字符串或 item 数组: %w", err)
	}
	result := make([]ir.Item, 0, len(items))
	projected := []string{}
	for _, item := range items {
		converted, err := responsesItemToIR(item)
		if err != nil {
			return nil, nil, err
		}
		if itemType := stringValue(item["type"]); isBuiltInItemType(itemType) {
			projected = append(projected, itemType)
		}
		result = append(result, converted)
	}
	return result, projected, nil
}

func responsesItemToIR(item map[string]json.RawMessage) (ir.Item, error) {
	itemType := stringValue(item["type"])
	if itemType == "" && stringValue(item["role"]) != "" {
		itemType = "message"
	}
	switch itemType {
	case "message":
		role := stringValue(item["role"])
		if role == "" {
			role = "user"
		}
		content, err := responsesContentToIR(item["content"], role)
		if err != nil {
			return ir.Item{}, err
		}
		return ir.Item{Type: "message", Role: role, Content: content}, nil
	case "function_call":
		converted := ir.Item{Type: "function_call", CallID: stringValue(item["call_id"]), Name: stringValue(item["name"]), Arguments: stringValue(item["arguments"])}
		if signature := stringValue(item["thought_signature"]); signature != "" {
			converted.Extensions = map[string]json.RawMessage{"thought_signature": mustJSON(signature)}
		}
		return converted, nil
	case "custom_tool_call":
		return ir.Item{Type: "custom_tool_call", CallID: stringValue(item["call_id"]), Name: stringValue(item["name"]), Input: stringValueAnyString(rawToAny(item["input"]))}, nil
	case "function_call_output", "custom_tool_call_output":
		return ir.Item{Type: itemType, CallID: stringValue(item["call_id"]), Output: cloneRaw(item["output"])}, nil
	case "reasoning":
		converted := ir.Item{Type: "reasoning", Role: "assistant", Encrypted: stringValue(item["encrypted_content"]), Raw: mustJSON(rawMapToAny(item))}
		converted.Content = responsesReasoningParts(item)
		return converted, nil
	case "tool_search_call":
		converted := ir.Item{Type: "function_call", CallID: stringValue(item["call_id"]), Name: "tool_search", Arguments: stringValue(item["arguments"]), Raw: mustJSON(rawMapToAny(item))}
		return converted, nil
	case "tool_search_output":
		converted := ir.Item{Type: "function_call_output", CallID: stringValue(item["call_id"]), Output: mustJSON(rawMapToAny(item))}
		return converted, nil
	case "web_search_call", "file_search_call", "code_interpreter_call", "computer_call", "computer_call_output",
		"image_generation_call", "mcp_call", "mcp_call_output":
		// 厂商托管的工具记录：保留原始 item，Chat/Gemini 侧没有对应结构，输出时跳过。
		return ir.Item{Type: itemType, Raw: mustJSON(rawMapToAny(item))}, nil
	default:
		return ir.Item{}, fmt.Errorf("不支持 Responses input item.type %q", itemType)
	}
}

func responsesReasoningParts(item map[string]json.RawMessage) []ir.ContentPart {
	parts := []ir.ContentPart{}
	for _, part := range rawList(item["summary"]) {
		if text := stringValue(part["text"]); text != "" {
			parts = append(parts, ir.ContentPart{Type: "summary_text", Text: text})
		}
	}
	for _, part := range rawList(item["content"]) {
		if text := stringValue(part["text"]); text != "" {
			parts = append(parts, ir.ContentPart{Type: "reasoning_text", Text: text})
		}
	}
	return parts
}

func responsesContentToIR(raw json.RawMessage, role string) ([]ir.ContentPart, error) {
	if !hasJSONValue(raw) {
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
		return nil, fmt.Errorf("解析 Responses content: %w", err)
	}
	result := make([]ir.ContentPart, 0, len(parts))
	for _, part := range parts {
		converted, err := responsesPartToIR(part, role)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, nil
}

func responsesPartToIR(part map[string]json.RawMessage, role string) (ir.ContentPart, error) {
	switch stringValue(part["type"]) {
	case "input_text", "text":
		return ir.ContentPart{Type: partTypeForRole(role), Text: stringValue(part["text"])}, nil
	case "output_text":
		converted := ir.ContentPart{Type: "output_text", Text: stringValue(part["text"])}
		for _, annotation := range rawList(part["annotations"]) {
			converted.Annotations = append(converted.Annotations, mustJSON(rawMapToAny(annotation)))
		}
		return converted, nil
	case "refusal":
		return ir.ContentPart{Type: "refusal", Refusal: stringValue(part["refusal"])}, nil
	case "input_image":
		url := stringValue(part["image_url"])
		fileID := stringValue(part["file_id"])
		if url == "" && fileID == "" {
			return ir.ContentPart{}, fmt.Errorf("Responses input_image 缺少 image_url 或 file_id")
		}
		return ir.ContentPart{Type: "input_image", ImageURL: url, FileID: fileID, Detail: stringValue(part["detail"])}, nil
	case "input_file":
		return ir.ContentPart{
			Type:     "input_file",
			FileID:   stringValue(part["file_id"]),
			FileData: stringValue(part["file_data"]),
			Filename: stringValue(part["filename"]),
			FileURL:  stringValue(part["file_url"]),
			Detail:   stringValue(part["detail"]),
		}, nil
	case "input_audio":
		audio := objectMap(part["input_audio"])
		return ir.ContentPart{Type: "input_audio", FileData: stringValue(audio["data"]), AudioFormat: stringValue(audio["format"])}, nil
	default:
		return ir.ContentPart{Type: stringValue(part["type"]), Raw: mustJSON(rawMapToAny(part))}, nil
	}
}

func responsesToolsToIR(raw json.RawMessage) ([]ir.Tool, []string, error) {
	tools := rawList(raw)
	result := make([]ir.Tool, 0, len(tools))
	builtIn := []string{}
	for _, tool := range tools {
		switch toolType := stringValue(tool["type"]); toolType {
		case "function":
			if stringValue(tool["name"]) == "" {
				return nil, nil, fmt.Errorf("Responses function tool 缺少 name")
			}
			result = append(result, ir.Tool{
				Type:        "function",
				Name:        stringValue(tool["name"]),
				Description: stringValue(tool["description"]),
				Parameters:  cloneRaw(tool["parameters"]),
				Strict:      boolPtr(tool["strict"]),
			})
		case "custom":
			if stringValue(tool["name"]) == "" {
				return nil, nil, fmt.Errorf("Responses custom tool 缺少 name")
			}
			result = append(result, ir.Tool{
				Type:        "custom",
				Name:        stringValue(tool["name"]),
				Description: stringValue(tool["description"]),
				Format:      cloneRaw(tool["format"]),
			})
		default:
			if isBuiltInToolType(toolType) {
				// 内置工具（web_search/file_search/mcp/...）无法交给模型，丢弃并记录。
				builtIn = append(builtIn, toolType)
				continue
			}
			return nil, nil, fmt.Errorf("Responses 暂不支持 tool.type %q", toolType)
		}
	}
	return result, builtIn, nil
}

// isBuiltInToolType 判断是否是厂商托管的内置工具（网关无法执行，也不交给模型）。
func isBuiltInToolType(toolType string) bool {
	switch toolType {
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11",
		"file_search", "code_interpreter", "computer_use_preview",
		"image_generation", "mcp", "tool_search":
		return true
	}
	return strings.HasPrefix(toolType, "web_search") ||
		strings.HasPrefix(toolType, "mcp") ||
		strings.HasPrefix(toolType, "tool_search")
}

// isBuiltInItemType 判断是否是厂商托管工具的调用/产物 item。
func isBuiltInItemType(itemType string) bool {
	switch itemType {
	case "web_search_call", "file_search_call", "code_interpreter_call", "computer_call", "computer_call_output",
		"image_generation_call", "mcp_call", "mcp_call_output", "tool_search_call", "tool_search_output":
		return true
	}
	return false
}

// IsBuiltInToolType 供其它协议包判断内置工具类型。
func IsBuiltInToolType(toolType string) bool { return isBuiltInToolType(toolType) }

// IsBuiltInItemType 供其它协议包判断内置工具 item 类型。
func IsBuiltInItemType(itemType string) bool { return isBuiltInItemType(itemType) }

func responsesChoiceToIR(raw json.RawMessage) (json.RawMessage, error) {
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
		return mustJSON(map[string]any{"type": "function", "name": stringValue(choice["name"])}), nil
	case "custom":
		return mustJSON(map[string]any{"type": "custom", "name": stringValue(choice["name"])}), nil
	case "allowed_tools":
		tools, err := allowedToolsToIR(choice["tools"])
		if err != nil {
			return nil, err
		}
		result := map[string]any{"type": "allowed_tools", "tools": tools}
		if mode := stringValue(choice["mode"]); mode != "" {
			result["mode"] = mode
		}
		return mustJSON(result), nil
	default:
		return nil, fmt.Errorf("Responses tool_choice 无法转换")
	}
}

// WriteRequest 把 IR 请求写成 Responses 请求。
func (ResponsesCodec) WriteRequest(request *ir.Request, options Options) ([]byte, error) {
	target := map[string]any{"stream": request.Stream}
	if request.Model != "" {
		target["model"] = request.Model
	}
	if text := joinParts(request.Instructions); text != "" {
		target["instructions"] = text
	}
	target["input"] = irItemsToResponses(request.Input)
	if len(request.Tools) > 0 {
		tools, err := irToolsToResponses(request.Tools)
		if err != nil {
			return nil, err
		}
		target["tools"] = tools
	}
	if hasJSONValue(request.ToolChoice) {
		choice, err := irChoiceToResponses(request.ToolChoice)
		if err != nil {
			return nil, err
		}
		target["tool_choice"] = choice
	}
	if request.Text != nil {
		text := map[string]any{}
		if hasJSONValue(request.Text.Format) {
			text["format"] = irFormatToResponses(request.Text.Format)
		}
		if request.Text.Verbosity != "" {
			text["verbosity"] = request.Text.Verbosity
		}
		if len(text) > 0 {
			target["text"] = text
		}
	}
	if request.Reasoning != nil {
		reasoning := map[string]any{}
		if request.Reasoning.Effort != "" {
			reasoning["effort"] = request.Reasoning.Effort
		}
		if request.Reasoning.Summary != "" {
			reasoning["summary"] = request.Reasoning.Summary
		}
		if len(reasoning) > 0 {
			target["reasoning"] = reasoning
		}
	}
	setAny(target, "temperature", request.Temperature)
	setAny(target, "top_p", request.TopP)
	setAny(target, "parallel_tool_calls", request.ParallelToolCalls)
	setAny(target, "store", request.Store)
	if request.MaxOutputTokens != nil {
		target["max_output_tokens"] = *request.MaxOutputTokens
	}
	if request.ServiceTier != "" {
		target["service_tier"] = request.ServiceTier
	}
	if hasJSONValue(request.Metadata) {
		target["metadata"] = rawToAny(request.Metadata)
	}
	return json.Marshal(target)
}

func irItemsToResponses(items []ir.Item) []any {
	items, _ = sanitizeToolItems(items)
	result := make([]any, 0, len(items))
	for _, item := range items {
		if len(item.Raw) > 0 && (isBuiltInItemType(item.Type) || item.Type == "reasoning") {
			if payload := rawToAny(item.Raw); payload != nil {
				result = append(result, payload)
				continue
			}
		}
		switch item.Type {
		case "message":
			result = append(result, map[string]any{"type": "message", "role": item.Role, "content": irContentToResponses(item.Content, item.Role)})
		case "function_call":
			call := map[string]any{"type": "function_call", "call_id": item.CallID, "name": item.Name, "arguments": item.Arguments}
			if signature := item.Extensions["thought_signature"]; len(signature) > 0 {
				if text := stringValue(signature); text != "" {
					call["thought_signature"] = text
				}
			}
			result = append(result, call)
		case "custom_tool_call":
			result = append(result, map[string]any{"type": "custom_tool_call", "call_id": item.CallID, "name": item.Name, "input": item.Input})
		case "function_call_output", "custom_tool_call_output":
			output := rawToAny(item.Output)
			if output == nil {
				output = ""
			}
			result = append(result, map[string]any{"type": item.Type, "call_id": item.CallID, "output": output})
		case "reasoning":
			result = append(result, map[string]any{"type": "reasoning", "summary": irReasoningSummary(item.Content)})
		}
	}
	return result
}

func irReasoningSummary(content []ir.ContentPart) []any {
	summary := []any{}
	for _, part := range content {
		if part.Type == "summary_text" || part.Type == "reasoning_text" {
			summary = append(summary, map[string]any{"type": "summary_text", "text": part.Text})
		}
	}
	return summary
}

func irContentToResponses(parts []ir.ContentPart, role string) any {
	result := make([]any, 0, len(parts))
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text":
			result = append(result, map[string]any{"type": textType, "text": part.Text})
		case "output_text":
			converted := map[string]any{"type": "output_text", "text": part.Text}
			if len(part.Annotations) > 0 {
				annotations := make([]any, 0, len(part.Annotations))
				for _, annotation := range part.Annotations {
					annotations = append(annotations, rawToAny(annotation))
				}
				converted["annotations"] = annotations
			}
			result = append(result, converted)
		case "refusal":
			result = append(result, map[string]any{"type": "refusal", "refusal": part.Refusal})
		case "summary_text", "reasoning_text":
			result = append(result, map[string]any{"type": "summary_text", "text": part.Text})
		case "input_image":
			image := map[string]any{"type": "input_image"}
			if part.ImageURL != "" {
				image["image_url"] = part.ImageURL
			}
			if part.FileID != "" {
				image["file_id"] = part.FileID
			}
			if part.Detail != "" {
				image["detail"] = part.Detail
			}
			result = append(result, image)
		case "input_file":
			file := map[string]any{"type": "input_file"}
			if part.FileID != "" {
				file["file_id"] = part.FileID
			}
			if part.FileData != "" {
				file["file_data"] = part.FileData
			}
			if part.Filename != "" {
				file["filename"] = part.Filename
			}
			if part.FileURL != "" {
				file["file_url"] = part.FileURL
			}
			result = append(result, file)
		case "input_audio":
			result = append(result, map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": part.FileData, "format": part.AudioFormat}})
		}
	}
	return result
}

func irToolsToResponses(tools []ir.Tool) ([]any, error) {
	result := make([]any, 0, len(tools))
	for _, tool := range tools {
		switch tool.Type {
		case "function":
			converted := map[string]any{"type": "function", "name": tool.Name}
			if tool.Description != "" {
				converted["description"] = tool.Description
			}
			if hasJSONValue(tool.Parameters) {
				converted["parameters"] = rawToAny(tool.Parameters)
			}
			if tool.Strict != nil {
				converted["strict"] = *tool.Strict
			}
			result = append(result, converted)
		case "custom":
			converted := map[string]any{"type": "custom", "name": tool.Name}
			if tool.Description != "" {
				converted["description"] = tool.Description
			}
			if hasJSONValue(tool.Format) {
				converted["format"] = rawToAny(tool.Format)
			}
			result = append(result, converted)
		default:
			return nil, fmt.Errorf("Responses 无法表达 tool type %q", tool.Type)
		}
	}
	return result, nil
}

func irChoiceToResponses(raw json.RawMessage) (any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	choice := objectMap(raw)
	switch stringValue(choice["type"]) {
	case "function":
		return map[string]any{"type": "function", "name": stringValue(choice["name"])}, nil
	case "custom":
		return map[string]any{"type": "custom", "name": stringValue(choice["name"])}, nil
	case "allowed_tools":
		var tools []map[string]any
		if err := json.Unmarshal(choice["tools"], &tools); err != nil {
			return nil, fmt.Errorf("allowed_tools.tools 必须是数组: %w", err)
		}
		result := map[string]any{"type": "allowed_tools", "tools": tools}
		if mode := stringValue(choice["mode"]); mode != "" {
			result["mode"] = mode
		}
		return result, nil
	default:
		return nil, fmt.Errorf("Responses 无法表达 tool_choice")
	}
}

func irFormatToResponses(raw json.RawMessage) any {
	format := objectMap(raw)
	switch stringValue(format["type"]) {
	case "json_schema":
		result := map[string]any{"type": "json_schema", "name": stringValue(format["name"]), "schema": rawToAny(format["schema"])}
		if description := stringValue(format["description"]); description != "" {
			result["description"] = description
		}
		if raw := format["strict"]; hasJSONValue(raw) {
			result["strict"] = rawToAny(raw)
		}
		return result
	case "json_object":
		return map[string]any{"type": "json_object"}
	default:
		return map[string]any{"type": "text"}
	}
}

// WriteResponse 把 IR 响应写成 Responses 响应。
func (ResponsesCodec) WriteResponse(response *ir.Response, options Options) ([]byte, error) {
	output, err := responsesOutputItems(response, options)
	if err != nil {
		return nil, err
	}
	target := map[string]any{
		"id":         responsesResponseID(response),
		"object":     "response",
		"created_at": response.CreatedAt,
		"model":      response.Model,
		"status":     responsesStatus(response),
		"output":     output,
	}
	if response.Usage != nil {
		target["usage"] = responsesUsage(response.Usage)
	}
	if hasJSONValue(response.IncompleteDetails) {
		target["incomplete_details"] = rawToAny(response.IncompleteDetails)
	}
	if hasJSONValue(response.Error) {
		target["error"] = rawToAny(response.Error)
	}
	if response.ServiceTier != "" {
		target["service_tier"] = response.ServiceTier
	}
	return json.Marshal(target)
}

// AggregateStream 把 Responses SSE 聚合成 response；终态事件自带的字段优先保留。
func (ResponsesCodec) AggregateStream(reader io.Reader, options Options) ([]byte, error) {
	state := &responsesStreamReader{}
	accumulator := NewAccumulator()
	var terminal json.RawMessage
	emit := func(event ir.StreamEvent) error {
		if event.Type == ir.EventCompleted || event.Type == ir.EventFailed {
			if len(event.Raw) > 0 {
				terminal = append(json.RawMessage(nil), event.Raw...)
			}
		}
		return accumulator.Apply(event)
	}
	if err := ReadSSE(reader, func(frame SSEFrame) error { return state.consume(frame, emit) }); err != nil {
		return nil, err
	}
	if !state.terminal {
		return nil, fmt.Errorf("Responses SSE 在终止事件前结束")
	}
	response, err := accumulator.Response()
	if err != nil {
		return nil, err
	}
	if len(terminal) == 0 {
		return ResponsesCodec{}.WriteResponse(response, options)
	}
	var payload map[string]any
	if err := json.Unmarshal(terminal, &payload); err != nil {
		return nil, fmt.Errorf("解析 Responses 终态响应: %w", err)
	}
	if output, ok := payload["output"].([]any); !ok || len(output) == 0 {
		items, err := responsesOutputItems(response, options)
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			payload["output"] = items
		}
	}
	if _, exists := payload["status"]; !exists {
		payload["status"] = responsesStatus(response)
	}
	if _, exists := payload["usage"]; !exists && response.Usage != nil {
		payload["usage"] = responsesUsage(response.Usage)
	}
	if _, exists := payload["model"]; !exists && response.Model != "" {
		payload["model"] = response.Model
	}
	return json.Marshal(payload)
}

// responsesOutputItems 把 IR output item 逐项写成 Responses 的 output 数组。
func responsesOutputItems(response *ir.Response, options Options) ([]any, error) {
	output := make([]any, 0, len(response.Output))
	for index, item := range response.Output {
		converted, err := irItemToResponses(item, index, options)
		if err != nil {
			return nil, err
		}
		if converted != nil {
			output = append(output, converted)
		}
	}
	return output, nil
}

// ParseResponse 把 Responses 响应解析成 IR。
func (ResponsesCodec) ParseResponse(body []byte, options Options) (*ir.Response, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, fmt.Errorf("解析 Responses 响应: %w", err)
	}
	response := &ir.Response{
		ID:                stringValue(source["id"]),
		Object:            "response",
		CreatedAt:         intValue(source["created_at"]),
		Model:             stringValue(source["model"]),
		Status:            stringValue(source["status"]),
		Usage:             usageFromResponses(source["usage"]),
		IncompleteDetails: cloneRaw(source["incomplete_details"]),
		Error:             cloneRaw(source["error"]),
		ServiceTier:       stringValue(source["service_tier"]),
	}
	for _, item := range rawList(source["output"]) {
		converted, err := responsesItemToIR(item)
		if err != nil {
			return nil, err
		}
		converted.ID = stringValue(item["id"])
		converted.Status = stringValue(item["status"])
		response.Output = append(response.Output, converted)
	}
	response.Output, _ = sanitizeToolItems(response.Output)
	response.FinishReason = responsesFinishReason(response.Status, response.IncompleteDetails)
	return response, nil
}

func irItemToResponses(item ir.Item, index int, options Options) (any, error) {
	if isToolCallItem(item.Type) && (item.Name == "" || item.CallID == "") {
		return nil, nil
	}
	if len(item.Raw) > 0 {
		if payload := rawToAny(item.Raw); payload != nil {
			return payload, nil
		}
	}
	switch item.Type {
	case "message":
		content := []any{}
		for _, part := range item.Content {
			if part.Type == "summary_text" || part.Type == "reasoning_text" {
				continue
			}
			content = append(content, irContentToResponses([]ir.ContentPart{part}, "assistant"))
		}
		flat := []any{}
		for _, entry := range content {
			flat = append(flat, entry.([]any)...)
		}
		return map[string]any{
			"id":      itemID(item, "msg_", index),
			"type":    "message",
			"role":    messageRole(item.Role),
			"content": flat,
			"status":  itemStatus(item),
		}, nil
	case "reasoning":
		converted := map[string]any{
			"id":      itemID(item, "rs_", index),
			"type":    "reasoning",
			"summary": irReasoningSummary(item.Content),
			"status":  itemStatus(item),
		}
		if item.Encrypted != "" {
			converted["encrypted_content"] = item.Encrypted
		}
		return converted, nil
	case "function_call":
		converted := map[string]any{
			"id":        itemID(item, "fc_", index),
			"type":      "function_call",
			"call_id":   item.CallID,
			"name":      item.Name,
			"arguments": item.Arguments,
			"status":    itemStatus(item),
		}
		if signature := item.Extensions["thought_signature"]; len(signature) > 0 {
			if text := stringValue(signature); text != "" {
				converted["thought_signature"] = text
			}
		}
		return converted, nil
	case "custom_tool_call":
		return map[string]any{
			"id":      itemID(item, "ctc_", index),
			"type":    "custom_tool_call",
			"call_id": item.CallID,
			"name":    item.Name,
			"input":   item.Input,
			"status":  itemStatus(item),
		}, nil
	default:
		return nil, unsupportedItem("Responses", item.Type)
	}
}

func itemID(item ir.Item, prefix string, index int) string {
	if item.ID != "" {
		return item.ID
	}
	return fmt.Sprintf("%s%d", prefix, index)
}

func itemStatus(item ir.Item) string {
	if item.Status != "" {
		return item.Status
	}
	return "completed"
}

func messageRole(role string) string {
	if role == "" {
		return "assistant"
	}
	return role
}

func responsesResponseID(response *ir.Response) string {
	if response.ID == "" {
		return newID("resp_")
	}
	if strings.HasPrefix(response.ID, "resp_") {
		return response.ID
	}
	return "resp_" + response.ID
}

func responsesStatus(response *ir.Response) string {
	switch response.Status {
	case "":
		return "completed"
	case "cancelled", "failed", "incomplete", "in_progress", "queued", "completed":
		return response.Status
	default:
		return "completed"
	}
}
