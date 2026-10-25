package convert_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/weaming/ai-xyz/ai-gateway/convert"
	"github.com/weaming/ai-xyz/ai-gateway/ir"
)

// chatFrame 构造 Chat chunk，finish 传 nil 表示 JSON null（无终态）。
func chatFrame(delta string, finish any) string {
	chunk := map[string]any{
		"id":      "chatcmpl-1",
		"object":  "chat.completion.chunk",
		"created": 1700000000,
		"model":   "m",
		"choices": []any{map[string]any{"index": 0, "delta": json.RawMessage(delta), "finish_reason": finish}},
	}
	data, _ := json.Marshal(chunk)
	return "data: " + string(data) + "\n\n"
}

func aggregate(t *testing.T, source, target convert.Protocol, stream string) map[string]any {
	t.Helper()
	output, err := openaiConverter().AggregateStream(source, target, strings.NewReader(stream), convert.Options{EmitReasoning: true})
	if err != nil {
		t.Fatalf("%s -> %s 聚合失败: %v", source, target, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("聚合输出不是合法 JSON: %v", err)
	}
	return decoded
}

// TestChatStreamKeepsReasoningAndContent 覆盖 reasoning 与正文共用索引导致互相丢失的问题。
func TestChatStreamKeepsReasoningAndContent(t *testing.T) {
	stream := chatFrame(`{"role":"assistant","reasoning_content":"think"}`, nil) +
		chatFrame(`{"content":"hi"}`, "stop") +
		"data: [DONE]\n\n"

	chatResponse := aggregate(t, convert.ProtocolChatCompletions, convert.ProtocolChatCompletions, stream)
	choice := chatResponse["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("无终态片段不应产生终态: %#v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if message["content"] != "hi" {
		t.Fatalf("正文丢失: %#v", message)
	}
	if message["reasoning_content"] != "think" {
		t.Fatalf("推理丢失: %#v", message)
	}

	responsesResponse := aggregate(t, convert.ProtocolChatCompletions, convert.ProtocolResponses, stream)
	types := []string{}
	for _, item := range responsesResponse["output"].([]any) {
		types = append(types, item.(map[string]any)["type"].(string))
	}
	if strings.Join(types, ",") != "reasoning,message" {
		t.Fatalf("output 顺序错误: %v", types)
	}

	var builder strings.Builder
	if err := openaiConverter().ConvertStream(convert.ProtocolChatCompletions, convert.ProtocolResponses, strings.NewReader(stream), &builder, convert.Options{EmitReasoning: true}); err != nil {
		t.Fatalf("流式转换失败: %v", err)
	}
	output := builder.String()
	for _, expected := range []string{
		`"type":"reasoning"`,
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_text.delta",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("缺少 %s: %s", expected, output)
		}
	}
	textDone := strings.Index(output, "response.reasoning_summary_text.done")
	partDone := strings.Index(output, "response.reasoning_summary_part.done")
	reasoningItemDone := strings.LastIndex(output, `"status":"completed","summary":[{"text":"think","type":"summary_text"}],"type":"reasoning"`)
	if textDone < 0 || partDone < 0 || reasoningItemDone < 0 || textDone > partDone || partDone > reasoningItemDone {
		t.Fatalf("reasoning 收尾顺序错误: %s", output)
	}
}

// TestChatStreamToolFragmentsKeepIdentity 覆盖后续只带 index 与参数的片段丢失调用身份的问题。
func TestChatStreamToolFragmentsKeepIdentity(t *testing.T) {
	stream := chatFrame(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]}`, nil) +
		chatFrame(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"x\"}"}}]}`, "tool_calls") +
		"data: [DONE]\n\n"

	chatResponse := aggregate(t, convert.ProtocolChatCompletions, convert.ProtocolChatCompletions, stream)
	choice := chatResponse["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %#v", calls)
	}
	call := calls[0].(map[string]any)
	function := call["function"].(map[string]any)
	if call["id"] != "call_1" || function["name"] != "lookup" || function["arguments"] != `{"q":"x"}` {
		t.Fatalf("调用身份或参数丢失: %#v", call)
	}

	var builder strings.Builder
	if err := openaiConverter().ConvertStream(convert.ProtocolChatCompletions, convert.ProtocolResponses, strings.NewReader(stream), &builder, convert.Options{}); err != nil {
		t.Fatalf("流式转换失败: %v", err)
	}
	output := builder.String()
	if strings.Contains(output, `"finish_reason":"null"`) {
		t.Fatalf("finish_reason 不应是字符串 null: %s", output)
	}
	if strings.Contains(output, `"call_id":""`) {
		t.Fatalf("参数事件缺少 call_id: %s", output)
	}
	for _, expected := range []string{`"call_id":"call_1"`, "response.output_item.added", "response.function_call_arguments.done", "response.output_item.done"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("缺少 %s: %s", expected, output)
		}
	}
}

// TestChatStreamMapsIncompleteFinish 覆盖截断与内容过滤被报成 completed 的问题。
func TestChatStreamMapsIncompleteFinish(t *testing.T) {
	cases := map[string]string{
		"length":         "max_output_tokens",
		"content_filter": "content_filter",
	}
	for finish, reason := range cases {
		stream := chatFrame(`{"content":"hi"}`, finish) + "data: [DONE]\n\n"
		responsesResponse := aggregate(t, convert.ProtocolChatCompletions, convert.ProtocolResponses, stream)
		if responsesResponse["status"] != "incomplete" {
			t.Fatalf("%s: status = %#v", finish, responsesResponse["status"])
		}
		details := responsesResponse["incomplete_details"].(map[string]any)
		if details["reason"] != reason {
			t.Fatalf("%s: incomplete_details = %#v", finish, details)
		}
	}
}

