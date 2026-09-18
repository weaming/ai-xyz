package convert

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestChatRequestToResponses(t *testing.T) {
	input := []byte(`{"model":"m","messages":[{"role":"system","content":"be concise"},{"role":"user","content":"hello"}],"max_completion_tokens":12}`)
	output, err := ChatRequestToResponses(input, "")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	if value["instructions"] != "be concise" {
		t.Fatalf("instructions = %#v", value["instructions"])
	}
	if value["max_output_tokens"] != float64(12) {
		t.Fatalf("max_output_tokens = %#v", value["max_output_tokens"])
	}
	items := value["input"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["type"] != "message" {
		t.Fatalf("input = %#v", value["input"])
	}
}

func TestChatRequestToResponsesKeepsAppendOnlyInputPrefix(t *testing.T) {
	baseInput := []byte(`{"model":"m","temperature":0.2,"messages":[{"role":"system","content":"be concise"},{"role":"user","content":"first"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"done"}]}`)
	extendedInput := []byte(`{"model":"m","temperature":0.2,"messages":[{"role":"system","content":"be concise"},{"role":"user","content":"first"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"done"},{"role":"user","content":"second"}]}`)

	baseOutput, err := ChatRequestToResponses(baseInput, "")
	if err != nil {
		t.Fatal(err)
	}
	extendedOutput, err := ChatRequestToResponses(extendedInput, "")
	if err != nil {
		t.Fatal(err)
	}

	var baseValue map[string]json.RawMessage
	if err := json.Unmarshal(baseOutput, &baseValue); err != nil {
		t.Fatal(err)
	}
	var extendedValue map[string]json.RawMessage
	if err := json.Unmarshal(extendedOutput, &extendedValue); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(baseValue["instructions"], extendedValue["instructions"]) {
		t.Fatalf("instructions changed: base=%s extended=%s", baseValue["instructions"], extendedValue["instructions"])
	}

	var baseItems []json.RawMessage
	if err := json.Unmarshal(baseValue["input"], &baseItems); err != nil {
		t.Fatal(err)
	}
	var extendedItems []json.RawMessage
	if err := json.Unmarshal(extendedValue["input"], &extendedItems); err != nil {
		t.Fatal(err)
	}
	if len(extendedItems) != len(baseItems)+1 {
		t.Fatalf("input length: base=%d extended=%d", len(baseItems), len(extendedItems))
	}
	for index, baseItem := range baseItems {
		if !bytes.Equal(baseItem, extendedItems[index]) {
			t.Fatalf("input item %d changed: base=%s extended=%s", index, baseItem, extendedItems[index])
		}
	}
}

func TestChatRequestToResponsesAcceptsStreamOptionsIncludeUsage(t *testing.T) {
	input := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}`)
	output, err := ChatRequestToResponses(input, "")
	if err != nil {
		t.Fatal(err)
	}

	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	if _, exists := value["stream_options"]; exists {
		t.Fatalf("stream_options should not be sent to Responses: %#v", value["stream_options"])
	}
}

func TestChatRequestToResponsesRejectsUnsupportedStreamOptions(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "disabled usage",
			input: `{"stream_options":{"include_usage":false},"messages":[{"role":"user","content":"hello"}]}`,
			want:  "include_usage=false",
		},
		{
			name:  "unknown field",
			input: `{"stream_options":{"unknown":true},"messages":[{"role":"user","content":"hello"}]}`,
			want:  `stream_options 不支持字段 "unknown"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ChatRequestToResponses([]byte(test.input), "")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestChatRequestToResponsesNormalizesNullContent(t *testing.T) {
	input := []byte(`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{}"}}]}]}`)
	output, err := ChatRequestToResponses(input, "")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	items := value["input"].([]any)
	message := items[0].(map[string]any)
	if content, ok := message["content"].([]any); !ok || content == nil {
		t.Fatalf("content = %#v", message["content"])
	}
	if items[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("input = %#v", items)
	}
}

func TestChatRequestToResponsesUsesOutputTextForAssistantContent(t *testing.T) {
	input := []byte(`{"model":"m","messages":[{"role":"user","content":"question"},{"role":"assistant","content":"answer"},{"role":"assistant","content":[{"type":"text","text":"second answer"}]}]}`)
	output, err := ChatRequestToResponses(input, "")
	if err != nil {
		t.Fatal(err)
	}

	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}

	items := value["input"].([]any)
	assertContentType := func(index int, expected string) {
		t.Helper()
		item := items[index].(map[string]any)
		content := item["content"].([]any)
		if content[0].(map[string]any)["type"] != expected {
			t.Fatalf("input[%d].content[0].type = %#v, want %q", index, content[0].(map[string]any)["type"], expected)
		}
	}

	assertContentType(0, "input_text")
	assertContentType(1, "output_text")
	assertContentType(2, "output_text")
}

