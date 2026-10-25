package convert

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/weaming/ai-xyz/ai-gateway/ir"
)

// ReadStream 把 Responses SSE 解析成 IR 事件。
func (ResponsesCodec) ReadStream(reader io.Reader, options Options, emit func(ir.StreamEvent) error) error {
	state := &responsesStreamReader{forwardBuiltIn: options.ForwardBuiltInTools}
	if err := ReadSSE(reader, func(frame SSEFrame) error { return state.consume(frame, emit) }); err != nil {
		return err
	}
	if !state.terminal {
		return fmt.Errorf("Responses SSE 在终止事件前结束")
	}
	return nil
}

type responsesStreamReader struct {
	terminal       bool
	created        bool
	forwardBuiltIn bool
	tools          map[string]*responsesToolIdentity
}

// responsesToolIdentity 记录 item_id → 工具身份，OpenAI 的 arguments 增量事件只带
// item_id/output_index，不带 call_id 和 name。
type responsesToolIdentity struct {
	callID   string
	name     string
	toolType string
}

func (reader *responsesStreamReader) tool(itemID string, outputIndex int, callID, toolType string) *responsesToolIdentity {
	key := itemID
	if key == "" {
		key = "index:" + strconv.Itoa(outputIndex)
	}
	if reader.tools == nil {
		reader.tools = map[string]*responsesToolIdentity{}
	}
	identity, exists := reader.tools[key]
	if !exists {
		identity = &responsesToolIdentity{toolType: toolType}
		reader.tools[key] = identity
	}
	if callID != "" {
		identity.callID = callID
	}
	if identity.toolType == "" {
		identity.toolType = toolType
	}
	return identity
}

