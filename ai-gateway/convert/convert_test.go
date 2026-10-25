package convert_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/weaming/ai-xyz/ai-gateway/convert"
)

func openaiConverter() convert.Converter {
	return convert.NewOpenAICompatibleConverter("openai")
}

func convertRequest(t *testing.T, source, target convert.Protocol, body string, options convert.Options) map[string]any {
	t.Helper()
	output, err := openaiConverter().ConvertRequest(source, target, []byte(body), options)
	if err != nil {
		t.Fatalf("%s -> %s 请求转换失败: %v", source, target, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, output)
	}
	return decoded
}

func convertResponse(t *testing.T, source, target convert.Protocol, body string, options convert.Options) map[string]any {
	t.Helper()
	output, err := openaiConverter().ConvertResponse(source, target, []byte(body), options)
	if err != nil {
		t.Fatalf("%s -> %s 响应转换失败: %v", source, target, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, output)
	}
	return decoded
}

func mustString(t *testing.T, value any) string {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("期望字符串，收到 %#v", value)
	}
	return text
}

func TestChatRequestToResponses(t *testing.T) {
	body := `{
		"model": "m",
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": [{"type": "text", "text": "hi"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAA", "detail": "high"}}]},
			{"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "f", "arguments": "{\"a\":1}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "ok"}
		],
		"tools": [{"type": "function", "function": {"name": "f", "description": "d", "parameters": {"type": "object"}}}],
		"tool_choice": {"type": "function", "function": {"name": "f"}},
		"response_format": {"type": "json_schema", "json_schema": {"name": "out", "schema": {"type": "object"}}},
		"max_completion_tokens": 32,
		"reasoning_effort": "medium"
	}`
	target := convertRequest(t, convert.ProtocolChatCompletions, convert.ProtocolResponses, body, convert.Options{})
	if target["instructions"] != "be brief" {
		t.Fatalf("instructions = %#v", target["instructions"])
	}
	if target["max_output_tokens"] != float64(32) {
		t.Fatalf("max_output_tokens = %#v", target["max_output_tokens"])
	}
	items, _ := target["input"].([]any)
	if len(items) != 3 {
		t.Fatalf("input = %#v", target["input"])
	}
	message := items[0].(map[string]any)
	parts := message["content"].([]any)
	image := parts[1].(map[string]any)
	if image["type"] != "input_image" || image["detail"] != "high" {
		t.Fatalf("image part = %#v", image)
	}
	call := items[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" {
		t.Fatalf("function_call = %#v", call)
	}
	output := items[2].(map[string]any)
	if output["type"] != "function_call_output" || output["output"] != "ok" {
		t.Fatalf("function_call_output = %#v", output)
	}
	tools := target["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "f" {
		t.Fatalf("tools = %#v", tools)
	}
	if choice := target["tool_choice"].(map[string]any); choice["name"] != "f" {
		t.Fatalf("tool_choice = %#v", choice)
	}
	format := target["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "out" {
		t.Fatalf("text.format = %#v", format)
	}
	reasoning := target["reasoning"].(map[string]any)
	if reasoning["effort"] != "medium" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
}

func TestChatRequestToResponsesRejectsUnsupported(t *testing.T) {
	cases := map[string]string{
		"logprobs":             `{"messages":[{"role":"user","content":"hi"}],"logprobs":true}`,
		"n 大于 1":               `{"messages":[{"role":"user","content":"hi"}],"n":2}`,
		"stream_options 不支持字段": `{"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_obfuscation":true}}`,
	}
	for expected, body := range cases {
		_, err := openaiConverter().ConvertRequest(convert.ProtocolChatCompletions, convert.ProtocolResponses, []byte(body), convert.Options{})
		if err == nil || !strings.Contains(err.Error(), expected) {
			t.Fatalf("%s: err = %v", expected, err)
		}
	}
}

