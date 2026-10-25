package gemini

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/weaming/ai-xyz/ai-gateway/convert"
	"github.com/weaming/ai-xyz/ai-gateway/ir"
	"google.golang.org/genai"
)

func toolRequestItems() []ir.Item {
	return []ir.Item{
		{Type: "message", Role: "user", Content: []ir.ContentPart{{Type: "text", Text: "hi"}}},
		{Type: "function_call", CallID: "call_1", Name: "lookup", Arguments: `{"q":"x"}`},
		{Type: "function_call", CallID: "call_2", Name: "other", Arguments: `{}`},
		{Type: "function_call_output", CallID: "call_1", Output: json.RawMessage(`"ok"`)},
		{Type: "function_call_output", CallID: "call_2", Name: "other", Output: json.RawMessage(`"done"`)},
	}
}

func TestEncodeRequestMergesToolResponses(t *testing.T) {
	native, err := encodeRequest(&ir.Request{Input: toolRequestItems()}, genai.ThinkingLevelUnspecified)
	if err != nil {
		t.Fatal(err)
	}
	if len(native.Contents) != 3 {
		t.Fatalf("contents = %d", len(native.Contents))
	}
	model := native.Contents[1]
	if model.Role != genai.RoleModel || len(model.Parts) != 2 {
		t.Fatalf("model content = %#v", model)
	}
	last := native.Contents[2]
	if last.Role != genai.RoleUser || len(last.Parts) != 2 {
		t.Fatalf("合并后的 tool 结果应为一条 user content: %#v", last)
	}
	if last.Parts[0].FunctionResponse.Name != "lookup" {
		t.Fatalf("call_id 应解析成 function name: %#v", last.Parts[0].FunctionResponse)
	}
	if last.Parts[1].FunctionResponse.Response["output"] != "done" {
		t.Fatalf("function response = %#v", last.Parts[1].FunctionResponse.Response)
	}
}

func TestEncodeRequestKeepsThoughtSignature(t *testing.T) {
	signature := base64.StdEncoding.EncodeToString([]byte("sig"))
	request := &ir.Request{Input: []ir.Item{
		{Type: "message", Role: "user", Content: []ir.ContentPart{{Type: "text", Text: "hi"}}},
		{Type: "function_call", CallID: "call_1", Name: "lookup", Arguments: "{}", Extensions: map[string]json.RawMessage{"thought_signature": json.RawMessage(`"` + signature + `"`)}},
	}}
	native, err := encodeRequest(request, genai.ThinkingLevelUnspecified)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(native.Contents[1].Parts[0].ThoughtSignature); got != "sig" {
		t.Fatalf("thought signature = %q", got)
	}
}

func TestEncodeRequestRejectsUnsupported(t *testing.T) {
	cases := map[string]*ir.Request{
		`收到 "custom"`:         {Input: toolRequestItems(), Tools: []ir.Tool{{Type: "custom", Name: "grammar"}}},
		"custom_tool_call":    {Input: []ir.Item{{Type: "custom_tool_call", CallID: "c", Name: "grammar", Input: "x"}}},
		"不支持请求字段":             {Input: toolRequestItems(), Unknown: map[string]json.RawMessage{"foo": json.RawMessage(`1`)}},
		"仅支持 base64 data URL": {Input: []ir.Item{{Type: "message", Role: "user", Content: []ir.ContentPart{{Type: "input_image", ImageURL: "https://example.com/a.png"}}}}},
		`收到 "mystery_tool"`:   {Input: toolRequestItems(), Tools: []ir.Tool{{Type: "mystery_tool"}}},
	}
	for expected, request := range cases {
		if _, err := encodeRequest(request, genai.ThinkingLevelUnspecified); err == nil || !strings.Contains(err.Error(), expected) {
			t.Fatalf("%s: err = %v", expected, err)
		}
	}
}

