package gemini

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"google.golang.org/genai"
)

func TestToGeminiContentsMergesToolResponses(t *testing.T) {
	assistantText := "准备调用工具"
	toolOne := `{"temperature": 21}`
	toolTwo := "Error: 工具超时"
	messages := []openaiMessage{
		{Role: "system", Content: stringPtr("你是助手")},
		{Role: "user", Content: stringPtr("现在天气如何？")},
		{
			Role:    "assistant",
			Content: &assistantText,
			ToolCalls: []openaiToolCall{{
				ID:               "call/one",
				Function:         openaiCallArgs{Name: "weather", Arguments: `{"city":"Hong Kong"}`},
				ThoughtSignature: base64.StdEncoding.EncodeToString([]byte("signature")),
			}},
		},
		{Role: "tool", Name: "weather", ToolCallID: "call/one", Content: &toolOne},
		{Role: "tool", Name: "weather", ToolCallID: "call/two", Content: &toolTwo},
	}

	contents, err := toGeminiContents(messages)
	if err != nil {
		t.Fatalf("toGeminiContents() error = %v", err)
	}
	if got := len(contents); got != 3 {
		t.Fatalf("content count = %d, want 3", got)
	}
	if got := contents[1].Parts[1].FunctionCall.ID; got != "call_one" {
		t.Fatalf("normalized call ID = %q", got)
	}
	if got := string(contents[1].Parts[1].ThoughtSignature); got != "signature" {
		t.Fatalf("thought signature = %q", got)
	}
	if got := len(contents[2].Parts); got != 2 {
		t.Fatalf("merged tool response count = %d, want 2", got)
	}
	if got := contents[2].Parts[0].FunctionResponse.Response["temperature"]; got != float64(21) {
		t.Fatalf("JSON tool response = %#v", got)
	}
	if got := contents[2].Parts[1].FunctionResponse.Response["error"]; got != toolTwo {
		t.Fatalf("error tool response = %#v", got)
	}
}

func TestToGeminiContentsRejectsInvalidToolArguments(t *testing.T) {
	_, err := toGeminiContents([]openaiMessage{{
		Role: "assistant",
		ToolCalls: []openaiToolCall{{
			Function: openaiCallArgs{Name: "weather", Arguments: "{"},
		}},
	}})
	if err == nil {
		t.Fatal("invalid tool arguments should return an error")
	}
}

func TestToGeminiContentsResolvesToolNameAndDeveloperRole(t *testing.T) {
	messages := []openaiMessage{
		{Role: "developer", Content: "Follow instructions"},
		{Role: "assistant", ToolCalls: []openaiToolCall{{
			ID: "call_1", Function: openaiCallArgs{Name: "lookup", Arguments: "{}"},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: "found"},
	}
	if system, err := extractSystemPrompt(messages); err != nil || system != "Follow instructions" {
		t.Fatalf("system instruction = %q", system)
	}
	contents, err := toGeminiContents(messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 2 || contents[1].Parts[0].FunctionResponse.Name != "lookup" {
		t.Fatalf("contents = %#v", contents)
	}
}

func TestToGeminiContentsRejectsUnknownToolCall(t *testing.T) {
	_, err := toGeminiContents([]openaiMessage{{Role: "tool", ToolCallID: "missing", Content: "found"}})
	if err == nil {
		t.Fatal("tool message without matching call should fail")
	}
}

func TestToGeminiContentsRejectsUnsupportedImageURL(t *testing.T) {
	messages := []openaiMessage{{Role: "user", Content: []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/image.png"}},
	}}}
	_, err := toGeminiContents(messages)
	if err == nil {
		t.Fatal("remote image URL should fail instead of becoming empty content")
	}
}

func TestFromGeminiResponse(t *testing.T) {
	text := "你好"
	response := &genai.GenerateContentResponse{
		ModelVersion: "gemini-test",
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{Parts: []*genai.Part{{Text: text}, {
				FunctionCall:     &genai.FunctionCall{Name: "weather", Args: map[string]any{"city": "Hong Kong"}},
				ThoughtSignature: []byte("signature"),
			}}},
			FinishReason: genai.FinishReasonStop,
		}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     10,
			CandidatesTokenCount: 4,
			ThoughtsTokenCount:   2,
			TotalTokenCount:      16,
		},
	}

	converted := fromGeminiResponse(response, "fallback")
	if converted.Model != "gemini-test" {
		t.Fatalf("model = %q", converted.Model)
	}
	if got := *converted.Choices[0].Message.Content; got != text {
		t.Fatalf("content = %q", got)
	}
	if converted.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q", converted.Choices[0].FinishReason)
	}
	if converted.Choices[0].Message.ToolCalls[0].ID == "" {
		t.Fatal("tool call ID must be present")
	}
	if got := converted.Choices[0].Message.ToolCalls[0].ThoughtSignature; got != base64.StdEncoding.EncodeToString([]byte("signature")) {
		t.Fatalf("thought signature = %q", got)
	}
	if converted.Usage.CompletionTokens != 4 || converted.Usage.CompletionTokensDetails.ReasoningTokens != 2 {
		t.Fatalf("usage = %#v", converted.Usage)
	}
}