func TestChatRequestToResponsesConvertsImageURL(t *testing.T) {
	input := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc","detail":"low"}}]}]}`)
	output, err := ChatRequestToResponses(input, "")
	if err != nil {
		t.Fatal(err)
	}

	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}

	content := value["input"].([]any)[0].(map[string]any)["content"].([]any)
	image := content[1].(map[string]any)
	if image["type"] != "input_image" {
		t.Fatalf("image type = %#v", image["type"])
	}
	if image["image_url"] != "data:image/png;base64,abc" {
		t.Fatalf("image URL = %#v", image["image_url"])
	}
	if image["detail"] != "low" {
		t.Fatalf("image detail = %#v", image["detail"])
	}
}

func TestChatRequestToResponsesConvertsInputAudio(t *testing.T) {
	input := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"abc","format":"wav"}}]}]}`)
	output, err := ChatRequestToResponses(input, "")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	content := value["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if content["type"] != "input_audio" || content["input_audio"].(map[string]any)["format"] != "wav" {
		t.Fatalf("input audio = %#v", content)
	}
}

func TestChatRequestToResponsesConvertsAllowedTools(t *testing.T) {
	input := []byte(`{"model":"m","tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[{"type":"function","function":{"name":"lookup"}}]}},"messages":[{"role":"user","content":"find"}]}`)
	output, err := ChatRequestToResponses(input, "")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	choice := value["tool_choice"].(map[string]any)
	if choice["type"] != "allowed_tools" || choice["mode"] != "auto" {
		t.Fatalf("tool choice = %#v", choice)
	}
	tool := choice["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "lookup" {
		t.Fatalf("allowed tool = %#v", tool)
	}
}

func TestChatRequestToResponsesOmitsEmptyImageDetail(t *testing.T) {
	input := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]}]}`)
	output, err := ChatRequestToResponses(input, "")
	if err != nil {
		t.Fatal(err)
	}

	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}

	content := value["input"].([]any)[0].(map[string]any)["content"].([]any)
	image := content[0].(map[string]any)
	if _, exists := image["detail"]; exists {
		t.Fatalf("empty image detail should be omitted: %#v", image)
	}
}

func TestChatRequestToResponsesConvertsStructuredOutput(t *testing.T) {
	input := []byte(`{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"answer","description":"result","strict":true,"schema":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}}},"messages":[{"role":"user","content":"answer"}]}`)
	output, err := ChatRequestToResponses(input, "")
	if err != nil {
		t.Fatal(err)
	}

	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	textConfig := value["text"].(map[string]any)
	format := textConfig["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "answer" || format["description"] != "result" || format["strict"] != true {
		t.Fatalf("text.format = %#v", format)
	}
	if format["schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("text.format.schema = %#v", format["schema"])
	}
}

func TestResponsesRequestToChatPreservesMultimodalContent(t *testing.T) {
	input := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"https://example.com/image.png","detail":"high"},{"type":"input_file","file_id":"file_1","filename":"data.txt"}]}]}`)
	output, err := ResponsesRequestToChat(input, "", ModeStrict)
	if err != nil {
		t.Fatal(err)
	}

	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	content := value["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if content[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("image content = %#v", content[1])
	}
	imageURL := content[1].(map[string]any)["image_url"].(map[string]any)
	if imageURL["detail"] != "high" {
		t.Fatalf("image detail = %#v", imageURL["detail"])
	}
	file := content[2].(map[string]any)["file"].(map[string]any)
	if file["file_id"] != "file_1" || file["filename"] != "data.txt" {
		t.Fatalf("file content = %#v", file)
	}
}

