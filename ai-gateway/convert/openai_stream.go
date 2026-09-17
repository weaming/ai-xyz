package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/weaming/ai-xyz/ai-gateway/ir"
)

func responsesToChatStream(reader io.Reader, writer io.Writer, emitReasoning bool) error {
	state := newStreamState()
	return ReadSSE(reader, func(frame SSEFrame) error {
		frames, err := state.responsesFrameToChat(frame, emitReasoning)
		if err != nil {
			return err
		}
		return writeFrames(writer, frames)
	})
}

func chatToResponsesStream(reader io.Reader, writer io.Writer) error {
	state := newStreamState()
	return ReadSSE(reader, func(frame SSEFrame) error {
		frames, err := state.chatFrameToResponses(frame)
		if err != nil {
			return err
		}
		return writeFrames(writer, frames)
	})
}

func responsesStreamToResponse(reader io.Reader) ([]byte, error) {
	state := newResponsesStreamState()
	if err := ReadSSE(reader, state.consume); err != nil {
		return nil, err
	}
	return state.response()
}

func chatStreamToResponse(reader io.Reader) ([]byte, error) {
	state := newChatStreamState()
	if err := ReadSSE(reader, state.consume); err != nil {
		return nil, err
	}
	return state.response()
}

func responsesStreamToChatResponse(reader io.Reader, emitReasoning bool) ([]byte, error) {
	var converted bytes.Buffer
	if err := responsesToChatStream(reader, &converted, emitReasoning); err != nil {
		return nil, err
	}
	return chatStreamToResponse(&converted)
}

func chatStreamToResponsesResponse(reader io.Reader) ([]byte, error) {
	var converted bytes.Buffer
	if err := chatToResponsesStream(reader, &converted); err != nil {
		return nil, err
	}
	return responsesStreamToResponse(&converted)
}

type responsesStreamState struct {
	payload   map[string]any
	items     map[int]map[string]any
	itemByID  map[string]int
	nextIndex int
}

func newResponsesStreamState() *responsesStreamState {
	return &responsesStreamState{
		items:    make(map[int]map[string]any),
		itemByID: make(map[string]int),
	}
}

func (state *responsesStreamState) consume(frame SSEFrame) error {
	if frame.Data == "[DONE]" {
		return nil
	}
	data, err := rawMap(frame.Data)
	if err != nil {
		return fmt.Errorf("解析 Responses SSE: %w", err)
	}
	eventType := stringValue(data["type"])
	if eventType == "" {
		eventType = frame.Event
	}
	switch eventType {
	case "response.created", "response.in_progress", "response.completed", "response.failed", "response.incomplete":
		if response := rawToAny(data["response"]); response != nil {
			if value, ok := response.(map[string]any); ok {
				state.payload = value
			}
		}
	case "response.output_item.added", "response.output_item.done":
		state.setItem(data["item"], int(intValue(data["output_index"])))
	case "response.output_text.delta":
		item := state.item(data, "message")
		appendItemText(item, "output_text", stringValue(data["delta"]))
	case "response.refusal.delta":
		item := state.item(data, "message")
		appendItemText(item, "refusal", stringValue(data["delta"]))
	case "response.function_call_arguments.delta":
		item := state.item(data, "function_call")
		item["arguments"] = stringValueValue(item["arguments"]) + stringValue(data["delta"])
	case "response.custom_tool_call_input.delta":
		item := state.item(data, "custom_tool_call")
		item["input"] = stringValueValue(item["input"]) + stringValue(data["delta"])
	}
	return nil
}

func (state *responsesStreamState) setItem(raw json.RawMessage, outputIndex int) {
	item, ok := rawToAny(raw).(map[string]any)
	if !ok {
		return
	}
	if id, ok := item["id"].(string); ok && id != "" {
		if existingIndex, exists := state.itemByID[id]; exists {
			outputIndex = existingIndex
		}
		state.itemByID[id] = outputIndex
	}
	if outputIndex >= state.nextIndex {
		state.nextIndex = outputIndex + 1
	}
	state.items[outputIndex] = item
}