func TestEncodeRequestDataURLImage(t *testing.T) {
	request := &ir.Request{Input: []ir.Item{{Type: "message", Role: "user", Content: []ir.ContentPart{
		{Type: "text", Text: "what"},
		{Type: "input_image", ImageURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("png"))},
	}}}}
	native, err := encodeRequest(request, genai.ThinkingLevelUnspecified)
	if err != nil {
		t.Fatal(err)
	}
	blob := native.Contents[0].Parts[1].InlineData
	if blob == nil || blob.MIMEType != "image/png" || string(blob.Data) != "png" {
		t.Fatalf("inline data = %#v", blob)
	}
}

// TestEncodeRequestIgnoresTuningFields 覆盖调参/元数据字段被忽略而不是报错。
// TestEncodeRequestDropsBuiltInToolsAndItems 覆盖内置工具与内置 item 被跳过。
func TestEncodeRequestDropsBuiltInToolsAndItems(t *testing.T) {
	request := &ir.Request{
		Input: []ir.Item{
			{Type: "message", Role: "user", Content: []ir.ContentPart{{Type: "text", Text: "hi"}}},
			{Type: "web_search_call", Raw: json.RawMessage(`{"type":"web_search_call","id":"ws_1"}`)},
		},
		Tools: []ir.Tool{{Type: "web_search"}, {Type: "function", Name: "f"}},
	}
	native, err := encodeRequest(request, genai.ThinkingLevelUnspecified)
	if err != nil {
		t.Fatalf("内置工具/item 应被跳过: %v", err)
	}
	if len(native.Tools) != 1 || len(native.Tools[0].FunctionDeclarations) != 1 || native.Tools[0].FunctionDeclarations[0].Name != "f" {
		t.Fatalf("tools = %#v", native.Tools)
	}
	if len(native.Contents) != 1 {
		t.Fatalf("contents = %d", len(native.Contents))
	}
}

func TestEncodeRequestIgnoresTuningFields(t *testing.T) {
	store := true
	parallel := true
	request := &ir.Request{
		Input:             toolRequestItems(),
		Store:             &store,
		ParallelToolCalls: &parallel,
		ServiceTier:       "scale",
		Metadata:          json.RawMessage(`{"k":"v"}`),
		Text:              &ir.TextConfig{Verbosity: "high"},
		Reasoning:         &ir.ReasoningConfig{Effort: "medium", Summary: "auto"},
		Dropped:           []string{"include", "prompt_cache_key"},
	}
	native, err := encodeRequest(request, genai.ThinkingLevelUnspecified)
	if err != nil {
		t.Fatalf("调参字段应被忽略: %v", err)
	}
	if native.GenerationConfig == nil || native.GenerationConfig.ThinkingConfig == nil || native.GenerationConfig.ThinkingConfig.ThinkingLevel != genai.ThinkingLevelMedium {
		t.Fatalf("thinking config = %#v", native.GenerationConfig)
	}
}

func TestBuildThinkingConfig(t *testing.T) {
	disabled := buildThinkingConfig(&ir.ReasoningConfig{Effort: "none"}, genai.ThinkingLevelUnspecified)
	if disabled.IncludeThoughts || disabled.ThinkingBudget == nil || *disabled.ThinkingBudget != 0 {
		t.Fatalf("关闭思考失败: %#v", disabled)
	}
	high := buildThinkingConfig(&ir.ReasoningConfig{Effort: "high"}, genai.ThinkingLevelUnspecified)
	if high.ThinkingLevel != genai.ThinkingLevelHigh || !high.IncludeThoughts {
		t.Fatalf("high = %#v", high)
	}
	fallback := buildThinkingConfig(nil, genai.ThinkingLevelMedium)
	if fallback.ThinkingLevel != genai.ThinkingLevelMedium || !fallback.IncludeThoughts {
		t.Fatalf("默认级别 = %#v", fallback)
	}
	explicit := buildThinkingConfig(&ir.ReasoningConfig{Effort: "low", IncludeThoughts: boolPointer(false)}, genai.ThinkingLevelUnspecified)
	if explicit.IncludeThoughts {
		t.Fatalf("includeThoughts 应遵循调用方: %#v", explicit)
	}
}