func TestResponsesRequestToChatPreservesInputAudio(t *testing.T) {
	input := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"abc","format":"wav"}}]}]}`)
	output, err := ResponsesRequestToChat(input, "", ModePortable)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	content := value["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if content["type"] != "input_audio" || content["input_audio"].(map[string]any)["format"] != "wav" {
		t.Fatalf("input audio = %#v", content)
	}
}

func TestResponsesRequestToChatConvertsAllowedTools(t *testing.T) {
	input := []byte(`{"model":"m","tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"custom","name":"lookup"}]},"input":"find"}`)
	output, err := ResponsesRequestToChat(input, "", ModePortable)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	choice := value["tool_choice"].(map[string]any)
	allowed := choice["allowed_tools"].(map[string]any)
	tool := allowed["tools"].([]any)[0].(map[string]any)
	if choice["type"] != "allowed_tools" || allowed["mode"] != "required" || tool["type"] != "custom" || tool["name"] != "lookup" {
		t.Fatalf("tool choice = %#v", choice)
	}
}

func TestResponsesRequestToChatRejectsConversationState(t *testing.T) {
	input := []byte(`{"model":"m","previous_response_id":"resp_1","input":"next"}`)
	if _, err := ResponsesRequestToChat(input, "", ModePreserve); err == nil {
		t.Fatal("previous_response_id 应明确拒绝")
	}
}

func TestResponsesResponseToChatConvertsCustomToolCall(t *testing.T) {
	input := []byte(`{"id":"resp_1","created_at":10,"model":"m","status":"completed","output":[{"type":"custom_tool_call","call_id":"call_1","name":"search","input":"{\"query\":\"go\"}"}]}`)
	output, err := ResponsesResponseToChat(input)
	if err != nil {
		t.Fatal(err)
	}

	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	call := value["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["type"] != "custom" || call["custom"].(map[string]any)["name"] != "search" {
		t.Fatalf("custom tool call = %#v", call)
	}
}

func TestDefaultRegistryProvidesProviderConverters(t *testing.T) {
	registry := DefaultRegistry()
	for _, provider := range []string{"openai", "deepseek"} {
		converter, err := registry.ForProvider(provider)
		if err != nil {
			t.Fatalf("provider %q: %v", provider, err)
		}
		if converter.Provider() != provider {
			t.Fatalf("converter provider = %q, want %q", converter.Provider(), provider)
		}
		if !converter.Supports(ProtocolChatCompletions, ProtocolResponses) {
			t.Fatalf("provider %q does not support Chat -> Responses", provider)
		}
	}
}

