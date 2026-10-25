package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/weaming/ai-xyz/ai-gateway/convert"
	"github.com/weaming/ai-xyz/ai-gateway/ir"
	"google.golang.org/genai"
)

// nativeRequest 是 generateContent 的 JSON 请求体，也是网关内 IR 的 Gemini 编码结果。
type nativeRequest struct {
	Contents          []*genai.Content             `json:"contents"`
	SystemInstruction *genai.Content               `json:"systemInstruction,omitempty"`
	Tools             []*genai.Tool                `json:"tools,omitempty"`
	ToolConfig        *genai.ToolConfig            `json:"toolConfig,omitempty"`
	GenerationConfig  *genai.GenerateContentConfig `json:"generationConfig,omitempty"`
}

// encodeRequest 把 IR 请求编码成 generateContent 请求体。
func encodeRequest(request *ir.Request, defaultThinking genai.ThinkingLevel) (*nativeRequest, error) {
	if err := rejectUnsupportedRequest(request); err != nil {
		return nil, err
	}
	systemText := joinTextParts(contentTexts(request.Instructions))
	contents, extraSystem, err := encodeContents(request.Input, systemText)
	if err != nil {
		return nil, err
	}
	native := &nativeRequest{Contents: contents}
	systemText = joinTextParts([]string{systemText, extraSystem})
	if systemText != "" {
		native.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: systemText}}}
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("请求缺少可发送的 contents")
	}
	tools, err := encodeTools(request.Tools)
	if err != nil {
		return nil, err
	}
	native.Tools = tools
	toolConfig, err := encodeToolChoice(request.ToolChoice)
	if err != nil {
		return nil, err
	}
	native.ToolConfig = toolConfig

	config, err := encodeConfig(request, defaultThinking)
	if err != nil {
		return nil, err
	}
	native.GenerationConfig = config
	return native, nil
}

// rejectUnsupportedRequest 只拒绝会改变语义的请求；调参/元数据字段（store、metadata、
// service_tier、parallel_tool_calls、text.verbosity、reasoning.summary）直接忽略。
func rejectUnsupportedRequest(request *ir.Request) error {
	if request.PreviousResponse != "" {
		return fmt.Errorf("generate_content 不支持 previous_response_id")
	}
	if len(request.Unknown) > 0 {
		keys := make([]string, 0, len(request.Unknown))
		for key := range request.Unknown {
			keys = append(keys, key)
		}
		return fmt.Errorf("generate_content 不支持请求字段 %q", strings.Join(keys, ","))
	}
	return nil
}

// encodeContents 把 IR item 转换成 Gemini contents，并返回额外的 system 文本。
func encodeContents(items []ir.Item, systemText string) ([]*genai.Content, string, error) {
	contents := make([]*genai.Content, 0, len(items))
	extraSystem := []string{}
	callNames := map[string]string{}
	for _, item := range items {
		switch item.Type {
		case "message":
			content, system, err := encodeMessage(item)
			if err != nil {
				return nil, "", err
			}
			if system != "" {
				extraSystem = append(extraSystem, system)
				continue
			}
			if content != nil {
				contents = append(contents, content)
			}
		case "function_call":
			part := &genai.Part{FunctionCall: &genai.FunctionCall{Name: item.Name, Args: decodeArgs(item.Arguments)}}
			if item.CallID != "" {
				part.FunctionCall.ID = normalizeToolCallID(item.CallID)
				callNames[item.CallID] = item.Name
			}
			signature, err := decodeThoughtSignature(item.Extensions["thought_signature"])
			if err != nil {
				return nil, "", fmt.Errorf("function_call %q: %w", item.CallID, err)
			}
			part.ThoughtSignature = signature
			contents = appendModelPart(contents, part)
		case "function_call_output":
			name := item.Name
			if name == "" {
				name = callNames[item.CallID]
			}
			if name == "" {
				return nil, "", fmt.Errorf("function_call_output 的 call_id %q 找不到对应 function name", item.CallID)
			}
			part := &genai.Part{FunctionResponse: &genai.FunctionResponse{Name: name, Response: toolOutputToResponse(outputText(item.Output))}}
			if item.CallID != "" {
				part.FunctionResponse.ID = normalizeToolCallID(item.CallID)
			}
			contents = appendUserPart(contents, part)
		case "custom_tool_call", "custom_tool_call_output":
			return nil, "", fmt.Errorf("generate_content 不支持 custom tool 的 %s", item.Type)
		case "reasoning":
			continue
		default:
			if convert.IsBuiltInItemType(item.Type) {
				// 厂商托管工具的记录对模型不可见，直接跳过。
				continue
			}
			return nil, "", fmt.Errorf("generate_content 不支持 item type %q", item.Type)
		}
	}
	return contents, joinTextParts(extraSystem), nil
}

