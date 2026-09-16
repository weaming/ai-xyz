package convert

import (
	"bufio"
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

// Mode 控制无法映射字段的处理方式。
type Mode string

const (
	ModePortable Mode = "portable"
	ModePreserve Mode = "preserve"
	ModeStrict   Mode = "strict"
)

// ChatRequestToResponses 将 Chat Completions 请求转换为 Responses 请求。
func ChatRequestToResponses(input []byte, defaultModel string) ([]byte, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(input, &source); err != nil {
		return nil, fmt.Errorf("解析 Chat 请求: %w", err)
	}

	target := make(map[string]json.RawMessage)
	copyKnown(source, target, "model", "temperature", "top_p", "stop", "stream", "tools", "tool_choice", "parallel_tool_calls", "store")
	copyIfPresent(source, target, "max_completion_tokens", "max_output_tokens")
	copyIfPresent(source, target, "response_format", "text")
	copyIfPresent(source, target, "reasoning_effort", "reasoning")
	if _, exists := target["model"]; !exists && defaultModel != "" {
		target["model"] = rawString(defaultModel)
	}

	messages, err := decodeRawList(source["messages"])
	if err != nil {
		return nil, fmt.Errorf("解析 Chat messages: %w", err)
	}
	inputItems, instructions, err := chatMessagesToResponses(messages)
	if err != nil {
		return nil, err
	}
	if len(instructions) > 0 {
		target["instructions"] = rawString(joinText(instructions))
	}
	target["input"] = mustJSON(inputItems)
	normalizeChatToolsToResponses(target)
	normalizeChatChoiceToResponses(target)
	normalizeChatGenerationToResponses(source, target)
	return json.Marshal(target)
}

// ResponsesRequestToChat 将 Responses 请求转换为 Chat Completions 请求。
func ResponsesRequestToChat(input []byte, defaultModel string, mode Mode) ([]byte, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(input, &source); err != nil {
		return nil, fmt.Errorf("解析 Responses 请求: %w", err)
	}
	if raw := source["previous_response_id"]; len(raw) > 0 && string(raw) != "null" {
		if mode == ModeStrict {
			return nil, fmt.Errorf("Chat Completions 不支持 previous_response_id")
		}
	}

	target := make(map[string]json.RawMessage)
	copyKnown(source, target, "temperature", "top_p", "stop", "stream", "tools", "tool_choice", "parallel_tool_calls")
	copyIfPresent(source, target, "max_output_tokens", "max_completion_tokens")
	copyIfPresent(source, target, "text", "response_format")
	copyIfPresent(source, target, "reasoning", "reasoning_effort")
	if model, exists := source["model"]; exists {
		target["model"] = bytes.Clone(model)
	} else if defaultModel != "" {
		target["model"] = rawString(defaultModel)
	}

	instructions, err := responseInstructions(source["instructions"])
	if err != nil {
		return nil, err
	}
	items, err := decodeResponseInput(source["input"])
	if err != nil {
		return nil, err
	}
	messages, err := responsesItemsToChatMessages(items, mode)
	if err != nil {
		return nil, err
	}
	if len(instructions) > 0 {
		messages = append([]map[string]any{{"role": "system", "content": instructions}}, messages...)
	}
	target["messages"] = mustJSON(messages)
	normalizeResponsesToolsToChat(target)
	normalizeResponsesChoiceToChat(target)
	normalizeResponsesGenerationToChat(source, target)
	return json.Marshal(target)
}

// ResponsesResponseToChat 将 Responses 响应投影为 Chat Completion 响应。
func ResponsesResponseToChat(input []byte) ([]byte, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(input, &source); err != nil {
		return nil, fmt.Errorf("解析 Responses 响应: %w", err)
	}
	response := responseMapToChat(source)
	return json.Marshal(response)
}

// ChatResponseToResponses 将 Chat Completion 响应转换为 Responses 响应。
func ChatResponseToResponses(input []byte) ([]byte, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(input, &source); err != nil {
		return nil, fmt.Errorf("解析 Chat 响应: %w", err)
	}
	response := chatMapToResponses(source)
	return json.Marshal(response)
}

// SSEFrame 表示一个完整的 SSE frame。
type SSEFrame struct {
	Event string
	Data  string
}

// ReadSSE 按 frame 读取 SSE，允许 data 跨多行，并保留 event 名称。
func ReadSSE(reader io.Reader, handle func(SSEFrame) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 16*1024*1024)
	var event string
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		frame := SSEFrame{Event: event, Data: strings.Join(data, "\n")}
		event = ""
		data = nil
		return handle(frame)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取 SSE: %w", err)
	}
	return flush()
}

