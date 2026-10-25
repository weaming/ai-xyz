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

// TestChatStreamToolNameArrivesLate 覆盖名字晚于参数片段到达时仍能补齐调用。
func TestChatStreamToolNameArrivesLate(t *testing.T) {
	stream := chatFrame(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"arguments":"{\"q\":"}}]}`, nil) +
		chatFrame(`{"tool_calls":[{"index":0,"function":{"name":"lookup","arguments":"\"x\"}"}}]}`, "tool_calls") +
		"data: [DONE]\n\n"

	var builder strings.Builder
	if err := openaiConverter().ConvertStream(convert.ProtocolChatCompletions, convert.ProtocolResponses, strings.NewReader(stream), &builder, convert.Options{}); err != nil {
		t.Fatalf("流式转换失败: %v", err)
	}
	output := builder.String()
	if strings.Contains(output, `"name":""`) {
		t.Fatalf("不应写出空 name: %s", output)
	}
	if !strings.Contains(output, `"name":"lookup"`) {
		t.Fatalf("名字应在到达后补发: %s", output)
	}

	// Chat 目标同样要补齐，且参数完整。
	var chat strings.Builder
	if err := openaiConverter().ConvertStream(convert.ProtocolChatCompletions, convert.ProtocolChatCompletions, strings.NewReader(stream), &chat, convert.Options{}); err != nil {
		t.Fatalf("同协议流应透传: %v", err)
	}
	aggregated, err := openaiConverter().AggregateStream(convert.ProtocolChatCompletions, convert.ProtocolChatCompletions, strings.NewReader(stream), convert.Options{})
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	if !strings.Contains(string(aggregated), `"name":"lookup"`) {
		t.Fatalf("聚合结果缺少 name: %s", aggregated)
	}
}

// TestResponsesStreamResolvesToolIdentity 覆盖 OpenAI 的 arguments 增量只带 item_id 的情况。
func TestResponsesStreamResolvesToolIdentity(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"created_at\":1700000000}}\n\n" +
		"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"output_index\":0,\"delta\":\"{\\\"q\\\":\"}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"output_index\":0,\"delta\":\"\\\"x\\\"}\"}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"output_index\":0,\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\",\"status\":\"completed\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n"

	var builder strings.Builder
	if err := openaiConverter().ConvertStream(convert.ProtocolResponses, convert.ProtocolChatCompletions, strings.NewReader(stream), &builder, convert.Options{}); err != nil {
		t.Fatalf("流式转换失败: %v", err)
	}
	output := builder.String()
	if strings.Contains(output, `"name":""`) || strings.Contains(output, `"id":""`) {
		t.Fatalf("工具调用身份丢失: %s", output)
	}
	if strings.Count(output, `"role":"assistant"`) != 1 {
		t.Fatalf("response.created 与 in_progress 只应创建一个响应: %s", output)
	}
	aggregated, err := openaiConverter().AggregateStream(convert.ProtocolResponses, convert.ProtocolChatCompletions, strings.NewReader(stream), convert.Options{})
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(aggregated, &chat); err != nil {
		t.Fatalf("聚合输出不是合法 JSON: %v", err)
	}
	message := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("应只有一个工具调用: %#v", calls)
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_1" || call["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("工具调用身份错误: %#v", call)
	}
}

// responsesFrame 构造 Responses SSE 帧，event 为空时只发 data。
func responsesFrame(event, data string) string {
	if event == "" {
		return "data: " + data + "\n\n"
	}
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// citationStream 构造带 url_citation 的 Responses 流，citation 同时出现在
// annotation.added 事件与 output_item.done 的完整 item 中。
func citationStream() string {
	annotation := `{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":2}`
	return responsesFrame("response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1700000000}}`) +
		responsesFrame("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`) +
		responsesFrame("response.output_text.delta", `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}`) +
		responsesFrame("response.output_text.annotation.added", `{"type":"response.output_text.annotation.added","output_index":0,"content_index":0,"annotation_index":0,"annotation":`+annotation+`}`) +
		responsesFrame("response.web_search_call.completed", `{"type":"response.web_search_call.completed","output_index":1,"item_id":"ws_1"}`) +
		responsesFrame("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[`+annotation+`]}]}}`) +
		responsesFrame("response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`)
}

// TestResponsesStreamCarriesCitationsToChat 覆盖流式 citation 的来源汇总与聚合去重。
func TestResponsesStreamCarriesCitationsToChat(t *testing.T) {
	stream := citationStream()
	var builder strings.Builder
	if err := openaiConverter().ConvertStream(convert.ProtocolResponses, convert.ProtocolChatCompletions, strings.NewReader(stream), &builder, convert.Options{ForwardBuiltInTools: true}); err != nil {
		t.Fatalf("流式转换失败: %v", err)
	}
	output := builder.String()
	if !strings.Contains(output, "Example: https://example.com") {
		t.Fatalf("流式正文缺少来源: %s", output)
	}
	if strings.Count(output, "https://example.com") != 1 {
		t.Fatalf("来源不应重复: %s", output)
	}

	aggregated, err := openaiConverter().AggregateStream(convert.ProtocolResponses, convert.ProtocolChatCompletions, strings.NewReader(stream), convert.Options{ForwardBuiltInTools: true})
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(aggregated, &chat); err != nil {
		t.Fatalf("聚合输出不是合法 JSON: %v", err)
	}
	message := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	annotations, ok := message["annotations"].([]any)
	if !ok || len(annotations) != 1 {
		t.Fatalf("同一 citation 应去重为一条: %#v", message["annotations"])
	}
	citation := annotations[0].(map[string]any)
	if citation["url"] != "https://example.com" || citation["end_index"] != float64(2) {
		t.Fatalf("citation 字段丢失: %#v", citation)
	}
	if _, exists := message["tool_calls"]; exists {
		t.Fatalf("托管工具调用不应投影成 Chat 工具调用: %#v", message["tool_calls"])
	}
}

// TestResponsesStreamAnnotationsOnlyInItemDone 覆盖 annotation 只出现在 item.done 的情况。
func TestResponsesStreamAnnotationsOnlyInItemDone(t *testing.T) {
	stream := responsesFrame("response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1700000000}}`) +
		responsesFrame("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`) +
		responsesFrame("response.output_text.delta", `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}`) +
		responsesFrame("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[{"type":"url_citation","url":"https://example.com","start_index":0,"end_index":2}]}]}}`) +
		responsesFrame("response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`)

	aggregated, err := openaiConverter().AggregateStream(convert.ProtocolResponses, convert.ProtocolChatCompletions, strings.NewReader(stream), convert.Options{ForwardBuiltInTools: true})
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(aggregated, &chat); err != nil {
		t.Fatalf("聚合输出不是合法 JSON: %v", err)
	}
	message := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if !strings.Contains(mustString(t, message["content"]), "hi") {
		t.Fatalf("正文丢失: %#v", message)
	}
	annotations, ok := message["annotations"].([]any)
	if !ok || len(annotations) != 1 {
		t.Fatalf("item.done 中的 annotations 未保留: %#v", message["annotations"])
	}
}

// dataFrame 构造只有 data 的 SSE 帧。
func dataFrame(data string) string { return "data: " + data + "\n\n" }

// TestResponsesStreamToolNameInItemDone 覆盖名字只在 output_item.done 出现的上游：
// 参数片段照发但不写 name 字段，名字到达后单独补一帧，不重复参数。
func TestResponsesStreamToolNameInItemDone(t *testing.T) {
	stream := responsesFrame("response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1}}`) +
		responsesFrame("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","arguments":""}}`) +
		dataFrame(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"q\":1}"}`) +
		responsesFrame("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}}`) +
		responsesFrame("response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`)

	var builder strings.Builder
	if err := openaiConverter().ConvertStream(convert.ProtocolResponses, convert.ProtocolChatCompletions, strings.NewReader(stream), &builder, convert.Options{}); err != nil {
		t.Fatalf("流式转换失败: %v", err)
	}
	output := builder.String()
	if strings.Contains(output, `"name":""`) {
		t.Fatalf("不应写出空 name: %s", output)
	}
	if !strings.Contains(output, `"arguments":"{\"q\":1}"`) {
		t.Fatalf("参数片段应照发: %s", output)
	}
	if strings.Count(output, `"name":"lookup"`) != 1 {
		t.Fatalf("名字应只补一次: %s", output)
	}
	// 补名字的那一帧不能重复参数，否则覆盖式合并的客户端会把参数写坏。
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, `"name":"lookup"`) && strings.Contains(line, "arguments") {
			t.Fatalf("补名字的帧不应带参数: %s", line)
		}
	}
}

// TestResponsesStreamOmitsUnknownToolName 覆盖全程没有名字的调用：片段照发，
// 但绝不写 name 字段（空串会被下游当成真名存进历史）。
func TestResponsesStreamOmitsUnknownToolName(t *testing.T) {
	stream := responsesFrame("response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1}}`) +
		responsesFrame("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","arguments":""}}`) +
		dataFrame(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"q\":1}"}`) +
		responsesFrame("response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`)

	var builder strings.Builder
	if err := openaiConverter().ConvertStream(convert.ProtocolResponses, convert.ProtocolChatCompletions, strings.NewReader(stream), &builder, convert.Options{}); err != nil {
		t.Fatalf("流式转换失败: %v", err)
	}
	output := builder.String()
	if strings.Contains(output, `"name":""`) || strings.Contains(output, `"name"`) {
		t.Fatalf("不应写出 name 字段: %s", output)
	}
	if !strings.Contains(output, `"arguments":"{\"q\":1}"`) || !strings.Contains(output, `"id":"call_1"`) {
		t.Fatalf("调用片段应照发: %s", output)
	}
}
