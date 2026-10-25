package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// ---- OpenAI 请求结构 ----

type openaiMessage struct {
	Role             string           `json:"role"`
	Content          any              `json:"content"`
	ReasoningContent *string          `json:"reasoning_content,omitempty"`
	Thought          *string          `json:"thought,omitempty"`
	ThoughtSignature string           `json:"thought_signature,omitempty"`
	ToolCalls        []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	Name             string           `json:"name,omitempty"`
}

type openaiContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openaiImageURL `json:"image_url,omitempty"`
}

type openaiImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type openaiToolCall struct {
	ID                    string         `json:"id"`
	Type                  string         `json:"type"`
	Function              openaiCallArgs `json:"function"`
	ThoughtSignature      string         `json:"thought_signature,omitempty"`
	ThoughtSignatureCamel string         `json:"thoughtSignature,omitempty"`
}

type openaiCallArgs struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openaiTool struct {
	Type     string        `json:"type"`
	Function openaiFuncDef `json:"function"`
}

type openaiFuncDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type openaiResponseFormat struct {
	Type       string                 `json:"type"`
	JSONSchema *openaiJSONSchemaParam `json:"json_schema,omitempty"`
}

type openaiJSONSchemaParam struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Schema      any    `json:"schema"`
	Strict      *bool  `json:"strict,omitempty"`
}

type openaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatRequest struct {
	Model               string                `json:"model"`
	Messages            []openaiMessage       `json:"messages"`
	Tools               []openaiTool          `json:"tools"`
	ToolChoice          any                   `json:"tool_choice"`
	ResponseFormat      *openaiResponseFormat `json:"response_format,omitempty"`
	StreamOptions       *openaiStreamOptions  `json:"stream_options,omitempty"`
	ReasoningEffort     string                `json:"reasoning_effort"`
	Temperature         *float32              `json:"temperature,omitempty"`
	TopP                *float32              `json:"top_p,omitempty"`
	Stop                stopSequences         `json:"stop,omitempty"`
	MaxTokens           int                   `json:"max_tokens"`
	MaxCompletionTokens int                   `json:"max_completion_tokens"`
	Stream              bool                  `json:"stream"`
}

// ---- 请求转换 ----

// extractSystemPrompt 拼接 system 消息,由调用方单独传给 SystemInstruction。
func extractSystemPrompt(messages []openaiMessage) (string, error) {
	parts := make([]string, 0, 1)
	for _, m := range messages {
		if m.Role == "system" || m.Role == "developer" {
			text, _, err := extractMessageText(m.Content)
			if err != nil {
				return "", fmt.Errorf("%s 消息 content: %w", m.Role, err)
			}
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// toGeminiContents OpenAI messages → Gemini contents。
// assistant 的 functionCall 回传时原样保留 thoughtSignature(Gemini 3.x 必填)。
func toGeminiContents(messages []openaiMessage) ([]*genai.Content, error) {
	contents := make([]*genai.Content, 0, len(messages))
	callIDToName := make(map[string]string)
	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			continue
		case "assistant":
			var parts []*genai.Part
			text, hasText, err := extractMessageText(msg.Content)
			if err != nil {
				return nil, fmt.Errorf("assistant 消息 content: %w", err)
			}
			if hasText && text != "" {
				parts = append(parts, &genai.Part{Text: text})
			}
			for _, tc := range msg.ToolCalls {
				if tc.ID != "" {
					callIDToName[tc.ID] = tc.Function.Name
				}
				args := map[string]any{}
				if tc.Function.Arguments != "" {
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
						return nil, fmt.Errorf("assistant tool call %q 的 arguments 不是合法 JSON: %w", tc.Function.Name, err)
					}
				}
				call := &genai.FunctionCall{Name: tc.Function.Name, Args: args}
				if tc.ID != "" {
					call.ID = normalizeToolCallID(tc.ID)
				}
				signature, err := decodeThoughtSignature(tc.thoughtSignature())
				if err != nil {
					return nil, fmt.Errorf("tool call %q: %w", tc.ID, err)
				}
				part := &genai.Part{FunctionCall: call, ThoughtSignature: signature}
				parts = append(parts, part)
			}
			if len(parts) > 0 {
				contents = append(contents, &genai.Content{Role: genai.RoleModel, Parts: parts})
			}
		case "tool":
			text, _, err := extractMessageText(msg.Content)
			if err != nil {
				return nil, fmt.Errorf("tool 消息 content: %w", err)
			}
			name := msg.Name
			if name == "" {
				name = callIDToName[msg.ToolCallID]
			}
			if name == "" {
				return nil, fmt.Errorf("tool 消息的 tool_call_id %q 找不到对应 function name", msg.ToolCallID)
			}
			part := &genai.Part{FunctionResponse: &genai.FunctionResponse{
				Name:     name,
				Response: toolOutputToResponse(text),
			}}
			if msg.ToolCallID != "" {
				part.FunctionResponse.ID = normalizeToolCallID(msg.ToolCallID)
			}
			// 多条并行工具结果合并到同一条 user 消息
			last := lastContent(contents)
			if last != nil && last.Role == genai.RoleUser && hasFunctionResponse(last) {
				last.Parts = append(last.Parts, part)
			} else {
				contents = append(contents, &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{part}})
			}
		case "user":
			parts, err := extractUserParts(msg.Content)
			if err != nil {
				return nil, fmt.Errorf("user 消息 content: %w", err)
			}
			if len(parts) > 0 {
				contents = append(contents, &genai.Content{Role: genai.RoleUser, Parts: parts})
			}
		default:
			return nil, fmt.Errorf("不支持 Chat 消息 role %q", msg.Role)
		}
	}
	return contents, nil
}