func encodeMessage(item ir.Item) (*genai.Content, string, error) {
	parts := make([]*genai.Part, 0, len(item.Content))
	for _, part := range item.Content {
		switch item.Role {
		case "system", "developer":
			if part.Type != "text" && part.Type != "output_text" {
				return nil, "", fmt.Errorf("generate_content 的 system 消息只支持文本")
			}
			parts = append(parts, &genai.Part{Text: part.Text})
		case "assistant":
			if part.Type != "text" && part.Type != "output_text" {
				return nil, "", fmt.Errorf("generate_content 的 assistant 消息不支持 content type %q", part.Type)
			}
			parts = append(parts, &genai.Part{Text: part.Text})
		case "tool":
			converted, err := encodePart(part, item.Role)
			if err != nil {
				return nil, "", err
			}
			parts = append(parts, converted)
		default:
			converted, err := encodePart(part, item.Role)
			if err != nil {
				return nil, "", err
			}
			parts = append(parts, converted)
		}
	}
	if len(parts) == 0 {
		return nil, "", nil
	}
	if item.Role == "system" || item.Role == "developer" {
		return nil, joinTextParts(contentTexts(item.Content)), nil
	}
	role := genai.RoleUser
	if item.Role == "assistant" {
		role = genai.RoleModel
	}
	return &genai.Content{Role: role, Parts: parts}, "", nil
}

func encodePart(part ir.ContentPart, role string) (*genai.Part, error) {
	if len(part.Annotations) > 0 {
		return nil, fmt.Errorf("generate_content 无法表达 output_text.annotations")
	}
	switch part.Type {
	case "text", "output_text":
		return &genai.Part{Text: part.Text}, nil
	case "input_image":
		if part.ImageURL == "" {
			return nil, fmt.Errorf("generate_content 只支持 image_url 的 base64 data URL")
		}
		blob, err := parseDataURL(part.ImageURL)
		if err != nil {
			return nil, err
		}
		return &genai.Part{InlineData: blob}, nil
	case "input_audio":
		data, err := base64.StdEncoding.DecodeString(part.FileData)
		if err != nil {
			return nil, fmt.Errorf("解码 audio data: %w", err)
		}
		mime := "audio/" + part.AudioFormat
		if part.AudioFormat == "" {
			mime = "audio/wav"
		}
		return &genai.Part{InlineData: &genai.Blob{MIMEType: mime, Data: data}}, nil
	default:
		return nil, fmt.Errorf("generate_content 不支持 content type %q", part.Type)
	}
}

func parseDataURL(urlStr string) (*genai.Blob, error) {
	const prefix = "data:"
	if !strings.HasPrefix(urlStr, prefix) {
		return nil, fmt.Errorf("仅支持 base64 data URL")
	}
	comma := strings.Index(urlStr, ",")
	if comma == -1 {
		return nil, fmt.Errorf("data URL 缺少数据分隔符")
	}
	header := urlStr[len(prefix):comma]
	dataStr := urlStr[comma+1:]
	if !strings.HasSuffix(header, ";base64") {
		return nil, fmt.Errorf("data URL 必须使用 base64 编码")
	}
	mime := strings.TrimSuffix(header, ";base64")
	if mime == "" {
		mime = "image/png"
	}
	data, err := base64.StdEncoding.DecodeString(dataStr)
	if err != nil {
		return nil, fmt.Errorf("解码 data URL: %w", err)
	}
	return &genai.Blob{MIMEType: mime, Data: data}, nil
}

func appendModelPart(contents []*genai.Content, part *genai.Part) []*genai.Content {
	if len(contents) > 0 {
		last := contents[len(contents)-1]
		if last.Role == genai.RoleModel {
			last.Parts = append(last.Parts, part)
			return contents
		}
	}
	return append(contents, &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}})
}

func appendUserPart(contents []*genai.Content, part *genai.Part) []*genai.Content {
	if len(contents) > 0 {
		last := contents[len(contents)-1]
		if last.Role == genai.RoleUser && hasFunctionResponse(last) {
			last.Parts = append(last.Parts, part)
			return contents
		}
	}
	return append(contents, &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{part}})
}