func (state *responsesStreamState) item(data map[string]json.RawMessage, itemType string) map[string]any {
	outputIndex := int(intValue(data["output_index"]))
	if itemID := stringValue(data["item_id"]); itemID != "" {
		if index, exists := state.itemByID[itemID]; exists {
			outputIndex = index
		}
	}
	if item := state.items[outputIndex]; item != nil {
		return item
	}
	itemID := stringValue(data["item_id"])
	if itemID == "" {
		itemID = fmt.Sprintf("item_%d", outputIndex)
	}
	item := map[string]any{
		"id":     itemID,
		"type":   itemType,
		"status": "completed",
	}
	if itemType == "message" {
		item["role"] = "assistant"
		item["content"] = []any{}
	}
	state.items[outputIndex] = item
	state.itemByID[itemID] = outputIndex
	if outputIndex >= state.nextIndex {
		state.nextIndex = outputIndex + 1
	}
	return item
}

func (state *responsesStreamState) response() ([]byte, error) {
	if state.payload == nil {
		return nil, fmt.Errorf("Responses SSE 缺少完成响应")
	}
	if len(state.items) > 0 {
		indices := make([]int, 0, len(state.items))
		for index := range state.items {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		output := make([]any, 0, len(indices))
		for _, index := range indices {
			output = append(output, state.items[index])
		}
		state.payload["output"] = output
	}
	return json.Marshal(state.payload)
}

func appendItemText(item map[string]any, partType, delta string) {
	if delta == "" {
		return
	}
	content, _ := item["content"].([]any)
	for _, rawPart := range content {
		part, ok := rawPart.(map[string]any)
		if !ok || part["type"] != partType {
			continue
		}
		part["text"] = stringValueValue(part["text"]) + delta
		item["content"] = content
		return
	}
	content = append(content, map[string]any{"type": partType, "text": delta})
	item["content"] = content
}

func stringValueValue(value any) string {
	text, _ := value.(string)
	return text
}

type chatStreamState struct {
	id        string
	model     string
	created   int64
	content   strings.Builder
	refusal   strings.Builder
	finish    string
	usage     *ir.Usage
	toolCalls map[int]*chatToolState
}

type chatToolState struct {
	id        string
	name      string
	arguments strings.Builder
}

func newChatStreamState() *chatStreamState {
	return &chatStreamState{toolCalls: make(map[int]*chatToolState)}
}

func (state *chatStreamState) consume(frame SSEFrame) error {
	if frame.Data == "[DONE]" {
		return nil
	}
	data, err := rawMap(frame.Data)
	if err != nil {
		return fmt.Errorf("解析 Chat SSE: %w", err)
	}
	if state.id == "" {
		state.id = stringValue(data["id"])
	}
	if state.model == "" {
		state.model = stringValue(data["model"])
	}
	if state.created == 0 {
		state.created = intValue(data["created"])
	}
	if usage := usageFromChat(data["usage"]); usage != nil {
		state.usage = usage
	}
	for _, choice := range rawList(data["choices"]) {
		delta := objectMap(choice["delta"])
		state.content.WriteString(stringValue(delta["content"]))
		state.refusal.WriteString(stringValue(delta["refusal"]))
		if finish := stringValue(choice["finish_reason"]); finish != "" {
			state.finish = finish
		}
		for _, rawCall := range rawList(delta["tool_calls"]) {
			index := int(intValue(rawCall["index"]))
			tool := state.toolCalls[index]
			if tool == nil {
				tool = &chatToolState{}
				state.toolCalls[index] = tool
			}
			if id := stringValue(rawCall["id"]); id != "" {
				tool.id = id
			}
			function := objectMap(rawCall["function"])
			if name := stringValue(function["name"]); name != "" {
				tool.name = name
			}
			tool.arguments.WriteString(stringValue(function["arguments"]))
		}
	}
	return nil
}

func (state *chatStreamState) response() ([]byte, error) {
	message := map[string]any{"role": "assistant", "content": nil}
	if state.content.Len() > 0 {
		message["content"] = state.content.String()
	}
	if state.refusal.Len() > 0 {
		message["refusal"] = state.refusal.String()
		if state.finish == "" {
			state.finish = "refusal"
		}
	}
	indices := make([]int, 0, len(state.toolCalls))
	for index := range state.toolCalls {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	toolCalls := make([]any, 0, len(indices))
	for _, index := range indices {
		tool := state.toolCalls[index]
		toolCalls = append(toolCalls, map[string]any{
			"id":   tool.id,
			"type": "function",
			"function": map[string]any{
				"name":      tool.name,
				"arguments": tool.arguments.String(),
			},
		})
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if state.finish == "" {
			state.finish = "tool_calls"
		}
	}
	if state.finish == "" {
		state.finish = "stop"
	}
	result := map[string]any{
		"id":      state.id,
		"object":  "chat.completion",
		"created": state.created,
		"model":   state.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": state.finish,
		}},
	}
	if state.usage != nil {
		result["usage"] = chatUsage(state.usage)
	}
	return json.Marshal(result)
}