func extractMessageText(content any) (string, bool, error) {
	if content == nil {
		return "", false, nil
	}
	switch v := content.(type) {
	case string:
		return v, true, nil
	case *string:
		if v != nil {
			return *v, true, nil
		}
		return "", false, nil
	case []any:
		var sb strings.Builder
		for index, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				return "", false, fmt.Errorf("content[%d] 必须是对象", index)
			}
			partType, _ := part["type"].(string)
			if partType != "text" && partType != "input_text" && partType != "output_text" {
				return "", false, fmt.Errorf("不支持 content[%d].type %q", index, partType)
			}
			text, ok := part["text"].(string)
			if !ok {
				return "", false, fmt.Errorf("content[%d].text 必须是字符串", index)
			}
			sb.WriteString(text)
		}
		return sb.String(), true, nil
	default:
		return "", false, fmt.Errorf("content 必须是字符串或文本数组")
	}
}

func extractUserParts(content any) ([]*genai.Part, error) {
	if content == nil {
		return []*genai.Part{{Text: ""}}, nil
	}
	switch v := content.(type) {
	case string:
		return []*genai.Part{{Text: v}}, nil
	case *string:
		if v != nil {
			return []*genai.Part{{Text: *v}}, nil
		}
		return []*genai.Part{{Text: ""}}, nil
	case []any:
		var parts []*genai.Part
		for index, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content[%d] 必须是对象", index)
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text", "input_text":
				text, ok := m["text"].(string)
				if !ok {
					return nil, fmt.Errorf("content[%d].text 必须是字符串", index)
				}
				parts = append(parts, &genai.Part{Text: text})
			case "image_url", "input_image":
				urlStr, ok := m["image_url"].(string)
				if !ok {
					imageObj, isObject := m["image_url"].(map[string]any)
					if isObject {
						urlStr, ok = imageObj["url"].(string)
					}
				}
				if !ok {
					return nil, fmt.Errorf("content[%d].image_url 缺少 URL", index)
				}
				part, err := parseDataURLPart(urlStr)
				if err != nil {
					return nil, fmt.Errorf("content[%d].image_url: %w", index, err)
				}
				parts = append(parts, part)
			default:
				return nil, fmt.Errorf("不支持 content[%d].type %q", index, typ)
			}
		}
		if len(parts) == 0 {
			return []*genai.Part{{Text: ""}}, nil
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("content 必须是字符串或内容数组")
	}
}

