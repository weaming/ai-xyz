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
	input := []byte(`{"id":"resp_1","created_at":10,"model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
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
