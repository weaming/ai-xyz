package gemini

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/weaming/ai-xyz/ai-gateway/convert"
	"github.com/weaming/ai-xyz/ai-gateway/ir"
	"google.golang.org/genai"
)

// codec 是 GenerateContent 协议与 IR 的双向编解码器。
type codec struct {
	defaultThinking genai.ThinkingLevel
}

// Protocol 返回 generate_content 协议标识。
func (codec) Protocol() convert.Protocol { return convert.ProtocolGenerateContent }

// ParseRequest 把 generateContent 请求体解析成 IR。
func (c codec) ParseRequest(body []byte, options convert.Options) (*ir.Request, error) {
	var native nativeRequest
	if err := json.Unmarshal(body, &native); err != nil {
		return nil, fmt.Errorf("解析 generateContent 请求: %w", err)
	}
	if len(native.Contents) == 0 {
		return nil, fmt.Errorf("generateContent 请求缺少 contents")
	}
	request := &ir.Request{}
	if native.SystemInstruction != nil {
		text := textParts(native.SystemInstruction.Parts)
		if text != "" {
			request.Instructions = []ir.ContentPart{{Type: "text", Text: text}}
		}
	}
	items, err := contentsToItems(native.Contents)
	if err != nil {
		return nil, err
	}
	request.Input = items
	tools, err := declarationsToTools(native.Tools)
	if err != nil {
		return nil, err
	}
	request.Tools = tools
	request.ToolChoice = toolConfigToChoice(native.ToolConfig)
	if config := native.GenerationConfig; config != nil {
		if config.Temperature != nil {
			request.Temperature = roundFloat(*config.Temperature)
		}
		if config.TopP != nil {
			request.TopP = roundFloat(*config.TopP)
		}
		if config.MaxOutputTokens > 0 {
			value := int(config.MaxOutputTokens)
			request.MaxOutputTokens = &value
		}
		request.Stop = config.StopSequences
		request.Text = formatFromConfig(config)
		if config.ThinkingConfig != nil {
			request.Reasoning = &ir.ReasoningConfig{
				Effort:          effortFromLevel(config.ThinkingConfig.ThinkingLevel),
				IncludeThoughts: &config.ThinkingConfig.IncludeThoughts,
			}
		}
	}
	return request, nil
}

// WriteRequest 把 IR 请求编码成 generateContent 请求体。
func (c codec) WriteRequest(request *ir.Request, options convert.Options) ([]byte, error) {
	native, err := encodeRequest(request, c.defaultThinking)
	if err != nil {
		return nil, err
	}
	return json.Marshal(native)
}

// ParseResponse 把 generateContent 响应解析成 IR。
func (codec) ParseResponse(body []byte, options convert.Options) (*ir.Response, error) {
	var native genai.GenerateContentResponse
	if err := json.Unmarshal(body, &native); err != nil {
		return nil, fmt.Errorf("解析 generateContent 响应: %w", err)
	}
	return decodeResponse(&native, native.ModelVersion), nil
}

// WriteResponse 把 IR 响应编码成 generateContent 响应体。
func (codec) WriteResponse(response *ir.Response, options convert.Options) ([]byte, error) {
	parts := []any{}
	for _, item := range response.Output {
		converted, err := itemToParts(item)
		if err != nil {
			return nil, err
		}
		parts = append(parts, converted...)
	}
	target := map[string]any{
		"candidates": []any{map[string]any{
			"index":        0,
			"content":      map[string]any{"role": "model", "parts": parts},
			"finishReason": finishReasonFromStatus(response),
		}},
	}
	if response.Model != "" {
		target["modelVersion"] = response.Model
	}
	if response.Usage != nil {
		target["usageMetadata"] = nativeUsage(response.Usage)
	}
	return json.Marshal(target)
}