func TestResponsesRequestToChatPreservesToolPair(t *testing.T) {
	input := []byte(`{"model":"m","instructions":"be concise","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"run"}]},{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)
	output, err := ResponsesRequestToChat(input, "", ModePreserve)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	messages := value["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	assistant := messages[2].(map[string]any)
	if assistant["role"] != "assistant" || len(assistant["tool_calls"].([]any)) != 1 {
		t.Fatalf("assistant = %#v", assistant)
	}
}

func TestResponsesResponseToChat(t *testing.T) {
	input := []byte(`{"id":"resp_1","created_at":10,"model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5,"input_tokens_details":{"cached_tokens":1},"output_tokens_details":{"reasoning_tokens":2}}}`)
	output, err := ResponsesResponseToChat(input)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	if value["object"] != "chat.completion" {
		t.Fatalf("object = %#v", value["object"])
	}
	choice := value["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "hello" || choice["finish_reason"] != "tool_calls" {
		t.Fatalf("choice = %#v", choice)
	}
	usage := value["usage"].(map[string]any)
	if usage["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(1) || usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(2) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestResponsesResponseToChatMapsIncompleteStatus(t *testing.T) {
	input := []byte(`{"id":"resp_1","created_at":10,"model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]}`)
	output, err := ResponsesResponseToChat(input)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	choice := value["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Fatalf("choice = %#v", choice)
	}
}

func TestChatResponseToResponsesMapsFinishReasonAndUsageDetails(t *testing.T) {
	input := []byte(`{"id":"chat_1","created":10,"model":"m","choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5,"prompt_tokens_details":{"cached_tokens":1},"completion_tokens_details":{"reasoning_tokens":2}}}`)
	output, err := ChatResponseToResponses(input)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	if value["status"] != "incomplete" || value["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("status = %#v", value)
	}
	usage := value["usage"].(map[string]any)
	if usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(1) || usage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(2) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestResponsesToChatStream(t *testing.T) {
	input := strings.Join([]string{
		`event: response.created`, `data: {"type":"response.created","response":{"id":"resp_1","model":"m","created_at":10}}`, "",
		`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","delta":"hi"}`, "",
		`event: response.completed`, `data: {"type":"response.completed","response":{"id":"resp_1","model":"m","created_at":10,"status":"completed"}}`, "",
	}, "\n")
	var output bytes.Buffer
	if err := ResponsesToChatStream(strings.NewReader(input), &output, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"content":"hi"`) || !strings.Contains(output.String(), "[DONE]") {
		t.Fatalf("output = %s", output.String())
	}
}

func TestResponsesToChatStreamHandlesReasoningOutputItem(t *testing.T) {
	input := strings.Join([]string{
		`event: response.created`, `data: {"type":"response.created","response":{"id":"resp_1","model":"m","created_at":10}}`, "",
		`event: response.output_item.added`, `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress","summary":[]}}`, "",
		`event: response.reasoning_summary_text.delta`, `data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"思考"}`, "",
		`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","output_index":1,"delta":"答案"}`, "",
		`event: response.output_item.done`, `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"思考"}]}}`, "",
		`event: response.completed`, `data: {"type":"response.completed","response":{"id":"resp_1","model":"m","created_at":10,"status":"completed"}}`, "",
	}, "\n")

	tests := []struct {
		name          string
		emitReasoning bool
		wantReasoning bool
	}{
		{name: "emits reasoning", emitReasoning: true, wantReasoning: true},
		{name: "hides reasoning", emitReasoning: false, wantReasoning: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := ResponsesToChatStream(strings.NewReader(input), &output, test.emitReasoning); err != nil {
				t.Fatal(err)
			}
			outputText := output.String()
			if !strings.Contains(outputText, `"content":"答案"`) || !strings.Contains(outputText, "[DONE]") {
				t.Fatalf("output = %s", outputText)
			}
			if hasReasoning := strings.Contains(outputText, `"reasoning_content":"思考"`); hasReasoning != test.wantReasoning {
				t.Fatalf("reasoning output = %v, want %v; output = %s", hasReasoning, test.wantReasoning, outputText)
			}
		})
	}
}

func TestResponsesStreamToChatResponse(t *testing.T) {
	input := strings.Join([]string{
		`event: response.created`, `data: {"type":"response.created","response":{"id":"resp_1","model":"m","created_at":10}}`, "",
		`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","delta":"hi"}`, "",
		`event: response.completed`, `data: {"type":"response.completed","response":{"id":"resp_1","model":"m","created_at":10,"status":"completed"}}`, "",
	}, "\n")
	output, err := ResponsesStreamToChatResponse(strings.NewReader(input), false)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatal(err)
	}
	choice := response["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "hi" || choice["finish_reason"] != "stop" {
		t.Fatalf("response = %#v", response)
	}
}

func TestResponsesStreamToResponseRebuildsOutputItems(t *testing.T) {
	input := strings.Join([]string{
		`event: response.created`, `data: {"type":"response.created","response":{"id":"resp_1","object":"response","model":"m","created_at":10,"output":[]}}`, "",
		`event: response.output_item.added`, `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}`, "",
		`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","delta":"hello"}`, "",
		`event: response.output_item.done`, `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello"}]}}`, "",
		`event: response.completed`, `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"m","created_at":10,"status":"completed","output":[]}}`, "",
	}, "\n")
	output, err := ResponsesStreamToResponse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatal(err)
	}
	items := response["output"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != "msg_1" {
		t.Fatalf("output = %#v", response["output"])
	}
	content := items[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("content = %#v", content)
	}
}

func TestChatToResponsesStreamEmitsCreatedOnce(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chat_1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, "",
		`data: {"id":"chat_1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`, "",
		`data: {"id":"chat_1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "",
		`data: [DONE]`, "",
	}, "\n")
	var output bytes.Buffer
	if err := ChatToResponsesStream(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(output.String(), `"type":"response.created"`); count != 1 {
		t.Fatalf("response.created count = %d, output = %s", count, output.String())
	}
	if !strings.Contains(output.String(), `"delta":"hi"`) || !strings.Contains(output.String(), `"type":"response.completed"`) {
		t.Fatalf("output = %s", output.String())
	}
}

func TestChatToResponsesStreamMapsIncompleteStatus(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chat_1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`, "",
		`data: {"id":"chat_1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`, "",
		`data: [DONE]`, "",
	}, "\n")
	var output bytes.Buffer
	if err := ChatToResponsesStream(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"type":"response.incomplete"`) {
		t.Fatalf("output = %s", output.String())
	}
}

func TestResponsesToChatStreamPreservesCustomToolCall(t *testing.T) {
	input := strings.Join([]string{
		`event: response.created`, `data: {"type":"response.created","response":{"id":"resp_1","model":"m","created_at":10}}`, "",
		`event: response.output_item.added`, `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"tool_1","type":"custom_tool_call","call_id":"call_1","name":"search","input":"","status":"in_progress"}}`, "",
		`event: response.custom_tool_call_input.delta`, `data: {"type":"response.custom_tool_call_input.delta","item_id":"tool_1","call_id":"call_1","delta":"query"}`, "",
		`event: response.completed`, `data: {"type":"response.completed","response":{"id":"resp_1","model":"m","created_at":10,"status":"completed"}}`, "",
	}, "\n")
	var output bytes.Buffer
	if err := ResponsesToChatStream(strings.NewReader(input), &output, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"type":"custom"`) || !strings.Contains(output.String(), `"input":"query"`) {
		t.Fatalf("output = %s", output.String())
	}
}

