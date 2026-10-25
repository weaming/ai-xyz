package convert

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/weaming/ai-xyz/ai-gateway/ir"
)

// ReadStream 把 Chat Completions SSE 解析成 IR 事件。
func (ChatCodec) ReadStream(reader io.Reader, options Options, emit func(ir.StreamEvent) error) error {
	state := &chatStreamReader{tools: map[int]*chatToolCall{}, nextIndex: OutputIndexFirstTool}
	if err := ReadSSE(reader, func(frame SSEFrame) error { return state.consume(frame, emit) }); err != nil {
		return err
	}
	if !state.done {
		return fmt.Errorf("Chat SSE 缺少 [DONE] 终止标记")
	}
	return nil
}

type chatStreamReader struct {
	id        string
	model     string
	created   int64
	started   bool
	nextIndex int
	tools     map[int]*chatToolCall
	systemFP  string
	service   string
	finish    string
	usage     *ir.Usage
	done      bool
}

func (reader *chatStreamReader) consume(frame SSEFrame, emit func(ir.StreamEvent) error) error {
	if frame.Data == "[DONE]" {
		reader.done = true
		status, incomplete := responsesStatusFromChatFinish(reader.finish)
		return emit(ir.StreamEvent{
			Type:              ir.EventCompleted,
			ResponseID:        reader.id,
			Model:             reader.model,
			CreatedAt:         reader.created,
			FinishReason:      reader.finish,
			Status:            status,
			IncompleteDetails: incomplete,
			Usage:             reader.usage,
		})
	}
	data, err := rawMap(frame.Data)
	if err != nil {
		return fmt.Errorf("解析 Chat SSE: %w", err)
	}
	if !reader.started {
		reader.started = true
		reader.id = stringValue(data["id"])
		reader.model = stringValue(data["model"])
		reader.created = intValue(data["created"])
		reader.systemFP = stringValue(data["system_fingerprint"])
		reader.service = stringValue(data["service_tier"])
		if err := emit(ir.StreamEvent{Type: ir.EventCreated, ResponseID: reader.id, Model: reader.model, CreatedAt: reader.created}); err != nil {
			return err
		}
	}
	if usage := usageFromChat(data["usage"]); usage != nil {
		reader.usage = usage
		if err := emit(ir.StreamEvent{Type: ir.EventUsage, Usage: usage}); err != nil {
			return err
		}
	}
	for _, choice := range rawList(data["choices"]) {
		if index := int(intValue(choice["index"])); index != 0 {
			return fmt.Errorf("Chat 流包含 choice index %d：多 choice (n>1) 无法转换为单一响应", index)
		}
		delta := objectMap(choice["delta"])
		events, err := reader.deltaEvents(delta)
		if err != nil {
			return err
		}
		for _, event := range events {
			if err := emit(event); err != nil {
				return err
			}
		}
		if finish := chatFinishValue(choice["finish_reason"]); finish != "" {
			reader.finish = finish
		}
	}
	return nil
}

func (reader *chatStreamReader) deltaEvents(delta map[string]json.RawMessage) ([]ir.StreamEvent, error) {
	events := []ir.StreamEvent{}
	if content := stringValue(delta["content"]); content != "" {
		events = append(events, ir.StreamEvent{Type: ir.EventTextDelta, OutputIndex: OutputIndexMessage, ContentIndex: 0, Delta: content})
	}
	if reasoning := stringValue(delta["reasoning_content"]); reasoning != "" {
		events = append(events, ir.StreamEvent{Type: ir.EventReasoningDelta, OutputIndex: OutputIndexReasoning, ContentIndex: 0, Delta: reasoning})
	}
	if refusal := stringValue(delta["refusal"]); refusal != "" {
		events = append(events, ir.StreamEvent{Type: ir.EventRefusalDelta, OutputIndex: OutputIndexMessage, ContentIndex: 0, Refusal: refusal})
	}
	for _, call := range rawList(delta["tool_calls"]) {
		callEvents, err := reader.toolCallEvents(call)
		if err != nil {
			return nil, err
		}
		events = append(events, callEvents...)
	}
	return events, nil
}

// chatFinishValue 读取 finish_reason，把字面量 "null" 视为缺失。
func chatFinishValue(raw json.RawMessage) string {
	finish := stringValue(raw)
	if finish == "null" {
		return ""
	}
	return finish
}

// chatToolCall 保存一次流式工具调用的身份，后续只带 index 与参数的片段也能补齐。
type chatToolCall struct {
	callID      string
	name        string
	toolType    string
	outputIndex int
}