type streamState struct {
	responseID  string
	model       string
	created     int64
	sequence    int64
	started     bool
	messageOpen bool
	messageID   string
	messageText strings.Builder
	refusal     strings.Builder
	toolByKey   map[string]*streamTool
	tools       []*streamTool
	usage       *ir.Usage
	finish      string
	completed   bool
}

type streamTool struct {
	itemID      string
	callID      string
	name        string
	arguments   strings.Builder
	outputIndex int
}

func newStreamState() *streamState {
	return &streamState{toolByKey: make(map[string]*streamTool)}
}

func (state *streamState) responsesFrameToChat(frame SSEFrame, emitReasoning bool) ([]SSEFrame, error) {
	if frame.Data == "[DONE]" {
		return nil, nil
	}
	data, err := rawMap(frame.Data)
	if err != nil {
		return nil, fmt.Errorf("解析 Responses SSE: %w", err)
	}
	eventType := stringValue(data["type"])
	if eventType == "" {
		eventType = frame.Event
	}
	switch eventType {
	case "response.created":
		response := objectMap(data["response"])
		state.setResponse(response)
		state.started = true
		return []SSEFrame{{Data: mustJSONString(chatChunk(state, map[string]any{"role": "assistant"}, nil, ""))}}, nil
	case "response.output_item.added":
		item := objectMap(data["item"])
		if stringValue(item["type"]) != "function_call" && stringValue(item["type"]) != "custom_tool_call" {
			return nil, nil
		}
		tool := state.addResponseTool(item, int(intValue(data["output_index"])))
		call := map[string]any{"index": toolIndex(state, tool), "id": tool.callID, "type": "function", "function": map[string]any{"name": tool.name, "arguments": ""}}
		return []SSEFrame{{Data: mustJSONString(chatChunk(state, nil, []any{call}, ""))}}, nil
	case "response.output_text.delta":
		return []SSEFrame{{Data: mustJSONString(chatChunk(state, map[string]any{"content": stringValue(data["delta"])}, nil, ""))}}, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if !emitReasoning {
			return nil, nil
		}
		return []SSEFrame{{Data: mustJSONString(chatChunk(state, map[string]any{"reasoning_content": stringValue(data["delta"])}, nil, ""))}}, nil
	case "response.refusal.delta":
		return []SSEFrame{{Data: mustJSONString(chatChunk(state, map[string]any{"refusal": stringValue(data["delta"])}, nil, ""))}}, nil
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		tool := state.responseTool(data)
		if tool == nil {
			return nil, fmt.Errorf("Responses 工具增量缺少可识别的 output item")
		}
		delta := stringValue(data["delta"])
		tool.arguments.WriteString(delta)
		call := map[string]any{"index": toolIndex(state, tool), "function": map[string]any{"arguments": delta}}
		return []SSEFrame{{Data: mustJSONString(chatChunk(state, nil, []any{call}, ""))}}, nil
	case "response.completed":
		response := objectMap(data["response"])
		state.setResponse(response)
		return state.completeChat(false), nil
	case "response.failed", "response.incomplete":
		return state.completeChat(true), nil
	default:
		return nil, nil
	}
}

func (state *streamState) chatFrameToResponses(frame SSEFrame) ([]SSEFrame, error) {
	if frame.Data == "[DONE]" {
		return state.completeResponses(), nil
	}
	data, err := rawMap(frame.Data)
	if err != nil {
		return nil, fmt.Errorf("解析 Chat SSE: %w", err)
	}
	isFirstFrame := !state.started
	if isFirstFrame {
		state.responseID = stringValue(data["id"])
		if state.responseID == "" {
			state.responseID = "resp_" + strconv.FormatInt(time.Now().UnixNano(), 10)
		}
		state.model = stringValue(data["model"])
		state.created = intValue(data["created"])
		if state.created == 0 {
			state.created = time.Now().Unix()
		}
		state.started = true
	}
	frames := []SSEFrame{}
	if isFirstFrame {
		frames = append(frames, state.responsesFrame("response.created", map[string]any{"response": state.responseObject("in_progress")}))
	}
	choices := rawList(data["choices"])
	if len(choices) == 0 {
		if usage := usageFromChat(data["usage"]); usage != nil {
			state.usage = usage
		}
		return frames, nil
	}
	for _, choice := range choices {
		delta := objectMap(choice["delta"])
		if content := stringValue(delta["content"]); content != "" {
			frames = append(frames, state.openMessage()...)
			state.messageText.WriteString(content)
			frames = append(frames, state.responsesFrame("response.output_text.delta", map[string]any{"item_id": state.messageID, "output_index": 0, "content_index": 0, "delta": content}))
		}
		if refusal := stringValue(delta["refusal"]); refusal != "" {
			frames = append(frames, state.openMessage()...)
			state.refusal.WriteString(refusal)
			frames = append(frames, state.responsesFrame("response.refusal.delta", map[string]any{"item_id": state.messageID, "output_index": 0, "content_index": 0, "delta": refusal}))
		}
		if reasoning := stringValue(delta["reasoning_content"]); reasoning != "" {
			frames = append(frames, state.responsesFrame("response.reasoning_summary_text.delta", map[string]any{"item_id": state.messageID, "delta": reasoning}))
		}
		for _, toolCall := range rawList(delta["tool_calls"]) {
			frames = append(frames, state.chatToolFrames(toolCall)...)
		}
		if finish := stringValue(choice["finish_reason"]); finish != "" {
			state.finish = finish
		}
	}
	if usage := usageFromChat(data["usage"]); usage != nil {
		state.usage = usage
	}
	return frames, nil
}

