package gemini

import (
	"encoding/base64"
	"testing"

	"google.golang.org/genai"
)

func TestToGeminiContentsFromResponsesInput(t *testing.T) {
	input := []responseItem{
		{
			Type:    "message",
			Role:    "user",
			Content: []any{map[string]any{"type": "input_text", "text": "What's the weather in Tokyo?"}},
		},
		{
			Type:             "function_call",
			CallID:           "call_123",
			Name:             "get_weather",
			Arguments:        `{"city":"Tokyo"}`,
			ThoughtSignature: base64.StdEncoding.EncodeToString([]byte("signature")),
		},
		{
			Type:   "function_call_output",
			CallID: "call_123",
			Output: `{"temperature": 22, "condition": "Sunny"}`,
		},
	}

	contents, err := toGeminiContentsFromResponsesInput(input)
	if err != nil {
		t.Fatalf("toGeminiContentsFromResponsesInput 失败: %v", err)
	}

	if len(contents) != 3 {
		t.Fatalf("期望 3 个 content, 实际 %d", len(contents))
	}

	// 1. User message
	if contents[0].Role != genai.RoleUser || len(contents[0].Parts) != 1 || contents[0].Parts[0].Text != "What's the weather in Tokyo?" {
		t.Errorf("第 1 个 content 结构不符合预期: %+v", contents[0])
	}

	// 2. Model function call
	if contents[1].Role != genai.RoleModel || len(contents[1].Parts) != 1 || contents[1].Parts[0].FunctionCall == nil {
		t.Fatalf("第 2 个 content 缺少 FunctionCall: %+v", contents[1])
	}
	if contents[1].Parts[0].FunctionCall.Name != "get_weather" {
		t.Errorf("FunctionCall 名字不匹配: %s", contents[1].Parts[0].FunctionCall.Name)
	}
	if string(contents[1].Parts[0].ThoughtSignature) != "signature" {
		t.Fatalf("thought signature = %q", contents[1].Parts[0].ThoughtSignature)
	}

	// 3. User function response
	if contents[2].Role != genai.RoleUser || len(contents[2].Parts) != 1 || contents[2].Parts[0].FunctionResponse == nil {
		t.Fatalf("第 3 个 content 缺少 FunctionResponse: %+v", contents[2])
	}
	if contents[2].Parts[0].FunctionResponse.Name != "get_weather" {
		t.Errorf("FunctionResponse 名字不匹配: %s", contents[2].Parts[0].FunctionResponse.Name)
	}
}

func TestResponsesInputWithoutType(t *testing.T) {
	var request responsesRequest
	if err := decodeKnownRequest([]byte(`{"input":[{"role":"user","content":"hello"}]}`), &request, responsesRequestFields); err != nil {
		t.Fatal(err)
	}
	contents, err := toGeminiContentsFromResponsesInput(request.Input)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 1 || contents[0].Parts[0].Text != "hello" {
		t.Fatalf("contents = %#v", contents)
	}
}

func TestResponsesInputRejectsUnknownItem(t *testing.T) {
	_, err := toGeminiContentsFromResponsesInput([]responseItem{{Type: "unknown"}})
	if err == nil {
		t.Fatal("unknown item type should fail")
	}
}

func TestResponsesStatusFromGeminiFinish(t *testing.T) {
	status, details := responsesStatusFromGeminiFinish(genai.FinishReasonMaxTokens)
	if status != "incomplete" || details["reason"] != "max_output_tokens" {
		t.Fatalf("max tokens = %q, %#v", status, details)
	}
	status, details = responsesStatusFromGeminiFinish(genai.FinishReasonSafety)
	if status != "incomplete" || details["reason"] != "content_filter" {
		t.Fatalf("safety = %q, %#v", status, details)
	}
}

func TestToResponsesUsage(t *testing.T) {
	u := &usageInfo{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		PromptTokensDetails: &promptTokensDetails{
			CachedTokens: 20,
		},
		CompletionTokensDetails: &completionTokDetails{
			ReasoningTokens: 10,
		},
	}

	res := toResponsesUsage(u)
	if res["input_tokens"] != 100 || res["output_tokens"] != 50 || res["total_tokens"] != 150 {
		t.Errorf("Usage 基本字段不匹配: %+v", res)
	}
	inDetails, ok := res["input_tokens_details"].(map[string]any)
	if !ok || inDetails["cached_tokens"] != 20 {
		t.Errorf("input_tokens_details 不匹配: %+v", inDetails)
	}
	outDetails, ok := res["output_tokens_details"].(map[string]any)
	if !ok || outDetails["reasoning_tokens"] != 10 {
		t.Errorf("output_tokens_details 不匹配: %+v", outDetails)
	}
}