func TestResponsesRequestToChat(t *testing.T) {
	body := `{
		"model": "m",
		"instructions": "be brief",
		"input": [
			{"type": "message", "role": "user", "content": [
				{"type": "input_text", "text": "hi"},
				{"type": "input_image", "image_url": "data:image/png;base64,AAA"},
				{"type": "input_audio", "input_audio": {"data": "AAA", "format": "wav"}}
			]},
			{"type": "function_call", "call_id": "c1", "name": "f", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "c1", "output": "ok"},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "think"}]}
		],
		"tools": [{"type": "function", "name": "f", "parameters": {"type": "object"}}],
		"tool_choice": "required",
		"text": {"format": {"type": "json_object"}},
		"max_output_tokens": 16,
		"parallel_tool_calls": true
	}`
	target := convertRequest(t, convert.ProtocolResponses, convert.ProtocolChatCompletions, body, convert.Options{})
	messages := target["messages"].([]any)
	system := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "be brief" {
		t.Fatalf("system message = %#v", system)
	}
	user := messages[1].(map[string]any)
	parts := user["content"].([]any)
	if len(parts) != 3 {
		t.Fatalf("user content = %#v", parts)
	}
	if parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("image part = %#v", parts[1])
	}
	if parts[2].(map[string]any)["type"] != "input_audio" {
		t.Fatalf("audio part = %#v", parts[2])
	}
	assistant := messages[2].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	if calls[0].(map[string]any)["id"] != "c1" {
		t.Fatalf("tool_calls = %#v", calls)
	}
	tool := messages[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "c1" {
		t.Fatalf("tool message = %#v", tool)
	}
	reasoned := messages[len(messages)-1].(map[string]any)
	if reasoned["reasoning_content"] != "think" {
		t.Fatalf("reasoning = %#v", reasoned)
	}
	if target["max_completion_tokens"] != float64(16) {
		t.Fatalf("max_completion_tokens = %#v", target["max_completion_tokens"])
	}
	format := target["response_format"].(map[string]any)
	if format["type"] != "json_object" {
		t.Fatalf("response_format = %#v", format)
	}
}

func TestResponsesRequestToChatPreservesCustomTool(t *testing.T) {
	body := `{"input":[{"type":"message","role":"user","content":"hi"}],
		"tools":[{"type":"custom","name":"grammar","description":"d","format":{"type":"grammar","definition":"x"}}],
		"tool_choice":{"type":"custom","name":"grammar"}}`
	target := convertRequest(t, convert.ProtocolResponses, convert.ProtocolChatCompletions, body, convert.Options{})
	tools := target["tools"].([]any)
	custom := tools[0].(map[string]any)["custom"].(map[string]any)
	if custom["name"] != "grammar" {
		t.Fatalf("custom tool = %#v", tools)
	}
	choice := target["tool_choice"].(map[string]any)["custom"].(map[string]any)
	if choice["name"] != "grammar" {
		t.Fatalf("tool_choice = %#v", target["tool_choice"])
	}
}

func TestResponsesRequestRejectsConversationState(t *testing.T) {
	for _, body := range []string{
		`{"previous_response_id":"resp_1","input":"hi"}`,
		`{"conversation":"conv_1","input":"hi"}`,
	} {
		_, err := openaiConverter().ConvertRequest(convert.ProtocolResponses, convert.ProtocolChatCompletions, []byte(body), convert.Options{})
		if err == nil {
			t.Fatalf("期望拒绝: %s", body)
		}
	}
}

func TestChatResponseToResponses(t *testing.T) {
	body := `{
		"id": "chatcmpl-1", "created": 1700000000, "model": "m",
		"choices": [{"index": 0, "finish_reason": "length", "message": {"role": "assistant", "content": "hi", "reasoning_content": "think", "tool_calls": [{"id": "c1", "type": "custom", "custom": {"name": "grammar", "input": "x"}}]}}],
		"usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3, "completion_tokens_details": {"reasoning_tokens": 1}, "prompt_tokens_details": {"cached_tokens": 1}}
	}`
	target := convertResponse(t, convert.ProtocolChatCompletions, convert.ProtocolResponses, body, convert.Options{EmitReasoning: true})
	if target["status"] != "incomplete" {
		t.Fatalf("status = %#v", target["status"])
	}
	if target["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details = %#v", target["incomplete_details"])
	}
	output := target["output"].([]any)
	types := []string{}
	for _, item := range output {
		types = append(types, mustString(t, item.(map[string]any)["type"]))
	}
	if strings.Join(types, ",") != "reasoning,message,custom_tool_call" {
		t.Fatalf("output types = %v", types)
	}
	usage := target["usage"].(map[string]any)
	if usage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(1) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestResponsesResponseToChat(t *testing.T) {
	body := `{
		"id": "resp_1", "created_at": 1700000000, "model": "m", "status": "incomplete",
		"incomplete_details": {"reason": "content_filter"},
		"output": [
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "think"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "hi"}]},
			{"type": "function_call", "call_id": "c1", "name": "f", "arguments": "{}"}
		],
		"usage": {"input_tokens": 1, "output_tokens": 2, "total_tokens": 3}
	}`
	target := convertResponse(t, convert.ProtocolResponses, convert.ProtocolChatCompletions, body, convert.Options{EmitReasoning: true})
	choice := target["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "hi" || message["reasoning_content"] != "think" {
		t.Fatalf("message = %#v", message)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v", choice["finish_reason"])
	}
	if len(message["tool_calls"].([]any)) != 1 {
		t.Fatalf("tool_calls = %#v", message["tool_calls"])
	}
}