// WriteSSE 写出一个 SSE frame。
func WriteSSE(writer io.Writer, frame SSEFrame) error {
	if frame.Event != "" {
		if _, err := fmt.Fprintf(writer, "event: %s\n", frame.Event); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(frame.Data, "\n") {
		if _, err := fmt.Fprintf(writer, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(writer, "\n")
	return err
}

// ResponsesToChatStream 将一个 Responses SSE 流转换为 Chat SSE 流。
func ResponsesToChatStream(reader io.Reader, writer io.Writer, emitReasoning bool) error {
	state := newStreamState()
	return ReadSSE(reader, func(frame SSEFrame) error {
		frames, err := state.responsesFrameToChat(frame, emitReasoning)
		if err != nil {
			return err
		}
		return writeFrames(writer, frames)
	})
}

// ChatToResponsesStream 将一个 Chat SSE 流转换为 Responses SSE 流。
func ChatToResponsesStream(reader io.Reader, writer io.Writer) error {
	state := newStreamState()
	return ReadSSE(reader, func(frame SSEFrame) error {
		frames, err := state.chatFrameToResponses(frame)
		if err != nil {
			return err
		}
		return writeFrames(writer, frames)
	})
}

// ResponsesStreamToResponse 将 Responses SSE 流聚合为单个 Responses 响应。
func ResponsesStreamToResponse(reader io.Reader) ([]byte, error) {
	state := newResponsesStreamState()
	err := ReadSSE(reader, state.consume)
	if err != nil {
		return nil, err
	}
	return state.response()
}

// ChatStreamToResponse 将 Chat Completions SSE 流聚合为单个 Chat 响应。
func ChatStreamToResponse(reader io.Reader) ([]byte, error) {
	state := newChatStreamState()
	if err := ReadSSE(reader, state.consume); err != nil {
		return nil, err
	}
	return state.response()
}

// ResponsesStreamToChatResponse 将 Responses SSE 流转换并聚合为 Chat 响应。
func ResponsesStreamToChatResponse(reader io.Reader, emitReasoning bool) ([]byte, error) {
	var converted bytes.Buffer
	if err := ResponsesToChatStream(reader, &converted, emitReasoning); err != nil {
		return nil, err
	}
	return ChatStreamToResponse(&converted)
}

// ChatStreamToResponsesResponse 将 Chat Completions SSE 流转换并聚合为 Responses 响应。
func ChatStreamToResponsesResponse(reader io.Reader) ([]byte, error) {
	var converted bytes.Buffer
	if err := ChatToResponsesStream(reader, &converted); err != nil {
		return nil, err
	}
	return ResponsesStreamToResponse(&converted)
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

func responseMapToChat(source map[string]json.RawMessage) map[string]any {
	message := map[string]any{"role": "assistant"}
	content := ""
	refusal := ""
	toolCalls := []any{}
	finish := "stop"
	for _, rawItem := range rawList(source["output"]) {
		typeName := stringValue(rawItem["type"])
		switch typeName {
		case "message":
			for _, part := range rawList(rawItem["content"]) {
				switch stringValue(part["type"]) {
				case "output_text":
					content += stringValue(part["text"])
				case "refusal":
					refusal += stringValue(part["refusal"])
				}
			}
		case "function_call", "custom_tool_call":
			toolCalls = append(toolCalls, map[string]any{"id": stringValue(rawItem["call_id"]), "type": "function", "function": map[string]any{"name": stringValue(rawItem["name"]), "arguments": stringValue(rawItem["arguments"])}})
			finish = "tool_calls"
		}
	}
	if content != "" {
		message["content"] = content
	} else {
		message["content"] = nil
	}
	if refusal != "" {
		message["refusal"] = refusal
		finish = "refusal"
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	choice := map[string]any{"index": 0, "message": message, "finish_reason": finish}
	if stringValue(source["status"]) == "incomplete" {
		if details := objectMap(source["incomplete_details"]); stringValue(details["reason"]) == "max_output_tokens" {
			choice["finish_reason"] = "length"
		}
	}
	result := map[string]any{"id": stringValue(source["id"]), "object": "chat.completion", "created": intValue(source["created_at"]), "model": stringValue(source["model"]), "choices": []any{choice}}
	if usage := usageFromResponses(source["usage"]); usage != nil {
		result["usage"] = chatUsage(usage)
	}
	return result
}

func chatMapToResponses(source map[string]json.RawMessage) map[string]any {
	choices := rawList(source["choices"])
	output := []any{}
	for _, choice := range choices {
		message := objectMap(choice["message"])
		content := []any{}
		if text := stringValue(message["content"]); text != "" {
			content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
		}
		if refusal := stringValue(message["refusal"]); refusal != "" {
			content = append(content, map[string]any{"type": "refusal", "refusal": refusal})
		}
		if len(content) > 0 {
			output = append(output, map[string]any{"id": "msg_" + stringValue(source["id"]), "type": "message", "role": "assistant", "content": content, "status": "completed"})
		}
		for _, toolCall := range rawList(message["tool_calls"]) {
			function := objectMap(toolCall["function"])
			output = append(output, map[string]any{"id": stringValue(toolCall["id"]), "type": "function_call", "call_id": stringValue(toolCall["id"]), "name": stringValue(function["name"]), "arguments": stringValue(function["arguments"]), "status": "completed"})
		}
	}
	result := map[string]any{"id": "resp_" + stringValue(source["id"]), "object": "response", "created_at": intValue(source["created"]), "model": stringValue(source["model"]), "status": "completed", "output": output}
	if usage := usageFromChat(source["usage"]); usage != nil {
		result["usage"] = responsesUsage(usage)
	}
	return result
}

func chatMessagesToResponses(messages []map[string]json.RawMessage) ([]map[string]any, []string, error) {
	items := []map[string]any{}
	instructions := []string{}
	for _, message := range messages {
		role := stringValue(message["role"])
		content, err := chatContentToResponses(message["content"], role)
		if err != nil {
			return nil, nil, err
		}
		switch role {
		case "system", "developer":
			instructions = append(instructions, contentText(content))
		case "tool":
			items = append(items, map[string]any{"type": "function_call_output", "call_id": stringValue(message["tool_call_id"]), "output": contentText(content)})
		default:
			item := map[string]any{"type": "message", "role": role, "content": content}
			if name := stringValue(message["name"]); name != "" {
				item["name"] = name
			}
			items = append(items, item)
		}
		for _, call := range rawList(message["tool_calls"]) {
			function := objectMap(call["function"])
			items = append(items, map[string]any{"type": "function_call", "call_id": stringValue(call["id"]), "name": stringValue(function["name"]), "arguments": stringValue(function["arguments"])})
		}
	}
	return items, instructions, nil
}

func responsesItemsToChatMessages(items []map[string]json.RawMessage, mode Mode) ([]map[string]any, error) {
	messages := []map[string]any{}
	for _, item := range items {
		switch stringValue(item["type"]) {
		case "message":
			message := map[string]any{"role": stringValue(item["role"]), "content": responseContentToChat(item["content"])}
			if message["role"] == "" {
				message["role"] = "user"
			}
			messages = append(messages, message)
		case "function_call":
			message := lastAssistantMessage(messages)
			if message == nil {
				message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{}}
				messages = append(messages, message)
			}
			calls, _ := message["tool_calls"].([]any)
			calls = append(calls, map[string]any{"id": stringValue(item["call_id"]), "type": "function", "function": map[string]any{"name": stringValue(item["name"]), "arguments": stringValue(item["arguments"])}})
			message["tool_calls"] = calls
		case "function_call_output":
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": stringValue(item["call_id"]), "content": stringValue(item["output"])})
		case "reasoning":
			if mode == ModeStrict {
				return nil, fmt.Errorf("Chat Completions 无法表达 Responses reasoning item")
			}
		case "item_reference":
			if mode == ModeStrict {
				return nil, fmt.Errorf("Chat Completions 无法表达 Responses item_reference")
			}
		default:
			if mode == ModeStrict {
				return nil, fmt.Errorf("Chat Completions 无法表达 Responses item type %q", stringValue(item["type"]))
			}
		}
	}
	return messages, nil
}

func responseInstructions(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if text := stringValue(raw); text != "" {
		return text, nil
	}
	items := rawList(raw)
	parts := []string{}
	for _, item := range items {
		if stringValue(item["type"]) == "message" {
			parts = append(parts, contentTextValue(item["content"]))
		}
	}
	return joinText(parts), nil
}

func chatContentToResponses(raw json.RawMessage, role string) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []map[string]any{}, nil
	}
	if text := stringValue(raw); text != "" {
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		return []map[string]any{{"type": contentType, "text": text}}, nil
	}
	parts := []map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("解析消息 content: %w", err)
	}
	result := make([]map[string]any, 0, len(parts))
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	for _, part := range parts {
		switch stringValue(part["type"]) {
		case "text", "input_text":
			result = append(result, map[string]any{"type": contentType, "text": stringValue(part["text"])})
		case "output_text":
			result = append(result, map[string]any{"type": contentType, "text": stringValue(part["text"])})
		case "image_url", "input_image":
			image := objectMap(part["image_url"])
			url := stringValue(part["image_url"])
			if url == "" {
				url = stringValue(image["url"])
			}
			result = append(result, map[string]any{"type": "input_image", "image_url": url, "detail": stringValue(part["detail"])})
		case "input_file", "file":
			result = append(result, rawMapToAny(part))
		default:
			result = append(result, rawMapToAny(part))
		}
	}
	return result, nil
}

func normalizeChatToolsToResponses(target map[string]json.RawMessage) {
	tools := rawList(target["tools"])
	for _, tool := range tools {
		function := objectMap(tool["function"])
		if len(function) == 0 {
			continue
		}
		for key, value := range function {
			if key != "type" {
				tool[key] = value
			}
		}
		delete(tool, "function")
	}
	target["tools"] = mustJSON(tools)
}

func normalizeResponsesToolsToChat(target map[string]json.RawMessage) {
	tools := rawList(target["tools"])
	for _, tool := range tools {
		if stringValue(tool["type"]) != "function" {
			continue
		}
		function := map[string]any{}
		for _, key := range []string{"name", "description", "parameters", "strict"} {
			if value, exists := tool[key]; exists {
				function[key] = rawToAny(value)
			}
		}
		tool["function"] = mustJSON(function)
		delete(tool, "name")
		delete(tool, "description")
		delete(tool, "parameters")
		delete(tool, "strict")
	}
	target["tools"] = mustJSON(tools)
}

func normalizeChatChoiceToResponses(target map[string]json.RawMessage) {
	if choice := target["tool_choice"]; len(choice) > 0 {
		var value any
		if json.Unmarshal(choice, &value) == nil {
			if object, ok := value.(map[string]any); ok {
				if function, ok := object["function"].(map[string]any); ok {
					target["tool_choice"] = mustJSON(map[string]any{"type": "function", "name": function["name"]})
				}
			}
		}
	}
}

func normalizeResponsesChoiceToChat(target map[string]json.RawMessage) {
	if choice := target["tool_choice"]; len(choice) > 0 {
		var value any
		if json.Unmarshal(choice, &value) == nil {
			if object, ok := value.(map[string]any); ok && object["type"] == "function" {
				target["tool_choice"] = mustJSON(map[string]any{"type": "function", "function": map[string]any{"name": object["name"]}})
			}
		}
	}
}

func normalizeChatGenerationToResponses(source, target map[string]json.RawMessage) {
	if _, exists := target["max_output_tokens"]; !exists {
		copyIfPresent(source, target, "max_tokens", "max_output_tokens")
	}
	if _, exists := target["reasoning"]; !exists {
		if effort := source["reasoning_effort"]; len(effort) > 0 {
			target["reasoning"] = mustJSON(map[string]any{"effort": stringValue(effort)})
		}
	}
}

func normalizeResponsesGenerationToChat(source, target map[string]json.RawMessage) {
	if _, exists := target["max_completion_tokens"]; !exists {
		copyIfPresent(source, target, "max_output_tokens", "max_completion_tokens")
	}
	if reasoning := objectMap(source["reasoning"]); len(reasoning) > 0 {
		target["reasoning_effort"] = rawString(stringValue(reasoning["effort"]))
	}
}

func responseContentToChat(raw json.RawMessage) string {
	if text := stringValue(raw); text != "" {
		return text
	}
	parts := rawList(raw)
	var builder strings.Builder
	for _, part := range parts {
		if stringValue(part["type"]) == "output_text" || stringValue(part["type"]) == "input_text" {
			builder.WriteString(stringValue(part["text"]))
		}
	}
	return builder.String()
}

func contentTextValue(raw json.RawMessage) string {
	if text := stringValue(raw); text != "" {
		return text
	}
	return contentTextRaw(rawList(raw))
}

func lastAssistantMessage(messages []map[string]any) map[string]any {
	if len(messages) == 0 {
		return nil
	}
	message := messages[len(messages)-1]
	if message["role"] == "assistant" {
		return message
	}
	return nil
}

func usageFromResponses(raw json.RawMessage) *ir.Usage {
	object := objectMap(raw)
	if len(object) == 0 {
		return nil
	}
	return &ir.Usage{InputTokens: int(intValue(object["input_tokens"])), OutputTokens: int(intValue(object["output_tokens"])), TotalTokens: int(intValue(object["total_tokens"])), ReasoningTokens: int(intValue(object["reasoning_tokens"])), CachedInputTokens: int(intValue(object["cached_tokens"]))}
}

func usageFromChat(raw json.RawMessage) *ir.Usage {
	object := objectMap(raw)
	if len(object) == 0 {
		return nil
	}
	details := objectMap(object["prompt_tokens_details"])
	completion := objectMap(object["completion_tokens_details"])
	return &ir.Usage{InputTokens: int(intValue(object["prompt_tokens"])), OutputTokens: int(intValue(object["completion_tokens"])), TotalTokens: int(intValue(object["total_tokens"])), ReasoningTokens: int(intValue(completion["reasoning_tokens"])), CachedInputTokens: int(intValue(details["cached_tokens"]))}
}

func responsesUsage(usage *ir.Usage) map[string]any {
	return map[string]any{"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens, "total_tokens": usage.TotalTokens, "output_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens}, "input_tokens_details": map[string]any{"cached_tokens": usage.CachedInputTokens}}
}