func hasFunctionResponse(content *genai.Content) bool {
	for _, part := range content.Parts {
		if part.FunctionResponse != nil {
			return true
		}
	}
	return false
}

func encodeTools(tools []ir.Tool) ([]*genai.Tool, error) {
	declarations := make([]*genai.FunctionDeclaration, 0, len(tools))
	for _, tool := range tools {
		if tool.Type != "function" {
			if convert.IsBuiltInToolType(tool.Type) {
				continue
			}
			return nil, fmt.Errorf("generate_content 只支持 function tool，收到 %q", tool.Type)
		}
		declaration := &genai.FunctionDeclaration{Name: tool.Name, Description: tool.Description}
		if hasRawValue(tool.Parameters) {
			schema, ok := toGenaiSchema(SanitizeSchema(rawToAny(tool.Parameters)))
			if !ok {
				return nil, fmt.Errorf("function tool %q 的 parameters 无法转换", tool.Name)
			}
			declaration.Parameters = schema
		}
		declarations = append(declarations, declaration)
	}
	if len(declarations) == 0 {
		return nil, nil
	}
	return []*genai.Tool{{FunctionDeclarations: declarations}}, nil
}

func encodeToolChoice(raw json.RawMessage) (*genai.ToolConfig, error) {
	if !hasRawValue(raw) {
		return nil, nil
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		switch mode {
		case "none":
			return &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeNone}}, nil
		case "auto":
			return &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto}}, nil
		case "required":
			return &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}}, nil
		}
		return nil, fmt.Errorf("不支持 tool_choice %q", mode)
	}
	choice := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, fmt.Errorf("解析 tool_choice: %w", err)
	}
	choiceType := rawString(choice["type"])
	switch choiceType {
	case "function":
		name := rawString(choice["name"])
		if name == "" {
			return nil, fmt.Errorf("tool_choice.function 缺少 name")
		}
		return &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny, AllowedFunctionNames: []string{name}}}, nil
	case "allowed_tools":
		var tools []map[string]any
		if err := json.Unmarshal(choice["tools"], &tools); err != nil {
			return nil, fmt.Errorf("tool_choice.tools 必须是数组: %w", err)
		}
		names := make([]string, 0, len(tools))
		for _, tool := range tools {
			name, _ := tool["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("allowed_tools 的 function 缺少 name")
			}
			names = append(names, name)
		}
		return &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny, AllowedFunctionNames: names}}, nil
	case "custom":
		return nil, fmt.Errorf("generate_content 不支持 custom tool_choice")
	default:
		return nil, fmt.Errorf("不支持 tool_choice.type %q", choiceType)
	}
}

func encodeConfig(request *ir.Request, defaultThinking genai.ThinkingLevel) (*genai.GenerateContentConfig, error) {
	config := &genai.GenerateContentConfig{}
	if request.Temperature != nil {
		config.Temperature = float32Ptr(*request.Temperature)
	}
	if request.TopP != nil {
		config.TopP = float32Ptr(*request.TopP)
	}
	if request.PresencePenalty != nil {
		config.PresencePenalty = float32Ptr(*request.PresencePenalty)
	}
	if request.FrequencyPenalty != nil {
		config.FrequencyPenalty = float32Ptr(*request.FrequencyPenalty)
	}
	if request.MaxOutputTokens != nil {
		config.MaxOutputTokens = int32(*request.MaxOutputTokens)
	}
	config.StopSequences = request.Stop
	config.ThinkingConfig = buildThinkingConfig(request.Reasoning, defaultThinking)
	if request.Text != nil && hasRawValue(request.Text.Format) {
		if err := applyFormat(config, request.Text.Format); err != nil {
			return nil, err
		}
	}
	return config, nil
}

func applyFormat(config *genai.GenerateContentConfig, raw json.RawMessage) error {
	format := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &format); err != nil {
		return fmt.Errorf("解析 text.format: %w", err)
	}
	switch rawString(format["type"]) {
	case "text":
		config.ResponseMIMEType = "text/plain"
	case "json_object":
		config.ResponseMIMEType = "application/json"
	case "json_schema":
		config.ResponseMIMEType = "application/json"
		schema, ok := toGenaiSchema(SanitizeSchema(rawToAny(format["schema"])))
		if !ok {
			return fmt.Errorf("text.format.schema 无法转换")
		}
		config.ResponseSchema = schema
	default:
		return fmt.Errorf("不支持 text.format.type %q", rawString(format["type"]))
	}
	return nil
}