func (state *streamState) setResponse(response map[string]json.RawMessage) {
	if response == nil {
		return
	}
	if id := stringValue(response["id"]); id != "" {
		state.responseID = id
	}
	if model := stringValue(response["model"]); model != "" {
		state.model = model
	}
	state.created = intValue(response["created_at"])
	if state.created == 0 {
		state.created = intValue(response["created"])
	}
	if usage := usageFromResponses(response["usage"]); usage != nil {
		state.usage = usage
	}
}

func (state *streamState) addResponseTool(item map[string]json.RawMessage, outputIndex int) *streamTool {
	key := stringValue(item["id"])
	if key == "" {
		key = stringValue(item["call_id"])
	}
	if existing := state.toolByKey[key]; existing != nil {
		return existing
	}
	tool := &streamTool{itemID: stringValue(item["id"]), callID: stringValue(item["call_id"]), name: stringValue(item["name"]), outputIndex: outputIndex}
	state.toolByKey[key] = tool
	state.tools = append(state.tools, tool)
	return tool
}

func (state *streamState) responseTool(data map[string]json.RawMessage) *streamTool {
	itemID := stringValue(data["item_id"])
	callID := stringValue(data["call_id"])
	if tool := state.toolByKey[itemID]; tool != nil {
		return tool
	}
	if tool := state.toolByKey[callID]; tool != nil {
		return tool
	}
	return nil
}

func (state *streamState) openMessage() []SSEFrame {
	if state.messageOpen {
		return nil
	}
	state.messageOpen = true
	if state.messageID == "" {
		state.messageID = "msg_" + strings.TrimPrefix(state.responseID, "resp_")
	}
	return []SSEFrame{
		state.responsesFrame("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": state.messageID, "type": "message", "role": "assistant", "content": []any{}, "status": "in_progress"}}),
		state.responsesFrame("response.content_part.added", map[string]any{"output_index": 0, "content_index": 0, "item_id": state.messageID, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}),
	}
}

func (state *streamState) chatToolFrames(raw map[string]json.RawMessage) []SSEFrame {
	function := objectMap(raw["function"])
	key := stringValue(raw["id"])
	indexKey := "index:" + strconv.FormatInt(intValue(raw["index"]), 10)
	if key == "" {
		key = indexKey
	}
	tool := state.toolByKey[key]
	if tool == nil {
		tool = state.toolByKey[indexKey]
	}
	isNew := tool == nil
	if tool == nil {
		callID := stringValue(raw["id"])
		if callID == "" {
			callID = "call_" + strconv.Itoa(len(state.tools))
		}
		outputIndex := len(state.tools)
		if state.messageOpen {
			outputIndex++
		}
		tool = &streamTool{itemID: "fc_" + callID, callID: callID, name: stringValue(function["name"]), outputIndex: outputIndex}
		state.toolByKey[key] = tool
		state.toolByKey[indexKey] = tool
		state.tools = append(state.tools, tool)
	}
	frames := []SSEFrame{}
	if tool.name == "" {
		tool.name = stringValue(function["name"])
	}
	if isNew {
		frames = append(frames, state.responsesFrame("response.output_item.added", map[string]any{"output_index": tool.outputIndex, "item": map[string]any{"id": tool.itemID, "type": "function_call", "call_id": tool.callID, "name": tool.name, "arguments": "", "status": "in_progress"}}))
	}
	if arguments := stringValue(function["arguments"]); arguments != "" {
		tool.arguments.WriteString(arguments)
		frames = append(frames, state.responsesFrame("response.function_call_arguments.delta", map[string]any{"item_id": tool.itemID, "output_index": tool.outputIndex, "call_id": tool.callID, "delta": arguments}))
	}
	return frames
}