func parseDataURLPart(urlStr string) (*genai.Part, error) {
	const dataPrefix = "data:"
	if !strings.HasPrefix(urlStr, dataPrefix) {
		return nil, fmt.Errorf("仅支持 base64 data URL")
	}
	commaIdx := strings.Index(urlStr, ",")
	if commaIdx == -1 {
		return nil, fmt.Errorf("data URL 缺少数据分隔符")
	}
	header := urlStr[len(dataPrefix):commaIdx]
	dataStr := urlStr[commaIdx+1:]
	isBase64 := strings.HasSuffix(header, ";base64")
	mimeType := header
	if isBase64 {
		mimeType = strings.TrimSuffix(header, ";base64")
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	if !isBase64 {
		return nil, fmt.Errorf("data URL 必须使用 base64 编码")
	}
	data, err := base64.StdEncoding.DecodeString(dataStr)
	if err != nil {
		return nil, fmt.Errorf("解码 data URL: %w", err)
	}
	return &genai.Part{
		InlineData: &genai.Blob{
			MIMEType: mimeType,
			Data:     data,
		},
	}, nil
}

func lastContent(contents []*genai.Content) *genai.Content {
	if len(contents) == 0 {
		return nil
	}
	return contents[len(contents)-1]
}

func hasFunctionResponse(content *genai.Content) bool {
	for _, part := range content.Parts {
		if part.FunctionResponse != nil {
			return true
		}
	}
	return false
}

// toolOutputToResponse 语义:JSON object 原样、Error: 开头标记失败、其余文本作为成功输出。
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

// applyResponseFormat 将 OpenAI response_format 映射到 Gemini 生成配置。
func applyResponseFormat(config *genai.GenerateContentConfig, format *openaiResponseFormat) error {
	if format == nil {
		return nil
	}
	switch strings.ToLower(format.Type) {
	case "json_object":
		config.ResponseMIMEType = "application/json"
	case "json_schema":
		if format.JSONSchema == nil || format.JSONSchema.Schema == nil {
			return fmt.Errorf("response_format.json_schema 缺少 schema")
		}
		config.ResponseMIMEType = "application/json"
		schema, ok := toGenaiSchema(format.JSONSchema.Schema)
		if !ok {
			return fmt.Errorf("response_format.json_schema 无法转换")
		}
		config.ResponseSchema = schema
	case "text":
		config.ResponseMIMEType = "text/plain"
	default:
		return fmt.Errorf("不支持 response_format.type %q", format.Type)
	}
	return nil
}

// toGeminiToolConfig 将 OpenAI tool_choice 转换为 Gemini ToolConfig。
func toGeminiToolConfig(toolChoice any) (*genai.ToolConfig, error) {
	if toolChoice == nil {
		return nil, nil
	}
	switch v := toolChoice.(type) {
	case string:
		switch strings.ToLower(v) {
		case "none":
			return &genai.ToolConfig{
				FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeNone},
			}, nil
		case "auto":
			return &genai.ToolConfig{
				FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto},
			}, nil
		case "required":
			return &genai.ToolConfig{
				FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny},
			}, nil
		}
	case map[string]any:
		if typ, _ := v["type"].(string); typ == "function" {
			if fn, ok := v["function"].(map[string]any); ok {
				if name, _ := fn["name"].(string); name != "" {
					return &genai.ToolConfig{
						FunctionCallingConfig: &genai.FunctionCallingConfig{
							Mode:                 genai.FunctionCallingConfigModeAny,
							AllowedFunctionNames: []string{name},
						},
					}, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("不支持 tool_choice: %v", toolChoice)
}

// toGeminiTools OpenAI tools → Gemini functionDeclarations。
func toGeminiTools(tools []openaiTool) ([]*genai.Tool, error) {
	var declarations []*genai.FunctionDeclaration
	for _, t := range tools {
		if t.Type != "function" || t.Function.Name == "" {
			return nil, fmt.Errorf("仅支持命名的 function tool，收到 type %q name %q", t.Type, t.Function.Name)
		}
		decl := &genai.FunctionDeclaration{Name: t.Function.Name, Description: t.Function.Description}
		if t.Function.Parameters != nil {
			params, ok := toGenaiSchema(t.Function.Parameters)
			if !ok {
				return nil, fmt.Errorf("function tool %q 的 parameters 无法转换", t.Function.Name)
			}
			decl.Parameters = params
		}
		declarations = append(declarations, decl)
	}
	if len(declarations) == 0 {
		return nil, nil
	}
	return []*genai.Tool{{FunctionDeclarations: declarations}}, nil
}

// normalizeToolCallID Gemini 3 要求 tool call id 仅含 [a-zA-Z0-9_-] 且不超过 64 字符。
func normalizeToolCallID(id string) string {
	var b strings.Builder
	for _, c := range id {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteRune('_')
		}
	}
	s := b.String()
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

// decodeThoughtSignature 把历史消息里的 base64 签名解码为 []byte(Go SDK 内部以 bytes 表示)。
func decodeThoughtSignature(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("thought_signature 不是合法 base64: %w", err)
	}
	return b, nil
}

// ---- 响应结构 ----

type chatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *usageInfo   `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatMessage struct {
	Role             string           `json:"role"`
	Content          *string          `json:"content"`
	ReasoningContent *string          `json:"reasoning_content,omitempty"`
	ToolCalls        []openaiToolCall `json:"tool_calls,omitempty"`
}

type usageInfo struct {
	PromptTokens            int                   `json:"prompt_tokens"`
	CompletionTokens        int                   `json:"completion_tokens"`
	TotalTokens             int                   `json:"total_tokens"`
	PromptTokensDetails     *promptTokensDetails  `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *completionTokDetails `json:"completion_tokens_details,omitempty"`
}

type promptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type completionTokDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ---- 响应转换 ----