// buildThinkingConfig 把 IR reasoning 映射到 Gemini 思考配置。
func buildThinkingConfig(reasoning *ir.ReasoningConfig, defaultLevel genai.ThinkingLevel) *genai.ThinkingConfig {
	effort := ""
	includeThoughts := true
	if reasoning != nil {
		effort = strings.ToLower(reasoning.Effort)
		if reasoning.IncludeThoughts != nil {
			includeThoughts = *reasoning.IncludeThoughts
		}
	}
	if effort == "none" || effort == "off" || effort == "disabled" || effort == "0" {
		budget := int32(0)
		return &genai.ThinkingConfig{IncludeThoughts: false, ThinkingBudget: &budget}
	}
	config := &genai.ThinkingConfig{IncludeThoughts: includeThoughts}
	if level := mapReasoningEffort(effort); level != genai.ThinkingLevelUnspecified {
		config.ThinkingLevel = level
	} else if defaultLevel != "" && defaultLevel != genai.ThinkingLevelUnspecified {
		config.ThinkingLevel = defaultLevel
	}
	return config
}

func mapReasoningEffort(effort string) genai.ThinkingLevel {
	switch effort {
	case "low", "minimal":
		return genai.ThinkingLevelLow
	case "medium":
		return genai.ThinkingLevelMedium
	case "high":
		return genai.ThinkingLevelHigh
	default:
		return genai.ThinkingLevelUnspecified
	}
}

// normalizeToolCallID Gemini 要求 tool call id 仅含 [a-zA-Z0-9_-] 且不超过 64 字符。
func normalizeToolCallID(id string) string {
	var builder strings.Builder
	for _, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-' {
			builder.WriteRune(char)
		} else {
			builder.WriteRune('_')
		}
	}
	value := builder.String()
	if len(value) > 64 {
		return value[:64]
	}
	return value
}

// decodeThoughtSignature 把历史消息里的 base64 签名解码成 SDK 使用的 bytes。
func decodeThoughtSignature(raw json.RawMessage) ([]byte, error) {
	signature := rawString(raw)
	if signature == "" {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return nil, fmt.Errorf("thought_signature 不是合法 base64: %w", err)
	}
	return decoded, nil
}

func encodeBase64(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(blob)
}

// toolOutputToResponse 语义：JSON object 原样、Error: 开头标记失败、其余文本作为成功输出。
func toolOutputToResponse(text string) map[string]any {
	if strings.HasPrefix(text, "Error:") {
		return map[string]any{"error": text}
	}
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err == nil {
		if object, ok := parsed.(map[string]any); ok {
			return object
		}
		return map[string]any{"output": parsed}
	}
	return map[string]any{"output": text}
}

func decodeArgs(arguments string) map[string]any {
	args := map[string]any{}
	if arguments == "" {
		return args
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return map[string]any{"output": arguments}
	}
	return args
}

// outputText 把 IR 工具输出还原成文本，Gemini 的 functionResponse 只接受对象。
func outputText(output json.RawMessage) string {
	if !hasRawValue(output) {
		return ""
	}
	var text string
	if json.Unmarshal(output, &text) == nil {
		return text
	}
	return string(output)
}

func joinTextParts(items []string) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if item != "" {
			parts = append(parts, item)
		}
	}
	return strings.Join(parts, "\n\n")
}

func contentTexts(parts []ir.ContentPart) []string {
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type == "text" || part.Type == "output_text" {
			result = append(result, part.Text)
		}
	}
	return result
}

func float32Ptr(value float64) *float32 {
	converted := float32(value)
	return &converted
}

func hasRawValue(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return len(raw) > 0 && value != "null" && value != "{}" && value != "[]" && value != `""`
}

func rawString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 {
		return ""
	}
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func rawToAny(raw json.RawMessage) any {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}

// thinkingLevel 解析配置中的默认思考级别。
func thinkingLevel(value string) (genai.ThinkingLevel, error) {
	switch strings.ToLower(value) {
	case "":
		return genai.ThinkingLevelUnspecified, nil
	case "low":
		return genai.ThinkingLevelLow, nil
	case "medium":
		return genai.ThinkingLevelMedium, nil
	case "high":
		return genai.ThinkingLevelHigh, nil
	default:
		return "", fmt.Errorf("Gemini thinking_level 无效: %q", value)
	}
}