func (reader *chatStreamReader) toolCallEvents(call map[string]json.RawMessage) ([]ir.StreamEvent, error) {
	index := int(intValue(call["index"]))
	function := objectMap(call["function"])
	custom := objectMap(call["custom"])
	identity, exists := reader.tools[index]
	events := []ir.StreamEvent{}
	if !exists {
		toolType := stringValue(call["type"])
		if toolType == "" {
			toolType = "function"
		}
		if toolType != "function" && toolType != "custom" {
			return nil, fmt.Errorf("Chat Completions 无法转换 tool_call.type %q", call["type"])
		}
		identity = &chatToolCall{
			callID:      stringValue(call["id"]),
			name:        stringValue(function["name"]),
			toolType:    toolType,
			outputIndex: reader.nextIndex,
		}
		if toolType == "custom" {
			identity.name = stringValue(custom["name"])
		}
		reader.nextIndex++
		reader.tools[index] = identity
		events = append(events, ir.StreamEvent{
			Type:        ir.EventToolCallStart,
			OutputIndex: identity.outputIndex,
			CallID:      identity.callID,
			ToolName:    identity.name,
			ToolType:    identity.toolType,
		})
	}
	if name := stringValue(function["name"]); name != "" && identity.name == "" {
		identity.name = name
	}
	if name := stringValue(custom["name"]); name != "" && identity.name == "" {
		identity.name = name
	}
	delta := stringValue(function["arguments"])
	if identity.toolType == "custom" {
		delta = stringValue(custom["input"])
	}
	if delta != "" {
		events = append(events, ir.StreamEvent{
			Type:        ir.EventToolArgumentsDelta,
			OutputIndex: identity.outputIndex,
			CallID:      identity.callID,
			ToolType:    identity.toolType,
			ToolName:    identity.name,
			Delta:       delta,
		})
	}
	return events, nil
}

// AggregateStream 把 Chat SSE 聚合成 chat.completion，并保留 chunk 级元数据。
func (ChatCodec) AggregateStream(reader io.Reader, options Options) ([]byte, error) {
	state := &chatStreamReader{tools: map[int]*chatToolCall{}, nextIndex: OutputIndexFirstTool}
	accumulator := NewAccumulator()
	if err := ReadSSE(reader, func(frame SSEFrame) error { return state.consume(frame, accumulator.Apply) }); err != nil {
		return nil, err
	}
	if !state.done {
		return nil, fmt.Errorf("Chat SSE 缺少 [DONE] 终止标记")
	}
	response, err := accumulator.Response()
	if err != nil {
		return nil, err
	}
	output, err := ChatCodec{}.WriteResponse(response, options)
	if err != nil {
		return nil, err
	}
	var target map[string]any
	if err := json.Unmarshal(output, &target); err != nil {
		return nil, fmt.Errorf("聚合 Chat 响应: %w", err)
	}
	if state.systemFP != "" {
		target["system_fingerprint"] = state.systemFP
	}
	if state.service != "" {
		target["service_tier"] = state.service
	}
	return json.Marshal(target)
}

// NewStreamWriter 创建把 IR 事件写成 Chat Completions SSE 的写入器。
func (ChatCodec) NewStreamWriter(writer io.Writer, options Options) StreamWriter {
	return &chatStreamWriter{
		sse:       sseWriter{writer: writer},
		options:   options,
		acc:       NewAccumulator(),
		toolIndex: map[string]int{},
		toolDelta: map[string]bool{},
		toolName:  map[string]string{},
		id:        newID("chatcmpl-"),
	}
}

type chatStreamWriter struct {
	sse       sseWriter
	options   Options
	acc       *Accumulator
	started   bool
	id        string
	model     string
	created   int64
	toolCount int
	toolIndex map[string]int
	toolDelta map[string]bool
	toolName  map[string]string
	usage     *ir.Usage
}