func TestResponsesToChatStream(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"f\",\"arguments\":\"{}\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n"
	var builder strings.Builder
	if err := openaiConverter().ConvertStream(convert.ProtocolResponses, convert.ProtocolChatCompletions, strings.NewReader(stream), &builder, convert.Options{}); err != nil {
		t.Fatalf("流式转换失败: %v", err)
	}
	output := builder.String()
	for _, expected := range []string{`"content":"hi"`, `"tool_calls"`, "data: [DONE]"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("缺少 %s: %s", expected, output)
		}
	}
}

func TestChatToResponsesStream(t *testing.T) {
	stream := "data: {\"id\":\"chatcmpl-1\",\"created\":1700000000,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"created\":1700000000,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	var builder strings.Builder
	if err := openaiConverter().ConvertStream(convert.ProtocolChatCompletions, convert.ProtocolResponses, strings.NewReader(stream), &builder, convert.Options{}); err != nil {
		t.Fatalf("流式转换失败: %v", err)
	}
	output := builder.String()
	if strings.Count(output, "event: response.created") != 1 {
		t.Fatalf("response.created 次数错误: %s", output)
	}
	for _, expected := range []string{"response.output_text.delta", "response.completed"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("缺少 %s: %s", expected, output)
		}
	}
}

func TestStreamRequiresTerminal(t *testing.T) {
	if err := openaiConverter().ConvertStream(convert.ProtocolChatCompletions, convert.ProtocolResponses, strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"), &strings.Builder{}, convert.Options{}); err == nil {
		t.Fatal("Chat 流缺少 [DONE] 时应报错")
	}
	if err := openaiConverter().ConvertStream(convert.ProtocolResponses, convert.ProtocolChatCompletions, strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"), &strings.Builder{}, convert.Options{}); err == nil {
		t.Fatal("Responses 流缺少终止事件时应报错")
	}
}

