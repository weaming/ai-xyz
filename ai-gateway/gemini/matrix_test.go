package gemini_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/weaming/ai-xyz/ai-gateway/convert"
	"github.com/weaming/ai-xyz/ai-gateway/gemini"
	"github.com/weaming/ai-xyz/ai-gateway/ir"
	"google.golang.org/genai"
)

// protocols 是 3×3 矩阵的三个协议。
var protocols = []convert.Protocol{
	convert.ProtocolChatCompletions,
	convert.ProtocolResponses,
	convert.ProtocolGenerateContent,
}

const chatRequest = `{
  "model": "gemini-test",
  "messages": [
    {"role": "system", "content": "be brief"},
    {"role": "user", "content": "hi"}
  ],
  "tools": [{"type": "function", "function": {"name": "lookup", "description": "d", "parameters": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]}}}],
  "tool_choice": "auto",
  "response_format": {"type": "json_object"},
  "max_completion_tokens": 64,
  "temperature": 0.2,
  "stop": ["END"],
  "stream": false
}`

const responsesRequest = `{
  "model": "gemini-test",
  "instructions": "be brief",
  "input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]}],
  "tools": [{"type": "function", "name": "lookup", "description": "d", "parameters": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]}}],
  "tool_choice": "auto",
  "text": {"format": {"type": "json_object"}},
  "max_output_tokens": 64,
  "temperature": 0.2,
  "stream": false
}`

const nativeRequest = `{
  "systemInstruction": {"parts": [{"text": "be brief"}]},
  "contents": [{"role": "user", "parts": [{"text": "hi"}]}],
  "tools": [{"functionDeclarations": [{"name": "lookup", "description": "d", "parameters": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]}}]}],
  "toolConfig": {"functionCallingConfig": {"mode": "AUTO"}},
  "generationConfig": {"temperature": 0.2, "maxOutputTokens": 64, "responseMimeType": "application/json"}
}`

const chatResponse = `{
  "id": "chatcmpl-1", "object": "chat.completion", "created": 1700000000, "model": "gemini-test",
  "choices": [{"index": 0, "message": {"role": "assistant", "content": "hello", "reasoning_content": "think"}, "finish_reason": "stop"}],
  "usage": {"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8, "completion_tokens_details": {"reasoning_tokens": 2}, "prompt_tokens_details": {"cached_tokens": 1}}
}`

const responsesResponse = `{
  "id": "resp_1", "object": "response", "created_at": 1700000000, "model": "gemini-test", "status": "completed",
  "output": [
    {"type": "reasoning", "summary": [{"type": "summary_text", "text": "think"}]},
    {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "hello"}]}
  ],
  "usage": {"input_tokens": 3, "output_tokens": 5, "total_tokens": 8, "output_tokens_details": {"reasoning_tokens": 2}, "input_tokens_details": {"cached_tokens": 1}}
}`

const nativeResponse = `{
  "candidates": [{"content": {"role": "model", "parts": [{"text": "think", "thought": true}, {"text": "hello"}]}, "finishReason": "STOP"}],
  "modelVersion": "gemini-test",
  "usageMetadata": {"promptTokenCount": 3, "candidatesTokenCount": 5, "totalTokenCount": 8, "thoughtsTokenCount": 2, "cachedContentTokenCount": 1}
}`

const chatStream = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gemini-test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hel\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gemini-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

const responsesStream = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gemini-test\",\"created_at\":1700000000}}\n\n" +
	"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hel\"}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"lo\"}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"

const nativeStream = "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hel\"}]}}]}\n\n" +
	"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"lo\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5,\"totalTokenCount\":8}}\n\n"

func newTestConverter() *gemini.Converter {
	return gemini.NewConverter(ir.ProviderGoogle, genai.ThinkingLevelUnspecified)
}

func requestFixture(protocol convert.Protocol) string {
	switch protocol {
	case convert.ProtocolChatCompletions:
		return chatRequest
	case convert.ProtocolResponses:
		return responsesRequest
	default:
		return nativeRequest
	}
}