func (reader *responsesStreamReader) consume(frame SSEFrame, emit func(ir.StreamEvent) error) error {
	if frame.Data == "[DONE]" {
		return nil
	}
	data, err := rawMap(frame.Data)
	if err != nil {
		return fmt.Errorf("解析 Responses SSE: %w", err)
	}
	eventTypeName := stringValue(data["type"])
	if eventTypeName == "" {
		eventTypeName = frame.Event
	}
	eventType := eventTypeName
	outputIndex := int(intValue(data["output_index"]))
	switch eventType {
	case "response.created", "response.in_progress":
		if reader.created {
			return nil
		}
		reader.created = true
		response := objectMap(data["response"])
		return emit(ir.StreamEvent{
			Type:       ir.EventCreated,
			ResponseID: stringValue(response["id"]),
			Model:      stringValue(response["model"]),
			CreatedAt:  intValue(response["created_at"]),
		})
	case "response.output_item.added", "response.output_item.done":
		item, err := reader.itemEvent(data, outputIndex)
		if err != nil {
			return err
		}
		eventType := ir.EventOutputItemAdded
		if eventTypeName == "response.output_item.done" {
			eventType = ir.EventOutputItemDone
		}
		return emit(ir.StreamEvent{Type: eventType, OutputIndex: outputIndex, Item: item})
	case "response.output_text.delta":
		return emit(ir.StreamEvent{Type: ir.EventTextDelta, OutputIndex: outputIndex, ContentIndex: int(intValue(data["content_index"])), Delta: stringValue(data["delta"])})
	case "response.output_text.annotation.added":
		return emit(ir.StreamEvent{Type: ir.EventAnnotationAdded, OutputIndex: outputIndex, ContentIndex: int(intValue(data["content_index"])), Annotation: cloneRaw(data["annotation"])})
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return emit(ir.StreamEvent{Type: ir.EventReasoningDelta, OutputIndex: outputIndex, ContentIndex: int(intValue(data["content_index"])), Delta: stringValue(data["delta"])})
	case "response.refusal.delta":
		return emit(ir.StreamEvent{Type: ir.EventRefusalDelta, OutputIndex: outputIndex, ContentIndex: int(intValue(data["content_index"])), Refusal: stringValue(data["delta"])})
	case "response.function_call_arguments.delta":
		identity := reader.tool(stringValue(data["item_id"]), outputIndex, stringValue(data["call_id"]), "function")
		return emit(ir.StreamEvent{Type: ir.EventToolArgumentsDelta, OutputIndex: outputIndex, CallID: identity.callID, ToolName: identity.name, ToolType: identity.toolType, Delta: stringValue(data["delta"])})
	case "response.custom_tool_call_input.delta":
		identity := reader.tool(stringValue(data["item_id"]), outputIndex, stringValue(data["call_id"]), "custom")
		return emit(ir.StreamEvent{Type: ir.EventToolArgumentsDelta, OutputIndex: outputIndex, CallID: identity.callID, ToolName: identity.name, ToolType: identity.toolType, Delta: stringValue(data["delta"])})
	case "response.function_call_arguments.done":
		identity := reader.tool(stringValue(data["item_id"]), outputIndex, stringValue(data["call_id"]), "function")
		return emit(ir.StreamEvent{Type: ir.EventToolArgumentsDone, OutputIndex: outputIndex, CallID: identity.callID, ToolName: identity.name, ToolType: identity.toolType, Arguments: stringValue(data["arguments"])})
	case "response.custom_tool_call_input.done":
		identity := reader.tool(stringValue(data["item_id"]), outputIndex, stringValue(data["call_id"]), "custom")
		return emit(ir.StreamEvent{Type: ir.EventToolArgumentsDone, OutputIndex: outputIndex, CallID: identity.callID, ToolName: identity.name, ToolType: identity.toolType, Arguments: stringValue(data["input"])})
	case "response.completed", "response.incomplete", "response.failed":
		reader.terminal = true
		response := objectMap(data["response"])
		if eventType == "response.failed" {
			return emit(ir.StreamEvent{Type: ir.EventFailed, ResponseID: stringValue(response["id"]), Status: "failed", FinishReason: "error", Usage: usageFromResponses(response["usage"]), Raw: cloneRaw(data["response"])})
		}
		status := "completed"
		if eventType == "response.incomplete" {
			status = "incomplete"
		}
		return emit(ir.StreamEvent{
			Type:              ir.EventCompleted,
			ResponseID:        stringValue(response["id"]),
			Model:             stringValue(response["model"]),
			CreatedAt:         intValue(response["created_at"]),
			Status:            status,
			IncompleteDetails: cloneRaw(response["incomplete_details"]),
			Usage:             usageFromResponses(response["usage"]),
			Raw:               cloneRaw(data["response"]),
		})
	default:
		// 托管工具的执行进度事件在 Chat 下游没有对应表达，开启转发时丢弃而不是中断。
		if isHostedToolEvent(eventType) && reader.forwardBuiltIn {
			return nil
		}
		if isUnsupportedResponsesEvent(eventType) {
			return fmt.Errorf("暂不支持 Responses stream event %q", eventType)
		}
		return nil
	}
}

// itemEvent 解析 item 并把工具身份登记下来，供只带 item_id 的 arguments 事件复用。
func (reader *responsesStreamReader) itemEvent(data map[string]json.RawMessage, outputIndex int) (*ir.Item, error) {
	rawItem := objectMap(data["item"])
	item, err := responsesItemToIR(rawItem)
	if err != nil {
		return nil, err
	}
	item.ID = stringValue(rawItem["id"])
	if isToolCallItem(item.Type) {
		identity := reader.tool(item.ID, outputIndex, item.CallID, toolTypeOf(item.Type))
		if item.Name != "" {
			identity.name = item.Name
		}
	}
	return &item, nil
}

// hostedToolEventPrefixes 是厂商托管工具的执行进度事件前缀，Chat 目标无法表达。
var hostedToolEventPrefixes = []string{
	"response.web_search_call.",
	"response.file_search_call.",
	"response.code_interpreter_call.",
	"response.computer_call.",
	"response.image_generation_call.",
	"response.mcp_call.",
	"response.tool_search_call.",
}

func isHostedToolEvent(eventType string) bool {
	for _, prefix := range hostedToolEventPrefixes {
		if strings.HasPrefix(eventType, prefix) {
			return true
		}
	}
	return false
}

func isUnsupportedResponsesEvent(eventType string) bool {
	return strings.HasPrefix(eventType, "response.audio.")
}