// ReadStream 把 generateContent SSE 解析成 IR 事件。
func (codec) ReadStream(reader io.Reader, options convert.Options, emit func(ir.StreamEvent) error) error {
	state := newChunkState()
	chunks := 0
	model := ""
	if err := convert.ReadSSE(reader, func(frame convert.SSEFrame) error {
		if frame.Data == "[DONE]" {
			return nil
		}
		var native genai.GenerateContentResponse
		if err := json.Unmarshal([]byte(frame.Data), &native); err != nil {
			return fmt.Errorf("解析 generateContent SSE: %w", err)
		}
		chunks++
		if native.ModelVersion != "" {
			model = native.ModelVersion
		}
		for _, event := range state.events(&native, model) {
			if err := emit(event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if chunks == 0 {
		return fmt.Errorf("generateContent SSE 没有数据")
	}
	for _, event := range state.finishEvents() {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

// NewStreamWriter 创建把 IR 事件写成 generateContent SSE 的写入器。
func (codec) NewStreamWriter(writer io.Writer, options convert.Options) convert.StreamWriter {
	return &streamWriter{sse: writer, acc: convert.NewAccumulator(), tools: map[int]string{}}
}

type streamWriter struct {
	sse     io.Writer
	acc     *convert.Accumulator
	started bool
	model   string
	tools   map[int]string
	usage   *ir.Usage
}

func (writer *streamWriter) Write(event ir.StreamEvent) error {
	if err := writer.acc.Apply(event); err != nil {
		return err
	}
	switch event.Type {
	case ir.EventCreated:
		writer.model = event.Model
		return writer.write(map[string]any{
			"modelVersion": event.Model,
			"candidates":   []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{}}}},
		})
	case ir.EventTextDelta:
		return writer.write(partChunk(map[string]any{"text": event.Delta}))
	case ir.EventReasoningDelta:
		return writer.write(partChunk(map[string]any{"text": event.Delta, "thought": true}))
	case ir.EventToolCallStart:
		writer.tools[event.OutputIndex] = event.ToolName
		return nil
	case ir.EventToolArgumentsDone:
		call := map[string]any{"name": event.ToolName, "args": rawToAny(json.RawMessage(event.Arguments))}
		if event.ToolName == "" {
			call["name"] = writer.tools[event.OutputIndex]
		}
		return writer.write(partChunk(map[string]any{"functionCall": call}))
	case ir.EventUsage:
		writer.usage = event.Usage
	}
	return nil
}

func (writer *streamWriter) Close() (*ir.Response, error) {
	response, err := writer.acc.Response()
	if err != nil {
		return nil, err
	}
	chunk := map[string]any{"candidates": []any{map[string]any{"finishReason": finishReasonFromStatus(response)}}}
	if response.Usage != nil {
		chunk["usageMetadata"] = nativeUsage(response.Usage)
	}
	if err := writer.write(chunk); err != nil {
		return nil, err
	}
	return response, nil
}

func (writer *streamWriter) write(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := convert.WriteSSE(writer.sse, convert.SSEFrame{Data: string(data)}); err != nil {
		return err
	}
	if flusher, ok := writer.sse.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	return nil
}

func partChunk(part map[string]any) map[string]any {
	return map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{part}},
		}},
	}
}

func contentsToItems(contents []*genai.Content) ([]ir.Item, error) {
	items := []ir.Item{}
	for _, content := range contents {
		if content == nil {
			continue
		}
		if content.Role == genai.RoleModel {
			for _, part := range content.Parts {
				switch {
				case part == nil:
					continue
				case part.FunctionCall != nil:
					items = append(items, functionCallItem(part))
				case part.Text != "":
					items = append(items, ir.Item{Type: "message", Role: "assistant", Content: []ir.ContentPart{{Type: "output_text", Text: part.Text}}})
				}
			}
			continue
		}
		for _, part := range content.Parts {
			switch {
			case part == nil:
				continue
			case part.FunctionResponse != nil:
				items = append(items, ir.Item{
					Type:   "function_call_output",
					CallID: part.FunctionResponse.ID,
					Name:   part.FunctionResponse.Name,
					Output: mustRaw(part.FunctionResponse.Response),
				})
			case part.InlineData != nil:
				converted, err := inlineDataPart(part.InlineData)
				if err != nil {
					return nil, err
				}
				items = append(items, ir.Item{Type: "message", Role: "user", Content: []ir.ContentPart{converted}})
			case part.Text != "":
				items = append(items, ir.Item{Type: "message", Role: "user", Content: []ir.ContentPart{{Type: "text", Text: part.Text}}})
			}
		}
	}
	return items, nil
}

// normalizeSchema 把 Gemini 的 Schema JSON 还原成标准 JSON Schema（小写 type、去掉 propertyOrdering）。
func normalizeSchema(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			switch key {
			case "propertyOrdering":
				delete(typed, key)
			case "type":
				if name, ok := item.(string); ok {
					typed[key] = strings.ToLower(name)
				}
			default:
				typed[key] = normalizeSchema(item)
			}
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = normalizeSchema(item)
		}
		return typed
	default:
		return value
	}
}

func inlineDataPart(blob *genai.Blob) (ir.ContentPart, error) {
	if blob == nil {
		return ir.ContentPart{}, fmt.Errorf("generateContent inlineData 缺少内容")
	}
	data := encodeBase64(blob.Data)
	if len(blob.MIMEType) >= 6 && blob.MIMEType[:6] == "image/" {
		return ir.ContentPart{Type: "input_image", ImageURL: fmt.Sprintf("data:%s;base64,%s", blob.MIMEType, data)}, nil
	}
	if len(blob.MIMEType) >= 6 && blob.MIMEType[:6] == "audio/" {
		return ir.ContentPart{Type: "input_audio", FileData: data, AudioFormat: blob.MIMEType[6:]}, nil
	}
	return ir.ContentPart{}, fmt.Errorf("generateContent 不支持的媒体类型 %q", blob.MIMEType)
}