func responseFixture(protocol convert.Protocol) string {
	switch protocol {
	case convert.ProtocolChatCompletions:
		return chatResponse
	case convert.ProtocolResponses:
		return responsesResponse
	default:
		return nativeResponse
	}
}

func streamFixture(protocol convert.Protocol) string {
	switch protocol {
	case convert.ProtocolChatCompletions:
		return chatStream
	case convert.ProtocolResponses:
		return responsesStream
	default:
		return nativeStream
	}
}

// TestMatrixSupportsAllPairs 确认 3×3 组合全部可用。
func TestMatrixSupportsAllPairs(t *testing.T) {
	converter := newTestConverter()
	for _, source := range protocols {
		for _, target := range protocols {
			if !converter.Supports(source, target) {
				t.Fatalf("Supports(%s, %s) = false", source, target)
			}
		}
	}
}

// TestMatrixConvertRequest 验证 9 种请求组合都能产出目标协议结构。
func TestMatrixConvertRequest(t *testing.T) {
	converter := newTestConverter()
	options := convert.Options{DefaultModel: "gemini-test", EmitReasoning: true}
	for _, source := range protocols {
		for _, target := range protocols {
			output, err := converter.ConvertRequest(source, target, []byte(requestFixture(source)), options)
			if err != nil {
				t.Fatalf("%s -> %s 请求转换失败: %v", source, target, err)
			}
			assertRequestShape(t, source, target, output)
		}
	}
}

func assertRequestShape(t *testing.T, source, target convert.Protocol, output []byte) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(output, &body); err != nil {
		t.Fatalf("%s -> %s 请求不是合法 JSON: %v (%s)", source, target, err, output)
	}
	var required string
	switch target {
	case convert.ProtocolChatCompletions:
		required = "messages"
	case convert.ProtocolResponses:
		required = "input"
	default:
		required = "contents"
	}
	if _, exists := body[required]; !exists {
		t.Fatalf("%s -> %s 缺少 %q: %s", source, target, required, output)
	}
	if !strings.Contains(string(output), "lookup") {
		t.Fatalf("%s -> %s 丢失工具定义: %s", source, target, output)
	}
	if target == convert.ProtocolGenerateContent && !strings.Contains(string(output), "be brief") {
		t.Fatalf("%s -> %s 丢失 system 指令: %s", source, target, output)
	}
}

// TestMatrixConvertResponse 验证 9 种响应组合都能产出目标协议结构。
func TestMatrixConvertResponse(t *testing.T) {
	converter := newTestConverter()
	options := convert.Options{EmitReasoning: true}
	for _, source := range protocols {
		for _, target := range protocols {
			output, err := converter.ConvertResponse(source, target, []byte(responseFixture(source)), options)
			if err != nil {
				t.Fatalf("%s -> %s 响应转换失败: %v", source, target, err)
			}
			assertResponseShape(t, source, target, output)
		}
	}
}

func assertResponseShape(t *testing.T, source, target convert.Protocol, output []byte) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(output, &body); err != nil {
		t.Fatalf("%s -> %s 响应不是合法 JSON: %v (%s)", source, target, err, output)
	}
	text := string(output)
	if !strings.Contains(text, "hello") {
		t.Fatalf("%s -> %s 丢失正文: %s", source, target, text)
	}
	switch target {
	case convert.ProtocolChatCompletions:
		if !strings.Contains(text, `"choices"`) {
			t.Fatalf("%s -> %s 缺少 choices: %s", source, target, text)
		}
	case convert.ProtocolResponses:
		if !strings.Contains(text, `"output"`) || !strings.Contains(text, "output_text") {
			t.Fatalf("%s -> %s 缺少 output_text: %s", source, target, text)
		}
	default:
		if !strings.Contains(text, `"candidates"`) {
			t.Fatalf("%s -> %s 缺少 candidates: %s", source, target, text)
		}
	}
}