// NewStreamWriter 创建把 IR 事件写成 Responses SSE 的写入器。
func (ResponsesCodec) NewStreamWriter(writer io.Writer, options Options) StreamWriter {
	return &responsesStreamWriter{
		sse:       sseWriter{writer: writer},
		options:   options,
		acc:       NewAccumulator(),
		tools:     map[int]*responsesToolState{},
		messageID: newID("msg_"),
	}
}

type responsesToolState struct {
	callID   string
	name     string
	kind     string
	buffer   strings.Builder
	added    bool
	argsDone bool
	itemDone bool
}

type responsesStreamWriter struct {
	sse            sseWriter
	options        Options
	acc            *Accumulator
	sequence       int64
	responseID     string
	model          string
	created        int64
	messageOpen    bool
	messageID      string
	messageIndex   int
	reasoningOpen  bool
	reasoningOwned bool
	reasoningDone  bool
	reasoningID    string
	reasoningIdx   int
	reasoningText  strings.Builder
	messageDone    bool
	messageText    strings.Builder
	contentSent    bool
	tools          map[int]*responsesToolState
	usage          *ir.Usage
	status         string
	incomplete     any
}

func (writer *responsesStreamWriter) Write(event ir.StreamEvent) error {
	if err := writer.acc.Apply(event); err != nil {
		return err
	}
	switch event.Type {
	case ir.EventCreated:
		writer.responseID = event.ResponseID
		if writer.responseID == "" {
			writer.responseID = newID("resp_")
		}
		writer.model = event.Model
		writer.created = event.CreatedAt
		return writer.frame("response.created", map[string]any{
			"response": map[string]any{
				"id":         writer.responseID,
				"object":     "response",
				"created_at": writer.created,
				"model":      writer.model,
				"status":     "in_progress",
				"output":     []any{},
			},
		})
	case ir.EventOutputItemAdded:
		if event.Item == nil {
			return nil
		}
		return writer.itemAdded(event.OutputIndex, *event.Item)
	case ir.EventTextDelta:
		if err := writer.openMessage(event.OutputIndex); err != nil {
			return err
		}
		writer.messageText.WriteString(event.Delta)
		return writer.frame("response.output_text.delta", map[string]any{
			"item_id":       writer.messageID,
			"output_index":  event.OutputIndex,
			"content_index": event.ContentIndex,
			"delta":         event.Delta,
		})
	case ir.EventReasoningDelta:
		if !writer.options.EmitReasoning {
			return nil
		}
		if err := writer.openReasoning(event.OutputIndex); err != nil {
			return err
		}
		writer.reasoningText.WriteString(event.Delta)
		return writer.frame("response.reasoning_summary_text.delta", map[string]any{
			"item_id":       writer.reasoningID,
			"output_index":  writer.reasoningIdx,
			"summary_index": 0,
			"delta":         event.Delta,
		})
	case ir.EventRefusalDelta:
		if err := writer.openMessage(event.OutputIndex); err != nil {
			return err
		}
		return writer.frame("response.refusal.delta", map[string]any{
			"item_id":       writer.messageID,
			"output_index":  event.OutputIndex,
			"content_index": event.ContentIndex,
			"delta":         event.Refusal,
		})
	case ir.EventToolCallStart, ir.EventToolArgumentsDelta:
		tool := writer.toolState(event)
		if event.Type == ir.EventToolArgumentsDelta {
			tool.buffer.WriteString(event.Delta)
		}
		if !tool.added {
			// 名字还没到齐时不发 item 事件，否则会写出上游/客户端都拒绝的空 name。
			if err := writer.ensureToolAdded(event, tool); err != nil {
				return err
			}
			if !tool.added {
				return nil
			}
			if tool.buffer.Len() == 0 {
				return nil
			}
			return writer.frame(writer.toolEvent(tool.kind, "delta"), map[string]any{
				"item_id":      writer.toolItemID(event.OutputIndex),
				"output_index": event.OutputIndex,
				"call_id":      tool.callID,
				"delta":        tool.buffer.String(),
			})
		}
		if event.Type == ir.EventToolArgumentsDelta {
			return writer.frame(writer.toolEvent(tool.kind, "delta"), map[string]any{
				"item_id":      writer.toolItemID(event.OutputIndex),
				"output_index": event.OutputIndex,
				"call_id":      tool.callID,
				"delta":        event.Delta,
			})
		}
		return nil
	case ir.EventToolArgumentsDone:
		tool := writer.toolState(event)
		if err := writer.ensureToolAdded(event, tool); err != nil {
			return err
		}
		tool.argsDone = true
		tool.buffer.Reset()
		tool.buffer.WriteString(event.Arguments)
		return writer.frame(writer.toolEvent(tool.kind, "done"), map[string]any{
			"item_id":           writer.toolItemID(event.OutputIndex),
			"output_index":      event.OutputIndex,
			"call_id":           tool.callID,
			fieldFor(tool.kind): event.Arguments,
		})
	case ir.EventOutputItemDone:
		if event.Item == nil {
			return nil
		}
		switch event.Item.Type {
		case "message":
			writer.messageDone = true
		case "reasoning":
			writer.reasoningDone = true
		}
		if tool := writer.toolFromItem(*event.Item); tool != nil {
			tool.argsDone = true
			tool.itemDone = true
		}
		payload := itemPayload(*event.Item, event.OutputIndex)
		if payload == nil {
			return nil
		}
		return writer.frame("response.output_item.done", map[string]any{"output_index": event.OutputIndex, "item": payload})
	case ir.EventUsage:
		writer.usage = event.Usage
	case ir.EventCompleted:
		writer.status = event.Status
		if event.Usage != nil {
			writer.usage = event.Usage
		}
		if hasJSONValue(event.IncompleteDetails) {
			writer.incomplete = rawToAny(event.IncompleteDetails)
		}
	case ir.EventFailed:
		writer.status = "failed"
	}
	return nil
}