func chatUsage(usage *ir.Usage) map[string]any {
	return map[string]any{"prompt_tokens": usage.InputTokens, "completion_tokens": usage.OutputTokens, "total_tokens": usage.TotalTokens, "prompt_tokens_details": map[string]any{"cached_tokens": usage.CachedInputTokens}, "completion_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens}}
}

func rawMap(data string) (map[string]json.RawMessage, error) {
	var result map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &result); err != nil {
		return nil, err
	}
	return result, nil
}

func objectMap(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil {
		return nil
	}
	return result
}

func rawList(raw json.RawMessage) []map[string]json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var values []map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
}

func decodeRawList(raw json.RawMessage) ([]map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("缺少 messages")
	}
	var values []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func decodeResponseInput(raw json.RawMessage) ([]map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if text := stringValue(raw); text != "" {
		return []map[string]json.RawMessage{{"type": rawString("message"), "role": rawString("user"), "content": mustJSON([]any{map[string]any{"type": "input_text", "text": text}})}}, nil
	}
	var values []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("解析 Responses input: %w", err)
	}
	return values, nil
}

func contentText(parts []map[string]any) string {
	var builder strings.Builder
	for _, part := range parts {
		if text, ok := part["text"].(string); ok {
			builder.WriteString(text)
		}
	}
	return builder.String()
}