func (writer *chatStreamWriter) Write(event ir.StreamEvent) error {
	if err := writer.acc.Apply(event); err != nil {
		return err
	}
	switch event.Type {
	case ir.EventCreated:
		if event.ResponseID != "" {
			writer.id = chatStreamID(event.ResponseID)
		}
		writer.model = event.Model
		writer.created = event.CreatedAt
		writer.started = true
		return writer.writeChunk(map[string]any{"role": "assistant"}, nil)
	case ir.EventTextDelta:
		return writer.writeChunk(map[string]any{"content": event.Delta}, nil)
	case ir.EventReasoningDelta:
		if !writer.options.EmitReasoning {
			return nil
		}
		return writer.writeChunk(map[string]any{"reasoning_content": event.Delta}, nil)
	case ir.EventRefusalDelta:
		return writer.writeChunk(map[string]any{"refusal": event.Refusal}, nil)
	case ir.EventToolCallStart:
		index := writer.toolSlot(event)
		writer.toolName[event.CallID] = event.ToolName
		call := writer.toolCall(index, event.CallID, event.ToolType, event.ToolName, event.Arguments)
		if event.Arguments != "" {
			writer.toolDelta[event.CallID] = true
		}
		return writer.writeChunk(map[string]any{"tool_calls": []any{call}}, nil)
	case ir.EventToolArgumentsDelta:
		index := writer.toolSlot(event)
		writer.toolDelta[event.CallID] = true
		call := writer.toolCall(index, event.CallID, event.ToolType, event.ToolName, event.Delta)
		return writer.writeChunk(map[string]any{"tool_calls": []any{call}}, nil)
	case ir.EventToolArgumentsDone:
		if writer.toolDelta[event.CallID] {
			return nil
		}
		writer.toolDelta[event.CallID] = true
		index := writer.toolSlot(event)
		call := writer.toolCall(index, event.CallID, event.ToolType, event.ToolName, event.Arguments)
		return writer.writeChunk(map[string]any{"tool_calls": []any{call}}, nil)
	case ir.EventOutputItemDone:
		return writer.writeItemTool(event)
	case ir.EventUsage:
		writer.usage = event.Usage
	case ir.EventCompleted:
		if event.Usage != nil {
			writer.usage = event.Usage
		}
	}
	return nil
}

func (writer *chatStreamWriter) writeItemTool(event ir.StreamEvent) error {
	item := event.Item
	if item == nil || (item.Type != "function_call" && item.Type != "custom_tool_call") {
		return nil
	}
	if writer.toolDelta[item.CallID] {
		return nil
	}
	writer.toolDelta[item.CallID] = true
	toolType := "function"
	value := item.Arguments
	if item.Type == "custom_tool_call" {
		toolType = "custom"
		value = item.Input
	}
	index := writer.toolSlot(ir.StreamEvent{CallID: item.CallID, ToolType: toolType, ToolName: item.Name})
	call := writer.toolCall(index, item.CallID, toolType, item.Name, value)
	return writer.writeChunk(map[string]any{"tool_calls": []any{call}}, nil)
}

func (writer *chatStreamWriter) toolSlot(event ir.StreamEvent) int {
	if index, exists := writer.toolIndex[event.CallID]; exists {
		return index
	}
	index := writer.toolCount
	writer.toolCount++
	writer.toolIndex[event.CallID] = index
	if event.ToolName != "" {
		writer.toolName[event.CallID] = event.ToolName
	}
	return index
}

func (writer *chatStreamWriter) toolCall(index int, callID, toolType, name, value string) map[string]any {
	if toolType == "custom" {
		return map[string]any{
			"index":  index,
			"id":     callID,
			"type":   "custom",
			"custom": map[string]any{"name": name, "input": value},
		}
	}
	return map[string]any{
		"index":    index,
		"id":       callID,
		"type":     "function",
		"function": map[string]any{"name": name, "arguments": value},
	}
}

// Close 补齐 finish chunk、usage chunk 与 [DONE]，并返回聚合响应。
func (writer *chatStreamWriter) Close() (*ir.Response, error) {
	response, err := writer.acc.Response()
	if err != nil {
		return nil, err
	}
	if !writer.started {
		if err := writer.writeChunk(map[string]any{"role": "assistant"}, nil); err != nil {
			return nil, err
		}
	}
	finish := chatFinishReason(response)
	if err := writer.writeChunk(map[string]any{}, finish); err != nil {
		return nil, err
	}
	if writer.options.IncludeUsage && writer.usage != nil {
		chunk := writer.chunk(nil, nil)
		chunk["choices"] = []any{}
		chunk["usage"] = chatUsage(writer.usage)
		if err := writer.sse.frame(SSEFrame{Data: mustJSONString(chunk)}); err != nil {
			return nil, err
		}
	}
	if err := writer.sse.frame(SSEFrame{Data: "[DONE]"}); err != nil {
		return nil, err
	}
	return response, nil
}

func (writer *chatStreamWriter) writeChunk(delta map[string]any, finish any) error {
	return writer.sse.frame(SSEFrame{Data: mustJSONString(writer.chunk(delta, finish))})
}

func (writer *chatStreamWriter) chunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id":      writer.id,
		"object":  "chat.completion.chunk",
		"created": writer.created,
		"model":   writer.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         deltaOrEmpty(delta),
			"finish_reason": finish,
		}},
	}
}

// chatStreamID 让 chat.completion.chunk 使用 chatcmpl 前缀。
func chatStreamID(id string) string {
	if id == "" {
		return newID("chatcmpl-")
	}
	return id
}

// sseWriter 在写入后尝试 flush，保持流式实时性。
type sseWriter struct {
	writer io.Writer
}

func (writer sseWriter) frame(frame SSEFrame) error {
	if err := WriteSSE(writer.writer, frame); err != nil {
		return err
	}
	if flusher, ok := writer.writer.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	return nil
}