// fromGeminiResponse Gemini generateContent 响应 → OpenAI Chat Completions 响应。
func fromGeminiResponse(resp *genai.GenerateContentResponse, model string) *chatCompletion {
	completion := &chatCompletion{
		ID:      requestID(),
		Object:  "chat.completion",
		Created: unixNow(),
		Model:   modelVersion(resp, model),
	}
	candidate := firstCandidate(resp)
	if candidate == nil {
		finishReason := "stop"
		if isPromptBlocked(resp) {
			finishReason = "content_filter"
		}
		completion.Choices = []chatChoice{{Index: 0, FinishReason: finishReason}}
		return completion
	}

	var parts []*genai.Part
	if candidate.Content != nil {
		parts = candidate.Content.Parts
	}
	message, toolCalls := partsToMessage(parts)
	completion.Choices = []chatChoice{{
		Index:        0,
		Message:      message,
		FinishReason: mapFinishReason(candidate.FinishReason, len(toolCalls) > 0),
	}}
	completion.Usage = toUsageInfo(resp.UsageMetadata)
	return completion
}

// partsToMessage 从响应 parts 提取 OpenAI 消息(文本 + tool_calls)。
func partsToMessage(parts []*genai.Part) (chatMessage, []openaiToolCall) {
	var textParts []string
	var reasoningParts []string
	var toolCalls []openaiToolCall
	for _, part := range parts {
		if part == nil {
			continue
		}
		switch {
		case part.Text != "" && part.Thought:
			reasoningParts = append(reasoningParts, part.Text)
		case part.Text != "":
			textParts = append(textParts, part.Text)
		case part.FunctionCall != nil:
			call := part.FunctionCall
			callID := call.ID
			if callID == "" {
				callID = newID("call_")
			}
			argsJSON, err := json.Marshal(call.Args)
			if err != nil {
				argsJSON = []byte("{}")
			}
			toolCalls = append(toolCalls, openaiToolCall{
				ID:               callID,
				Type:             "function",
				Function:         openaiCallArgs{Name: call.Name, Arguments: string(argsJSON)},
				ThoughtSignature: encodeThoughtSignature(part.ThoughtSignature),
			})
		}
	}
	var content *string
	if len(textParts) > 0 {
		text := strings.Join(textParts, "")
		content = &text
	}
	message := chatMessage{Role: "assistant", Content: content}
	if len(reasoningParts) > 0 {
		reasoning := strings.Join(reasoningParts, "")
		message.ReasoningContent = &reasoning
	}
	if len(toolCalls) > 0 {
		message.ToolCalls = toolCalls
	}
	return message, toolCalls
}

// mapFinishReason Gemini finishReason → OpenAI finish_reason。
func mapFinishReason(reason genai.FinishReason, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	switch reason {
	case genai.FinishReasonMaxTokens:
		return "length"
	case genai.FinishReasonSafety, genai.FinishReasonRecitation, genai.FinishReasonBlocklist,
		genai.FinishReasonProhibitedContent, genai.FinishReasonSPII, genai.FinishReasonImageSafety,
		genai.FinishReasonLanguage, genai.FinishReasonImageProhibitedContent, genai.FinishReasonImageRecitation,
		genai.FinishReasonImageOther:
		return "content_filter"
	default:
		return "stop"
	}
}

// toUsageInfo Gemini usageMetadata → OpenAI usage 格式。
func toUsageInfo(usage *genai.GenerateContentResponseUsageMetadata) *usageInfo {
	if usage == nil {
		return nil
	}
	return &usageInfo{
		PromptTokens:            int(usage.PromptTokenCount),
		CompletionTokens:        int(usage.CandidatesTokenCount),
		TotalTokens:             int(usage.TotalTokenCount),
		PromptTokensDetails:     &promptTokensDetails{CachedTokens: int(usage.CachedContentTokenCount)},
		CompletionTokensDetails: &completionTokDetails{ReasoningTokens: int(usage.ThoughtsTokenCount)},
	}
}

func firstCandidate(resp *genai.GenerateContentResponse) *genai.Candidate {
	if resp == nil || len(resp.Candidates) == 0 {
		return nil
	}
	return resp.Candidates[0]
}

func isPromptBlocked(resp *genai.GenerateContentResponse) bool {
	return resp != nil && resp.PromptFeedback != nil &&
		resp.PromptFeedback.BlockReason != "" &&
		resp.PromptFeedback.BlockReason != genai.BlockedReasonUnspecified
}

func modelVersion(resp *genai.GenerateContentResponse, fallback string) string {
	if resp != nil && resp.ModelVersion != "" {
		return resp.ModelVersion
	}
	return fallback
}

func (call openaiToolCall) thoughtSignature() string {
	if call.ThoughtSignature != "" {
		return call.ThoughtSignature
	}
	return call.ThoughtSignatureCamel
}

func encodeThoughtSignature(sig []byte) string {
	if len(sig) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(sig)
}