func TestFromGeminiBlockedResponse(t *testing.T) {
	response := &genai.GenerateContentResponse{
		PromptFeedback: &genai.GenerateContentResponsePromptFeedback{BlockReason: genai.BlockedReasonSafety},
	}
	converted := fromGeminiResponse(response, "gemini-test")
	if converted.Choices[0].FinishReason != "content_filter" {
		t.Fatalf("finish reason = %q", converted.Choices[0].FinishReason)
	}
}

func TestChatCompletionJSONUsesOpenAIFields(t *testing.T) {
	content := "ok"
	data, err := json.Marshal(chatCompletion{
		ID:      "chatcmpl-test",
		Choices: []chatChoice{{Message: chatMessage{Role: "assistant", Content: &content}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" {
		t.Fatal("empty JSON")
	}
}

func TestToGeminiContentsMultimodal(t *testing.T) {
	pngBase64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	messages := []openaiMessage{
		{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "请看这张图片"},
				map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": "data:image/png;base64," + pngBase64,
					},
				},
			},
		},
	}

	contents, err := toGeminiContents(messages)
	if err != nil {
		t.Fatalf("toGeminiContents() error = %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("contents count = %d, want 1", len(contents))
	}
	if len(contents[0].Parts) != 2 {
		t.Fatalf("parts count = %d, want 2", len(contents[0].Parts))
	}
	if contents[0].Parts[0].Text != "请看这张图片" {
		t.Fatalf("part 0 text = %q", contents[0].Parts[0].Text)
	}
	blob := contents[0].Parts[1].InlineData
	if blob == nil || blob.MIMEType != "image/png" || len(blob.Data) == 0 {
		t.Fatalf("part 1 blob = %#v", blob)
	}
}

func TestToGeminiToolConfig(t *testing.T) {
	// "none"
	cfg, err := toGeminiToolConfig("none")
	if err != nil || cfg == nil || cfg.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeNone {
		t.Fatalf("none config = %#v", cfg)
	}

	// "auto"
	cfg, err = toGeminiToolConfig("auto")
	if err != nil || cfg == nil || cfg.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeAuto {
		t.Fatalf("auto config = %#v", cfg)
	}

	// "required"
	cfg, err = toGeminiToolConfig("required")
	if err != nil || cfg == nil || cfg.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeAny {
		t.Fatalf("required config = %#v", cfg)
	}

	// specific function
	cfg, err = toGeminiToolConfig(map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "lookup",
		},
	})
	if err != nil || cfg == nil || cfg.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeAny || len(cfg.FunctionCallingConfig.AllowedFunctionNames) != 1 || cfg.FunctionCallingConfig.AllowedFunctionNames[0] != "lookup" {
		t.Fatalf("specific function config = %#v", cfg)
	}
}

func TestUnsupportedToolOptionsFail(t *testing.T) {
	if _, err := toGeminiToolConfig("invalid"); err == nil {
		t.Fatal("unknown tool_choice should fail")
	}
	if _, err := toGeminiTools([]openaiTool{{Type: "custom"}}); err == nil {
		t.Fatal("custom tool should fail")
	}
	if err := applyResponseFormat(&genai.GenerateContentConfig{}, &openaiResponseFormat{Type: "invalid"}); err == nil {
		t.Fatal("unknown response_format should fail")
	}
}

