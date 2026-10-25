package convert

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

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
		toolID:    map[string]string{},
		toolNamed: map[string]bool{},
		toolArgs:  map[string]bool{},
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
	toolIndex map[string]int    // 调用 → Chat 的 tool_calls index
	toolID    map[string]string // 调用 → 下发的 id（上游缺 id 时造一个）
	toolNamed map[string]bool   // 真实名字已下发，之后不再重复
	toolArgs  map[string]bool   // 参数已下发
	toolSent  map[string]bool   // 已下发过至少一个片段
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
		return writer.writeTool(event, event.ToolName, event.Arguments)
	case ir.EventToolArgumentsDelta:
		return writer.writeTool(event, event.ToolName, event.Delta)
	case ir.EventToolArgumentsDone:
		value := event.Arguments
		if writer.toolArgs[toolKey(event.CallID, event.OutputIndex)] {
			// 参数已随增量下发，这里只补可能后到的名字。
			value = ""
		}
		return writer.writeTool(event, event.ToolName, value)
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

// writeItemTool 用完整 item 补齐调用：参数已下发时只补名字，否则整条发出。
func (writer *chatStreamWriter) writeItemTool(event ir.StreamEvent) error {
	item := event.Item
	if item == nil || (item.Type != "function_call" && item.Type != "custom_tool_call") {
		return nil
	}
	toolType := "function"
	value := item.Arguments
	if item.Type == "custom_tool_call" {
		toolType = "custom"
		value = item.Input
	}
	if writer.toolArgs[toolKey(item.CallID, event.OutputIndex)] {
		value = ""
	}
	return writer.writeTool(ir.StreamEvent{CallID: item.CallID, OutputIndex: event.OutputIndex, ToolType: toolType}, item.Name, value)
}

// toolKey 是写端的调用状态键：上游给了 call_id 就用它，没给就按 output_index 区分。
func toolKey(callID string, outputIndex int) string {
	if callID != "" {
		return "id:" + callID
	}
	return "at:" + strconv.Itoa(outputIndex)
}

// writeTool 写一条 tool_calls 片段：名字未知时省略 name 字段（不写空串），
// 名字首次可用时随该片段补发一次；参数按增量下发，空值不写字段。
func (writer *chatStreamWriter) writeTool(event ir.StreamEvent, name, value string) error {
	key := toolKey(event.CallID, event.OutputIndex)
	index, exists := writer.toolIndex[key]
	if !exists {
		index = writer.toolCount
		writer.toolCount++
		writer.toolIndex[key] = index
	}
	argField := "arguments"
	if event.ToolType == "custom" {
		argField = "input"
	}
	fields := map[string]any{}
	if name != "" && !writer.toolNamed[key] {
		writer.toolNamed[key] = true
		fields["name"] = name
	}
	if value != "" {
		writer.toolArgs[key] = true
		fields[argField] = value
	}
	if len(fields) == 0 {
		return nil
	}
	call := map[string]any{"index": index, "id": writer.callID(key, event.CallID), "type": "function"}
	if event.ToolType == "custom" {
		call["type"] = "custom"
		call["custom"] = fields
	} else {
		call["function"] = fields
	}
	return writer.writeChunk(map[string]any{"tool_calls": []any{call}}, nil)
}

// callID 是下发给下游的调用 id，上游没给时造一个并保持一致。
func (writer *chatStreamWriter) callID(key, callID string) string {
	if id, exists := writer.toolID[key]; exists {
		return id
	}
	if callID == "" {
		callID = newID("call_")
	}
	writer.toolID[key] = callID
	return callID
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