func TestSameProtocolPassthrough(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"custom_field":{"keep":true}}`)
	output, err := openaiConverter().ConvertRequest(convert.ProtocolChatCompletions, convert.ProtocolChatCompletions, body, convert.Options{})
	if err != nil || string(output) != string(body) {
		t.Fatalf("同协议请求应透传: %v %s", err, output)
	}
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"
	var builder strings.Builder
	if err := openaiConverter().ConvertStream(convert.ProtocolChatCompletions, convert.ProtocolChatCompletions, strings.NewReader(stream), &builder, convert.Options{}); err != nil || builder.String() != stream {
		t.Fatalf("同协议流应透传: %v %s", err, builder.String())
	}
}

func TestAggregateStream(t *testing.T) {
	stream := "data: {\"id\":\"chatcmpl-1\",\"created\":1700000000,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	output, err := openaiConverter().AggregateStream(convert.ProtocolChatCompletions, convert.ProtocolChatCompletions, strings.NewReader(stream), convert.Options{})
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("聚合输出不是合法 JSON: %v", err)
	}
	choice := decoded["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %#v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if message["content"] != "hi" {
		t.Fatalf("message = %#v", message)
	}
}

func TestDefaultModelApplied(t *testing.T) {
	target := convertRequest(t, convert.ProtocolChatCompletions, convert.ProtocolResponses, `{"messages":[{"role":"user","content":"hi"}]}`, convert.Options{DefaultModel: "default-model"})
	if target["model"] != "default-model" {
		t.Fatalf("model = %#v", target["model"])
	}
}

// TestResponsesRequestToChatDropsVerbosityAndSummary 覆盖 Chat 不表达的两个调参字段被丢弃。
func TestResponsesRequestToChatDropsVerbosityAndSummary(t *testing.T) {
	body := `{"input":"hi","text":{"format":{"type":"text"},"verbosity":"high"},"reasoning":{"effort":"medium","summary":"auto"}}`
	target := convertRequest(t, convert.ProtocolResponses, convert.ProtocolChatCompletions, body, convert.Options{})
	if target["reasoning_effort"] != "medium" {
		t.Fatalf("effort 应保留: %#v", target["reasoning_effort"])
	}
	if _, exists := target["verbosity"]; exists {
		t.Fatalf("verbosity 应被丢弃: %#v", target)
	}
	if _, exists := target["reasoning"]; exists {
		t.Fatalf("reasoning.summary 应被丢弃: %#v", target)
	}
}

// TestEncryptedReasoningDegradesToVisibleText 覆盖加密 reasoning 转向 Chat 时保留可见文本。
func TestEncryptedReasoningDegradesToVisibleText(t *testing.T) {
	body := `{"id":"resp_1","created_at":1700000000,"model":"m","status":"completed","output":[
		{"type":"reasoning","id":"rs_1","encrypted_content":"gAAAA-sealed","summary":[{"type":"summary_text","text":"think"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`
	target := convertResponse(t, convert.ProtocolResponses, convert.ProtocolChatCompletions, body, convert.Options{EmitReasoning: true})
	message := target["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["reasoning_content"] != "think" {
		t.Fatalf("可见 reasoning 丢失: %#v", message)
	}
	// 只有密文时降级为空，不报错
	sealedOnly := `{"id":"resp_2","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_2","encrypted_content":"gAAAA"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`
	if _, err := openaiConverter().ConvertResponse(convert.ProtocolResponses, convert.ProtocolChatCompletions, []byte(sealedOnly), convert.Options{EmitReasoning: true}); err != nil {
		t.Fatalf("仅密文时应降级而非报错: %v", err)
	}
}

// TestAnnotationsSurviveResponses 覆盖 output_text.annotations 的解析、保真与拒绝。
func TestAnnotationsSurviveResponses(t *testing.T) {
	body := `{"id":"resp_1","created_at":1700000000,"model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[{"type":"url_citation","url":"https://example.com","title":"t"}]}]}]}`
	codec := convert.ResponsesCodec{}
	response, err := codec.ParseResponse([]byte(body), convert.Options{})
	if err != nil {
		t.Fatalf("解析 Responses 响应失败: %v", err)
	}
	if len(response.Output[0].Content[0].Annotations) != 1 {
		t.Fatalf("annotations 未解析: %#v", response.Output[0].Content)
	}
	encoded, err := codec.WriteResponse(response, convert.Options{})
	if err != nil {
		t.Fatalf("写回 Responses 失败: %v", err)
	}
	if !strings.Contains(string(encoded), "url_citation") {
		t.Fatalf("annotations 未保留: %s", encoded)
	}
	if _, err := openaiConverter().ConvertResponse(convert.ProtocolResponses, convert.ProtocolChatCompletions, []byte(body), convert.Options{}); err == nil || !strings.Contains(err.Error(), "annotations") {
		t.Fatalf("Chat 目标应拒绝 annotations: %v", err)
	}
}

// TestServiceTierAndFingerprintRoundTrip 覆盖 service_tier 与 system_fingerprint 的映射。
func TestServiceTierAndFingerprintRoundTrip(t *testing.T) {
	body := `{"id":"chatcmpl-1","created":1700000000,"model":"m","service_tier":"scale","system_fingerprint":"fp_1","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	responses := convertResponse(t, convert.ProtocolChatCompletions, convert.ProtocolResponses, body, convert.Options{})
	if responses["service_tier"] != "scale" {
		t.Fatalf("service_tier 丢失: %#v", responses)
	}
	chat := convertResponse(t, convert.ProtocolResponses, convert.ProtocolChatCompletions, `{"id":"resp_1","model":"m","status":"completed","service_tier":"scale","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`, convert.Options{})
	if chat["service_tier"] != "scale" {
		t.Fatalf("service_tier 未回填: %#v", chat)
	}
	codec := convert.ChatCodec{}
	parsed, err := codec.ParseResponse([]byte(body), convert.Options{})
	if err != nil {
		t.Fatalf("解析 Chat 响应失败: %v", err)
	}
	encoded, err := codec.WriteResponse(parsed, convert.Options{})
	if err != nil {
		t.Fatalf("写回 Chat 响应失败: %v", err)
	}
	if !strings.Contains(string(encoded), `"system_fingerprint":"fp_1"`) {
		t.Fatalf("system_fingerprint 丢失: %s", encoded)
	}
}

// TestBuiltInToolsAndItemsProjectInsteadOfFailing 覆盖内置工具与内置 item 的降级。
func TestBuiltInToolsAndItemsProjectInsteadOfFailing(t *testing.T) {
	body := `{
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]},
			{"type": "web_search_call", "id": "ws_1", "status": "completed", "action": {"type": "search", "query": "go"}},
			{"type": "tool_search_call", "call_id": "c1", "arguments": {"query": "tools"}},
			{"type": "tool_search_output", "call_id": "c1", "tools": [{"type": "function", "name": "f"}]}
		],
		"tools": [
			{"type": "web_search"},
			{"type": "function", "name": "f", "parameters": {"type": "object"}}
		]
	}`
	converted := convertRequest(t, convert.ProtocolResponses, convert.ProtocolChatCompletions, body, convert.Options{})
	messages := converted["messages"].([]any)
	// web_search_call 被跳过，tool_search_call/output 投影成 function 调用与 tool 消息
	if len(messages) != 3 {
		t.Fatalf("内置 item 应被跳过: %#v", messages)
	}
	if messages[2].(map[string]any)["role"] != "tool" || messages[2].(map[string]any)["tool_call_id"] != "c1" {
		t.Fatalf("tool_search_output 投影失败: %#v", messages[2])
	}
	toolCalls := messages[1].(map[string]any)["tool_calls"].([]any)
	if toolCalls[0].(map[string]any)["function"].(map[string]any)["name"] != "tool_search" {
		t.Fatalf("tool_search_call 应投影成 function call: %#v", toolCalls)
	}
	tools := converted["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "f" {
		t.Fatalf("内置工具应被丢弃: %#v", tools)
	}

	request, err := convert.ResponsesCodec{}.ParseRequest([]byte(body), convert.Options{})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	dropped := strings.Join(request.Dropped, ",")
	for _, expected := range []string{"tools.web_search", "items.web_search_call", "items.tool_search_call"} {
		if !strings.Contains(dropped, expected) {
			t.Fatalf("Dropped 缺少 %s: %v", expected, request.Dropped)
		}
	}
}

// TestBuiltInItemsRoundTripThroughResponses 覆盖内置 item 转向 Responses 时原样保留。
func TestBuiltInItemsRoundTripThroughResponses(t *testing.T) {
	body := `{"input": [
		{"type": "message", "role": "user", "content": "hi"},
		{"type": "web_search_call", "id": "ws_1", "status": "completed", "action": {"type": "search", "query": "go"}}
	]}`
	codec := convert.ResponsesCodec{}
	request, err := codec.ParseRequest([]byte(body), convert.Options{})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	output, err := codec.WriteRequest(request, convert.Options{})
	if err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	if !strings.Contains(string(output), `"web_search_call"`) || !strings.Contains(string(output), `"ws_1"`) {
		t.Fatalf("内置 item 未保留: %s", output)
	}
}

// TestSealedReasoningKeepsOriginalItem 覆盖加密 reasoning 的原始 item 保真。
func TestSealedReasoningKeepsOriginalItem(t *testing.T) {
	body := `{"id":"resp_1","created_at":1700000000,"model":"m","status":"completed","output":[
		{"type":"reasoning","id":"rs_1","status":"completed","encrypted_content":"gAAAA","vendored_field":{"x":1},"summary":[{"type":"summary_text","text":"think"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`
	codec := convert.ResponsesCodec{}
	response, err := codec.ParseResponse([]byte(body), convert.Options{})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	output, err := codec.WriteResponse(response, convert.Options{})
	if err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	text := string(output)
	if !strings.Contains(text, "vendored_field") || !strings.Contains(text, `"rs_1"`) {
		t.Fatalf("密封 reasoning 未原样保留: %s", text)
	}
	// 转 Chat 仍然只取可见 summary，不报错。
	chat := convertResponse(t, convert.ProtocolResponses, convert.ProtocolChatCompletions, body, convert.Options{EmitReasoning: true})
	message := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["reasoning_content"] != "think" {
		t.Fatalf("可见 reasoning 丢失: %#v", message)
	}
}