func TestMapReasoningEffort(t *testing.T) {
	if got := mapReasoningEffort("low"); got != genai.ThinkingLevelLow {
		t.Fatalf("mapReasoningEffort(low) = %v", got)
	}
	if got := mapReasoningEffort("medium"); got != genai.ThinkingLevelMedium {
		t.Fatalf("mapReasoningEffort(medium) = %v", got)
	}
	if got := mapReasoningEffort("high"); got != genai.ThinkingLevelHigh {
		t.Fatalf("mapReasoningEffort(high) = %v", got)
	}
	if got := mapReasoningEffort("unknown"); got != genai.ThinkingLevelUnspecified {
		t.Fatalf("mapReasoningEffort(unknown) = %v", got)
	}
}

func TestApplyResponseFormat(t *testing.T) {
	// json_object
	cfg := &genai.GenerateContentConfig{}
	if err := applyResponseFormat(cfg, &openaiResponseFormat{Type: "json_object"}); err != nil {
		t.Fatal(err)
	}
	if cfg.ResponseMIMEType != "application/json" {
		t.Fatalf("response mime type = %q", cfg.ResponseMIMEType)
	}

	// json_schema
	cfg = &genai.GenerateContentConfig{}
	if err := applyResponseFormat(cfg, &openaiResponseFormat{
		Type: "json_schema",
		JSONSchema: &openaiJSONSchemaParam{
			Name: "person",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.ResponseMIMEType != "application/json" || cfg.ResponseSchema == nil || cfg.ResponseSchema.Type != genai.TypeObject {
		t.Fatalf("response schema config = %#v", cfg)
	}
}

func TestAssistantMessageContents(t *testing.T) {
	content := "最终答案"
	messages := []openaiMessage{
		{
			Role:    "assistant",
			Content: &content,
		},
	}

	contents, err := toGeminiContents(messages)
	if err != nil {
		t.Fatalf("toGeminiContents() error = %v", err)
	}
	if len(contents) != 1 || len(contents[0].Parts) != 1 {
		t.Fatalf("parts len = %d, want 1", len(contents[0].Parts))
	}
	if contents[0].Parts[0].Text != content {
		t.Fatalf("part 0 text = %q", contents[0].Parts[0].Text)
	}
}

func TestModelAliases(t *testing.T) {
	server, err := NewServer(nil, ServerOptions{DefaultModel: "gemini-3.7-flash", ModelAliases: map[string]string{"gpt-4o": "gemini-2.5-flash"}})
	if err != nil {
		t.Fatal(err)
	}
	if model := server.resolveModel("gpt-4o"); model != "gemini-2.5-flash" {
		t.Fatalf("resolved model = %q", model)
	}
	if model := server.resolveModel(""); model != "gemini-3.7-flash" {
		t.Fatalf("default model = %q", model)
	}
	server.model = "gpt-4o"
	if model := server.resolveModel(""); model != "gemini-2.5-flash" {
		t.Fatalf("aliased default model = %q", model)
	}
}

func TestBuildThinkingConfig(t *testing.T) {
	// default: 默认开启 IncludeThoughts: true，但 ThinkingLevel 保持未指定（走服务端自适应）
	cfg := buildThinkingConfig("gemini-3.7-flash", "", genai.ThinkingLevelUnspecified)
	if cfg == nil || !cfg.IncludeThoughts || (cfg.ThinkingLevel != "" && cfg.ThinkingLevel != genai.ThinkingLevelUnspecified) {
		t.Fatalf("gemini-3 default thinking config = %#v", cfg)
	}

	// low reasoning effort
	cfg = buildThinkingConfig("gemini-2.5-flash", "low", genai.ThinkingLevelUnspecified)
	if cfg == nil || !cfg.IncludeThoughts || cfg.ThinkingLevel != genai.ThinkingLevelLow {
		t.Fatalf("low thinking config = %#v", cfg)
	}

	// off / none
	cfg = buildThinkingConfig("gemini-3.7-flash", "none", genai.ThinkingLevelHigh)
	if cfg == nil || cfg.IncludeThoughts || cfg.ThinkingBudget == nil || *cfg.ThinkingBudget != 0 {
		t.Fatalf("none thinking config = %#v", cfg)
	}
}

func stringPtr(value string) *string {
	return &value
}