func contentTextRaw(parts []map[string]json.RawMessage) string {
	var builder strings.Builder
	for _, part := range parts {
		if stringValue(part["type"]) == "input_text" || stringValue(part["type"]) == "output_text" {
			builder.WriteString(stringValue(part["text"]))
		}
	}
	return builder.String()
}

func joinText(parts []string) string {
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func stringValue(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return ""
}

func intValue(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var value int64
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return 0
}

func rawString(value string) json.RawMessage {
	return mustJSON(value)
}

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func mustJSONString(value any) string {
	return string(mustJSON(value))
}

func rawToAny(raw json.RawMessage) any {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}

func rawMapToAny(value map[string]json.RawMessage) map[string]any {
	result := make(map[string]any, len(value))
	for key, raw := range value {
		result[key] = rawToAny(raw)
	}
	return result
}

func copyKnown(source, target map[string]json.RawMessage, keys ...string) {
	for _, key := range keys {
		copyIfPresent(source, target, key, key)
	}
}

func copyIfPresent(source, target map[string]json.RawMessage, from, to string) {
	if raw, exists := source[from]; exists {
		target[to] = bytes.Clone(raw)
	}
}

func deltaOrEmpty(delta map[string]any) map[string]any {
	if delta == nil {
		return map[string]any{}
	}
	return delta
}

func toolIndex(state *streamState, tool *streamTool) int {
	for index, candidate := range state.tools {
		if candidate == tool {
			return index
		}
	}
	return 0
}