// Close 写出终态事件并返回聚合响应。
func (writer *responsesStreamWriter) Close() (*ir.Response, error) {
	response, err := writer.acc.Response()
	if err != nil {
		return nil, err
	}
	if err := writer.closeTools(); err != nil {
		return nil, err
	}
	if writer.reasoningOpen && !writer.reasoningDone {
		summary := writer.reasoningText.String()
		if writer.reasoningOwned {
			if err := writer.frame("response.reasoning_summary_text.done", map[string]any{
				"item_id":       writer.reasoningID,
				"output_index":  writer.reasoningIdx,
				"summary_index": 0,
				"text":          summary,
			}); err != nil {
				return nil, err
			}
			if err := writer.frame("response.reasoning_summary_part.done", map[string]any{
				"item_id":       writer.reasoningID,
				"output_index":  writer.reasoningIdx,
				"summary_index": 0,
				"part":          map[string]any{"type": "summary_text", "text": summary},
			}); err != nil {
				return nil, err
			}
		}
		item := ir.Item{ID: writer.reasoningID, Type: "reasoning", Role: "assistant", Status: "completed", Content: []ir.ContentPart{{Type: "summary_text", Text: summary}}}
		if err := writer.frame("response.output_item.done", map[string]any{"output_index": writer.reasoningIdx, "item": itemPayload(item, writer.reasoningIdx)}); err != nil {
			return nil, err
		}
	}
	if writer.messageOpen && !writer.messageDone {
		if err := writer.frame("response.output_text.done", map[string]any{"item_id": writer.messageID, "output_index": writer.messageIndex, "content_index": 0, "text": writer.messageText.String()}); err != nil {
			return nil, err
		}
		item := ir.Item{ID: writer.messageID, Type: "message", Role: "assistant", Status: "completed", Content: []ir.ContentPart{{Type: "output_text", Text: writer.messageText.String()}}}
		if err := writer.frame("response.output_item.done", map[string]any{"output_index": writer.messageIndex, "item": itemPayload(item, writer.messageIndex)}); err != nil {
			return nil, err
		}
	}
	status := response.Status
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
	}
	if status == "failed" {
		event = "response.failed"
	}
	payload := map[string]any{"id": responsesResponseID(response), "status": status}
	if response.Usage != nil {
		payload["usage"] = responsesUsage(response.Usage)
	}
	if hasJSONValue(response.IncompleteDetails) {
		payload["incomplete_details"] = rawToAny(response.IncompleteDetails)
	}
	if err := writer.frame(event, map[string]any{"response": payload}); err != nil {
		return nil, err
	}
	return response, nil
}