func declarationsToTools(tools []*genai.Tool) ([]ir.Tool, error) {
	result := []ir.Tool{}
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		if len(tool.FunctionDeclarations) == 0 {
			return nil, fmt.Errorf("generateContent 暂不支持非 function 工具")
		}
		for _, declaration := range tool.FunctionDeclarations {
			converted := ir.Tool{Type: "function", Name: declaration.Name, Description: declaration.Description}
			if declaration.Parameters != nil {
				parameters, err := json.Marshal(declaration.Parameters)
				if err != nil {
					return nil, fmt.Errorf("编码 function %q 的 parameters: %w", declaration.Name, err)
				}
				converted.Parameters = mustRaw(normalizeSchema(rawToAny(parameters)))
			}
			result = append(result, converted)
		}
	}
	return result, nil
}

func toolConfigToChoice(config *genai.ToolConfig) json.RawMessage {
	if config == nil || config.FunctionCallingConfig == nil {
		return nil
	}
	calling := config.FunctionCallingConfig
	switch calling.Mode {
	case genai.FunctionCallingConfigModeNone:
		return mustRaw("none")
	case genai.FunctionCallingConfigModeAuto:
		return mustRaw("auto")
	case genai.FunctionCallingConfigModeAny:
		names := dedupeStrings(calling.AllowedFunctionNames)
		if len(names) == 1 {
			return mustRaw(map[string]any{"type": "function", "name": names[0]})
		}
		if len(names) > 1 {
			tools := make([]any, 0, len(names))
			for _, name := range names {
				tools = append(tools, map[string]any{"type": "function", "name": name})
			}
			return mustRaw(map[string]any{"type": "allowed_tools", "tools": tools, "mode": "required"})
		}
		return mustRaw("required")
	default:
		return nil
	}
}

func formatFromConfig(config *genai.GenerateContentConfig) *ir.TextConfig {
	if config.ResponseMIMEType == "" && config.ResponseSchema == nil {
		return nil
	}
	text := &ir.TextConfig{}
	switch {
	case config.ResponseSchema != nil:
		schema, err := json.Marshal(config.ResponseSchema)
		if err != nil {
			return text
		}
		text.Format = mustRaw(map[string]any{"type": "json_schema", "name": "response", "schema": rawToAny(schema)})
	case config.ResponseMIMEType == "application/json":
		text.Format = mustRaw(map[string]any{"type": "json_object"})
	case config.ResponseMIMEType == "text/plain":
		text.Format = mustRaw(map[string]any{"type": "text"})
	}
	return text
}

func effortFromLevel(level genai.ThinkingLevel) string {
	switch level {
	case genai.ThinkingLevelLow:
		return "low"
	case genai.ThinkingLevelMedium:
		return "medium"
	case genai.ThinkingLevelHigh:
		return "high"
	default:
		return ""
	}
}

func itemToParts(item ir.Item) ([]any, error) {
	switch item.Type {
	case "message":
		parts := make([]any, 0, len(item.Content))
		for _, part := range item.Content {
			if part.Type != "text" && part.Type != "output_text" {
				return nil, fmt.Errorf("generateContent 响应只支持文本 part，收到 %q", part.Type)
			}
			parts = append(parts, map[string]any{"text": part.Text})
		}
		return parts, nil
	case "reasoning":
		return []any{map[string]any{"text": joinTextParts(contentTexts(item.Content)), "thought": true}}, nil
	case "function_call":
		call := map[string]any{"name": item.Name, "args": decodeArgs(item.Arguments)}
		if item.CallID != "" {
			call["id"] = normalizeToolCallID(item.CallID)
		}
		part := map[string]any{"functionCall": call}
		if signature := rawString(item.Extensions["thought_signature"]); signature != "" {
			part["thoughtSignature"] = signature
		}
		return []any{part}, nil
	default:
		return nil, fmt.Errorf("generateContent 无法表达 item type %q", item.Type)
	}
}

func finishReasonFromStatus(response *ir.Response) string {
	switch response.FinishReason {
	case "length":
		return string(genai.FinishReasonMaxTokens)
	case "content_filter":
		return string(genai.FinishReasonSafety)
	case "tool_calls", "stop", "":
		return string(genai.FinishReasonStop)
	default:
		return string(genai.FinishReasonStop)
	}
}

// roundFloat 把 float32 转成 float64 并抹掉二进制误差。
func roundFloat(value float32) *float64 {
	converted := math.Round(float64(value)*1e6) / 1e6
	return &converted
}

func nativeUsage(usage *ir.Usage) map[string]any {
	return map[string]any{
		"promptTokenCount":        usage.InputTokens,
		"candidatesTokenCount":    usage.OutputTokens,
		"totalTokenCount":         usage.TotalTokens,
		"thoughtsTokenCount":      usage.ReasoningTokens,
		"cachedContentTokenCount": usage.CachedInputTokens,
	}
}