func TestDecodeResponseFinishMapping(t *testing.T) {
	cases := []struct {
		reason genai.FinishReason
		status string
		finish string
	}{
		{genai.FinishReasonStop, "completed", "stop"},
		{genai.FinishReasonMaxTokens, "incomplete", "length"},
		{genai.FinishReasonSafety, "incomplete", "content_filter"},
	}
	for _, item := range cases {
		response := &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "hi"}}},
			FinishReason: item.reason,
		}}}
		decoded := decodeResponse(response, "m")
		if decoded.Status != item.status || decoded.FinishReason != item.finish {
			t.Fatalf("%s: %s/%s", item.reason, decoded.Status, decoded.FinishReason)
		}
	}
	withCall := decodeResponse(&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "f", Args: map[string]any{}}}}},
	}}}, "m")
	if withCall.FinishReason != "tool_calls" || withCall.Output[0].Type != "function_call" {
		t.Fatalf("tool call = %#v", withCall)
	}
}

const nativeRequestBody = `{
  "systemInstruction": {"parts": [{"text": "be brief"}]},
  "contents": [{"role": "user", "parts": [{"text": "hi"}]}],
  "tools": [{"functionDeclarations": [{"name": "lookup", "description": "d", "parameters": {"type": "OBJECT", "properties": {"q": {"type": "STRING"}}}}]}],
  "toolConfig": {"functionCallingConfig": {"mode": "AUTO"}},
  "generationConfig": {"temperature": 0.2, "maxOutputTokens": 64, "responseMimeType": "application/json"}
}`

const nativeResponseBody = `{
  "candidates": [{"content": {"role": "model", "parts": [{"text": "think", "thought": true}, {"text": "hello"}]}, "finishReason": "STOP"}],
  "modelVersion": "gemini-test"
}`

func TestNativeCodecRoundTrip(t *testing.T) {
	c := codec{}
	request, err := c.ParseRequest([]byte(nativeRequestBody), convert.Options{})
	if err != nil {
		t.Fatal(err)
	}
	output, err := c.WriteRequest(request, convert.Options{})
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, expected := range []string{`"contents"`, `"be brief"`, `"lookup"`, `"AUTO"`, `"application/json"`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("缺少 %s: %s", expected, text)
		}
	}
	response, err := c.ParseResponse([]byte(nativeResponseBody), convert.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 2 || response.Output[0].Type != "reasoning" || response.Output[1].Type != "message" {
		t.Fatalf("output = %#v", response.Output)
	}
	encoded, err := c.WriteResponse(response, convert.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"thought":true`) {
		t.Fatalf("thought 丢失: %s", encoded)
	}
}

// TestServeRejectsUnsupportedField 确认网关层对无法表达的字段返回 400。
func TestServeRejectsUnsupportedField(t *testing.T) {
	server := newTestServer(t, func(writer http.ResponseWriter, request *http.Request) {
		t.Errorf("不应请求上游: %s", request.URL.Path)
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hi","previous_response_id":"resp_1"}`))
	server.Serve(recorder, request, []byte(`{"input":"hi","previous_response_id":"resp_1"}`), convert.ProtocolResponses)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "previous_response_id") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestServeStreamIncludesUsage 确认 chat 流按 include_usage 追加 usage chunk。
func TestServeStreamIncludesUsage(t *testing.T) {
	server := newTestServer(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5,\"totalTokenCount\":8}}\n\n"))
	})
	body := `{"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	server.Serve(recorder, request, []byte(body), convert.ProtocolChatCompletions)
	output := recorder.Body.String()
	if !strings.Contains(output, `"content":"hello"`) {
		t.Fatalf("缺少正文: %s", output)
	}
	if !strings.Contains(output, `"usage"`) || !strings.Contains(output, "data: [DONE]") {
		t.Fatalf("缺少 usage 或 [DONE]: %s", output)
	}
}

func newTestServer(t *testing.T, handler http.HandlerFunc) *Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	client, err := NewClient(t.Context(), ClientOptions{Provider: ir.ProviderGoogle, GoogleAPIKey: "k", BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(client, ServerOptions{DefaultModel: "gemini-test", Provider: ir.ProviderGoogle})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func boolPointer(value bool) *bool {
	return &value
}