func (state *streamState) completeChat(failed bool) []SSEFrame {
	if state.completed {
		return nil
	}
	state.completed = true
	finish := state.finish
	if finish == "" {
		if len(state.tools) > 0 {
			finish = "tool_calls"
		} else if failed {
			finish = "error"
		} else {
			finish = "stop"
		}
	}
	frames := []SSEFrame{}
	if state.usage != nil {
		frames = append(frames, SSEFrame{Data: mustJSONString(chatChunk(state, nil, nil, ""))})
		usageChunk := chatChunk(state, nil, nil, "")
		usageChunk["usage"] = chatUsage(state.usage)
		frames[len(frames)-1] = SSEFrame{Data: mustJSONString(usageChunk)}
	}
	frames = append(frames, SSEFrame{Data: mustJSONString(chatChunk(state, nil, nil, finish))}, SSEFrame{Data: "[DONE]"})
	return frames
}

func (state *streamState) completeResponses() []SSEFrame {
	if state.completed {
		return nil
	}
	state.completed = true
	frames := []SSEFrame{}
	if state.messageOpen {
		frames = append(frames,
			state.responsesFrame("response.output_text.done", map[string]any{"item_id": state.messageID, "output_index": 0, "content_index": 0, "text": state.messageText.String()}),
			state.responsesFrame("response.content_part.done", map[string]any{"item_id": state.messageID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": state.messageText.String()}}),
			state.responsesFrame("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{"id": state.messageID, "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": state.messageText.String(), "annotations": []any{}}}, "status": "completed"}}),
		)
	}
	for _, tool := range state.tools {
		frames = append(frames,
			state.responsesFrame("response.function_call_arguments.done", map[string]any{"item_id": tool.itemID, "output_index": tool.outputIndex, "call_id": tool.callID, "arguments": tool.arguments.String()}),
			state.responsesFrame("response.output_item.done", map[string]any{"output_index": tool.outputIndex, "item": map[string]any{"id": tool.itemID, "type": "function_call", "call_id": tool.callID, "name": tool.name, "arguments": tool.arguments.String(), "status": "completed"}}),
		)
	}
	frames = append(frames, state.responsesFrame("response.completed", map[string]any{"response": state.responseObject("completed")}))
	return frames
}

func (state *streamState) responseObject(status string) map[string]any {
	output := []any{}
	if state.messageOpen {
		output = append(output, map[string]any{"id": state.messageID, "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": state.messageText.String(), "annotations": []any{}}}, "status": status})
	}
	for _, tool := range state.tools {
		output = append(output, map[string]any{"id": tool.itemID, "type": "function_call", "call_id": tool.callID, "name": tool.name, "arguments": tool.arguments.String(), "status": status})
	}
	result := map[string]any{"id": state.responseID, "object": "response", "created_at": state.created, "model": state.model, "status": status, "output": output}
	if state.usage != nil {
		result["usage"] = responsesUsage(state.usage)
	}
	return result
}

func (state *streamState) responsesFrame(event string, payload map[string]any) SSEFrame {
	state.sequence++
	payload["type"] = event
	payload["sequence_number"] = state.sequence
	return SSEFrame{Event: event, Data: mustJSONString(payload)}
}

func writeFrames(writer io.Writer, frames []SSEFrame) error {
	for _, frame := range frames {
		if err := WriteSSE(writer, frame); err != nil {
			return err
		}
		if flusher, ok := writer.(interface{ Flush() }); ok {
			flusher.Flush()
		}
	}
	return nil
}

func chatChunk(state *streamState, delta map[string]any, toolCalls []any, finish string) map[string]any {
	choice := map[string]any{"index": 0, "delta": deltaOrEmpty(delta), "finish_reason": nil}
	if len(toolCalls) > 0 {
		choice["delta"].(map[string]any)["tool_calls"] = toolCalls
	}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	return map[string]any{"id": state.responseID, "object": "chat.completion.chunk", "created": state.created, "model": state.model, "choices": []any{choice}}
}

func toolIndex(state *streamState, tool *streamTool) int {
	for index, candidate := range state.tools {
		if candidate == tool {
			return index
		}
	}
	return 0
}