// TestMatrixConvertStream 验证 9 种流式组合都能输出目标协议 SSE。
func TestMatrixConvertStream(t *testing.T) {
	converter := newTestConverter()
	options := convert.Options{EmitReasoning: true}
	for _, source := range protocols {
		for _, target := range protocols {
			var builder strings.Builder
			if err := converter.ConvertStream(source, target, strings.NewReader(streamFixture(source)), &builder, options); err != nil {
				t.Fatalf("%s -> %s 流式转换失败: %v", source, target, err)
			}
			assertStreamShape(t, source, target, builder.String())
		}
	}
}

func assertStreamShape(t *testing.T, source, target convert.Protocol, output string) {
	t.Helper()
	if !strings.Contains(output, "hel") {
		t.Fatalf("%s -> %s 丢失增量: %s", source, target, output)
	}
	switch target {
	case convert.ProtocolChatCompletions:
		if !strings.Contains(output, "data: [DONE]") {
			t.Fatalf("%s -> %s 缺少 [DONE]: %s", source, target, output)
		}
	case convert.ProtocolResponses:
		if !strings.Contains(output, "event: response.completed") {
			t.Fatalf("%s -> %s 缺少 response.completed: %s", source, target, output)
		}
	default:
		if !strings.Contains(output, "finishReason") {
			t.Fatalf("%s -> %s 缺少 finishReason: %s", source, target, output)
		}
	}
}

// TestMatrixAggregateStream 验证 9 种组合的非流式聚合。
func TestMatrixAggregateStream(t *testing.T) {
	converter := newTestConverter()
	options := convert.Options{EmitReasoning: true}
	for _, source := range protocols {
		for _, target := range protocols {
			output, err := converter.AggregateStream(source, target, strings.NewReader(streamFixture(source)), options)
			if err != nil {
				t.Fatalf("%s -> %s 流式聚合失败: %v", source, target, err)
			}
			if !strings.Contains(string(output), "hello") {
				t.Fatalf("%s -> %s 聚合结果丢失正文: %s", source, target, output)
			}
		}
	}
}

// TestNativeStreamKeepsReasoningMessageAndTool 覆盖推理、正文与工具并行时的索引冲突。
func TestNativeStreamKeepsReasoningMessageAndTool(t *testing.T) {
	stream := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"think\",\"thought\":true}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"q\":\"x\"},\"id\":\"call_1\"}}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]},\"finishReason\":\"STOP\"}]}\n\n"
	converter := newTestConverter()
	options := convert.Options{EmitReasoning: true}

	responsesOutput, err := converter.AggregateStream(convert.ProtocolGenerateContent, convert.ProtocolResponses, strings.NewReader(stream), options)
	if err != nil {
		t.Fatalf("聚合 Responses 失败: %v", err)
	}
	var responses map[string]any
	if err := json.Unmarshal(responsesOutput, &responses); err != nil {
		t.Fatalf("聚合输出不是合法 JSON: %v", err)
	}
	types := []string{}
	for _, item := range responses["output"].([]any) {
		types = append(types, item.(map[string]any)["type"].(string))
	}
	if strings.Join(types, ",") != "reasoning,message,function_call" {
		t.Fatalf("output 顺序错误: %v", types)
	}

	chatOutput, err := converter.AggregateStream(convert.ProtocolGenerateContent, convert.ProtocolChatCompletions, strings.NewReader(stream), options)
	if err != nil {
		t.Fatalf("聚合 Chat 失败: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatOutput, &chat); err != nil {
		t.Fatalf("聚合输出不是合法 JSON: %v", err)
	}
	message := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hello" || message["reasoning_content"] != "think" {
		t.Fatalf("正文或推理丢失: %#v", message)
	}
	call := message["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "call_1" || call["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("工具调用身份丢失: %#v", call)
	}
}