// openReasoning 为推理增量补上 output_item.added，保持 Responses 事件完整。
func (writer *responsesStreamWriter) openReasoning(outputIndex int) error {
	if writer.reasoningOpen {
		return nil
	}
	writer.reasoningOpen = true
	writer.reasoningOwned = true
	writer.reasoningIdx = outputIndex
	if writer.reasoningID == "" {
		writer.reasoningID = newID("rs_")
	}
	if err := writer.frame("response.output_item.added", map[string]any{
		"output_index": outputIndex,
		"item": map[string]any{
			"id":      writer.reasoningID,
			"type":    "reasoning",
			"summary": []any{},
			"status":  "in_progress",
		},
	}); err != nil {
		return err
	}
	return writer.frame("response.reasoning_summary_part.added", map[string]any{
		"item_id":       writer.reasoningID,
		"output_index":  outputIndex,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text", "text": ""},
	})
}

func (writer *responsesStreamWriter) openMessage(outputIndex int) error {
	if writer.messageOpen {
		return nil
	}
	writer.messageOpen = true
	writer.messageIndex = outputIndex
	if err := writer.frame("response.output_item.added", map[string]any{
		"output_index": outputIndex,
		"item": map[string]any{
			"id":      writer.messageID,
			"type":    "message",
			"role":    "assistant",
			"content": []any{},
			"status":  "in_progress",
		},
	}); err != nil {
		return err
	}
	return writer.frame("response.content_part.added", map[string]any{
		"item_id":       writer.messageID,
		"output_index":  outputIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (writer *responsesStreamWriter) itemAdded(outputIndex int, item ir.Item) error {
	if item.Type == "message" && !writer.messageOpen {
		writer.messageOpen = true
		writer.messageIndex = outputIndex
		writer.messageID = item.ID
	}
	if item.Type == "reasoning" && !writer.reasoningOpen {
		writer.reasoningOpen = true
		writer.reasoningIdx = outputIndex
		writer.reasoningID = item.ID
	}
	if item.Type == "function_call" || item.Type == "custom_tool_call" {
		tool := writer.toolState(ir.StreamEvent{OutputIndex: outputIndex, CallID: item.CallID, ToolName: item.Name, ToolType: toolTypeOf(item.Type)})
		tool.name = item.Name
		tool.callID = item.CallID
	}
	return writer.frame("response.output_item.added", map[string]any{"output_index": outputIndex, "item": itemPayload(item, outputIndex)})
}

// ensureToolAdded 保证工具 item 有 output_item.added 事件，参数增量才不会孤立出现。
func (writer *responsesStreamWriter) ensureToolAdded(event ir.StreamEvent, tool *responsesToolState) error {
	if tool.added || tool.name == "" {
		return nil
	}
	tool.added = true
	item := map[string]any{
		"id":      writer.toolItemID(event.OutputIndex),
		"type":    "function_call",
		"call_id": tool.callID,
		"name":    tool.name,
		"status":  "in_progress",
	}
	if tool.kind == "custom" {
		item["type"] = "custom_tool_call"
		item["input"] = ""
	} else {
		item["arguments"] = ""
	}
	return writer.frame("response.output_item.added", map[string]any{"output_index": event.OutputIndex, "item": item})
}

func (writer *responsesStreamWriter) toolFromItem(item ir.Item) *responsesToolState {
	for _, tool := range writer.tools {
		if item.CallID != "" && tool.callID == item.CallID {
			return tool
		}
	}
	return nil
}

// closeTools 为没有显式 done 事件的来源补齐工具终态。
func (writer *responsesStreamWriter) closeTools() error {
	indices := make([]int, 0, len(writer.tools))
	for index := range writer.tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		tool := writer.tools[index]
		if tool.name == "" {
			// 始终没有 name 的调用无法表达，整个 item 跳过。
			continue
		}
		arguments := tool.buffer.String()
		if !tool.argsDone {
			tool.argsDone = true
			if err := writer.frame(writer.toolEvent(tool.kind, "done"), map[string]any{
				"item_id":           writer.toolItemID(index),
				"output_index":      index,
				"call_id":           tool.callID,
				fieldFor(tool.kind): arguments,
			}); err != nil {
				return err
			}
		}
		if tool.itemDone {
			continue
		}
		item := ir.Item{ID: writer.toolItemID(index), Type: "function_call", CallID: tool.callID, Name: tool.name, Arguments: arguments, Status: "completed"}
		if tool.kind == "custom" {
			item.Type = "custom_tool_call"
			item.Input = arguments
		}
		payload := itemPayload(item, index)
		if payload == nil {
			continue
		}
		if err := writer.frame("response.output_item.done", map[string]any{"output_index": index, "item": payload}); err != nil {
			return err
		}
	}
	return nil
}

func (writer *responsesStreamWriter) toolState(event ir.StreamEvent) *responsesToolState {
	tool, exists := writer.tools[event.OutputIndex]
	if !exists {
		tool = &responsesToolState{callID: event.CallID, name: event.ToolName, kind: event.ToolType}
		writer.tools[event.OutputIndex] = tool
	}
	if event.CallID != "" {
		tool.callID = event.CallID
	}
	if event.ToolName != "" {
		tool.name = event.ToolName
	}
	if event.ToolType != "" {
		tool.kind = event.ToolType
	}
	return tool
}

func (writer *responsesStreamWriter) toolItemID(outputIndex int) string {
	return fmt.Sprintf("fc_%d", outputIndex)
}

func (writer *responsesStreamWriter) toolEvent(toolType, phase string) string {
	if toolType == "custom" {
		return "response.custom_tool_call_input." + phase
	}
	return "response.function_call_arguments." + phase
}

func (writer *responsesStreamWriter) frame(event string, payload map[string]any) error {
	writer.sequence++
	payload["type"] = event
	payload["sequence_number"] = writer.sequence
	return writer.sse.frame(SSEFrame{Event: event, Data: mustJSONString(payload)})
}

func itemPayload(item ir.Item, index int) map[string]any {
	if isToolCallItem(item.Type) && (item.Name == "" || item.CallID == "") {
		return nil
	}
	if len(item.Raw) > 0 {
		if payload, ok := rawToAny(item.Raw).(map[string]any); ok {
			return payload
		}
	}
	switch item.Type {
	case "message":
		content := []any{}
		for _, part := range item.Content {
			if part.Type == "output_text" || part.Type == "text" {
				content = append(content, map[string]any{"type": "output_text", "text": part.Text, "annotations": []any{}})
			}
			if part.Type == "refusal" {
				content = append(content, map[string]any{"type": "refusal", "refusal": part.Refusal})
			}
		}
		return map[string]any{"id": itemID(item, "msg_", index), "type": "message", "role": messageRole(item.Role), "content": content, "status": itemStatus(item)}
	case "reasoning":
		return map[string]any{"id": itemID(item, "rs_", index), "type": "reasoning", "summary": irReasoningSummary(item.Content), "status": itemStatus(item)}
	case "function_call":
		return map[string]any{"id": itemID(item, "fc_", index), "type": "function_call", "call_id": item.CallID, "name": item.Name, "arguments": item.Arguments, "status": itemStatus(item)}
	case "custom_tool_call":
		return map[string]any{"id": itemID(item, "ctc_", index), "type": "custom_tool_call", "call_id": item.CallID, "name": item.Name, "input": item.Input, "status": itemStatus(item)}
	default:
		return map[string]any{"id": itemID(item, "", index), "type": item.Type, "status": itemStatus(item)}
	}
}

func toolTypeOf(itemType string) string {
	if itemType == "custom_tool_call" {
		return "custom"
	}
	return "function"
}

func fieldFor(toolType string) string {
	if toolType == "custom" {
		return "input"
	}
	return "arguments"
}