func TestResponsesStreamToChatResponsePreservesCustomToolCall(t *testing.T) {
	input := strings.Join([]string{
		`event: response.created`, `data: {"type":"response.created","response":{"id":"resp_1","model":"m","created_at":10}}`, "",
		`event: response.output_item.added`, `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"tool_1","type":"custom_tool_call","call_id":"call_1","name":"search","input":"","status":"in_progress"}}`, "",
		`event: response.custom_tool_call_input.delta`, `data: {"type":"response.custom_tool_call_input.delta","item_id":"tool_1","call_id":"call_1","delta":"query"}`, "",
		`event: response.custom_tool_call_input.done`, `data: {"type":"response.custom_tool_call_input.done","item_id":"tool_1","output_index":0,"input":"query"}`, "",
		`event: response.output_item.done`, `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"tool_1","type":"custom_tool_call","call_id":"call_1","name":"search","input":"query","status":"completed"}}`, "",
		`event: response.completed`, `data: {"type":"response.completed","response":{"id":"resp_1","model":"m","created_at":10,"status":"completed"}}`, "",
	}, "\n")
	output, err := ResponsesStreamToChatResponse(strings.NewReader(input), false)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatal(err)
	}
	call := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["type"] != "custom" || call["custom"].(map[string]any)["input"] != "query" {
		t.Fatalf("custom tool call = %#v", call)
	}
}

func TestResponsesToChatStreamRejectsUnsupportedToolEvent(t *testing.T) {
	input := "event: response.web_search_call.in_progress\ndata: {\"type\":\"response.web_search_call.in_progress\"}\n\n"
	var output bytes.Buffer
	if err := ResponsesToChatStream(strings.NewReader(input), &output, false); err == nil {
		t.Fatal("不支持的 Responses tool event 应返回错误")
	}
}

func TestChatToResponsesStreamPreservesCustomToolCall(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chat_1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"custom","custom":{"name":"search","input":"query"}}]},"finish_reason":null}]}`, "",
		`data: {"id":"chat_1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, "",
		`data: [DONE]`, "",
	}, "\n")
	var output bytes.Buffer
	if err := ChatToResponsesStream(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"type":"custom_tool_call"`) || !strings.Contains(output.String(), `"input":"query"`) {
		t.Fatalf("output = %s", output.String())
	}
}

func TestSameProtocolStreamPassesThroughToolEvents(t *testing.T) {
	input := "event: response.web_search_call.in_progress\ndata: {\"type\":\"response.web_search_call.in_progress\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	var output bytes.Buffer
	if err := (OpenAIConverter{}).ConvertStream(ProtocolResponses, ProtocolResponses, strings.NewReader(input), &output, Options{}); err != nil {
		t.Fatal(err)
	}
	if output.String() != input {
		t.Fatalf("stream was changed: %q", output.String())
	}
}