// TestChatStreamRejectsMultipleChoices 覆盖上游返回多 choice 时无法表达单一响应的情况。
func TestChatStreamRejectsMultipleChoices(t *testing.T) {
	stream := "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}},{\"index\":1,\"delta\":{\"content\":\"b\"}}]}\n\n" + "data: [DONE]\n\n"
	if _, err := openaiConverter().AggregateStream(convert.ProtocolChatCompletions, convert.ProtocolChatCompletions, strings.NewReader(stream), convert.Options{}); err == nil || !strings.Contains(err.Error(), "多 choice") {
		t.Fatalf("流式多 choice 应报错: %v", err)
	}
	body := `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"},{"index":1,"message":{"role":"assistant","content":"b"},"finish_reason":"stop"}]}`
	if _, err := openaiConverter().ConvertResponse(convert.ProtocolChatCompletions, convert.ProtocolResponses, []byte(body), convert.Options{}); err == nil || !strings.Contains(err.Error(), "多 choice") {
		t.Fatalf("非流式多 choice 应报错: %v", err)
	}
}

// TestSameProtocolAggregatesKeepSourceFields 覆盖同协议聚合保留源协议字段。
func TestSameProtocolAggregatesKeepSourceFields(t *testing.T) {
	chatStream := "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"m\",\"system_fingerprint\":\"fp_1\",\"service_tier\":\"scale\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" + "data: [DONE]\n\n"
	chatOutput, err := openaiConverter().AggregateStream(convert.ProtocolChatCompletions, convert.ProtocolChatCompletions, strings.NewReader(chatStream), convert.Options{})
	if err != nil {
		t.Fatalf("chat 同协议聚合失败: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatOutput, &chat); err != nil {
		t.Fatalf("聚合输出不是合法 JSON: %v", err)
	}
	if chat["system_fingerprint"] != "fp_1" || chat["service_tier"] != "scale" {
		t.Fatalf("chunk 级字段丢失: %#v", chat)
	}

	responsesStream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"m\",\"service_tier\":\"scale\",\"temperature\":0.2,\"metadata\":{\"k\":\"v\"},\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hi\"}]}]}}\n\n"
	responsesOutput, err := openaiConverter().AggregateStream(convert.ProtocolResponses, convert.ProtocolResponses, strings.NewReader(responsesStream), convert.Options{})
	if err != nil {
		t.Fatalf("responses 同协议聚合失败: %v", err)
	}
	var responses map[string]any
	if err := json.Unmarshal(responsesOutput, &responses); err != nil {
		t.Fatalf("聚合输出不是合法 JSON: %v", err)
	}
	if responses["service_tier"] != "scale" || responses["temperature"] != 0.2 {
		t.Fatalf("终态响应字段丢失: %#v", responses)
	}
	if _, exists := responses["metadata"].(map[string]any)["k"]; !exists {
		t.Fatalf("metadata 丢失: %#v", responses)
	}
}

// TestChatStreamUsageChunkFollowsFinish 覆盖 usage 片段的位置与结构。
func TestChatStreamUsageChunkFollowsFinish(t *testing.T) {
	var buffer strings.Builder
	writer := convert.ChatCodec{}.NewStreamWriter(&buffer, convert.Options{IncludeUsage: true})
	events := []ir.StreamEvent{
		{Type: ir.EventCreated, ResponseID: "chatcmpl-1", Model: "m", CreatedAt: 1700000000},
		{Type: ir.EventTextDelta, Delta: "hi"},
		{Type: ir.EventUsage, Usage: &ir.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}},
		{Type: ir.EventCompleted, Status: "completed", FinishReason: "stop"},
	}
	for _, event := range events {
		if err := writer.Write(event); err != nil {
			t.Fatalf("写入事件失败: %v", err)
		}
	}
	if _, err := writer.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	frames := []map[string]any{}
	if err := convert.ReadSSE(strings.NewReader(buffer.String()), func(frame convert.SSEFrame) error {
		if frame.Data == "[DONE]" {
			return nil
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(frame.Data), &decoded); err != nil {
			return err
		}
		frames = append(frames, decoded)
		return nil
	}); err != nil {
		t.Fatalf("解析输出 SSE 失败: %v", err)
	}
	finishIndex, usageIndex := -1, -1
	for index, frame := range frames {
		if _, exists := frame["usage"]; exists {
			usageIndex = index
			if choices, ok := frame["choices"].([]any); !ok || len(choices) != 0 {
				t.Fatalf("usage 片段应带空 choices: %#v", frame)
			}
		}
		choices, _ := frame["choices"].([]any)
		if len(choices) == 1 {
			if choices[0].(map[string]any)["finish_reason"] != nil {
				finishIndex = index
			}
		}
	}
	if finishIndex < 0 || usageIndex < 0 || finishIndex > usageIndex {
		t.Fatalf("finish=%d usage=%d: %s", finishIndex, usageIndex, buffer.String())
	}
}
