package ir

import "encoding/json"

// EventType 是跨协议共有的流语义事件类型。
type EventType string

const (
	EventCreated            EventType = "created"
	EventOutputItemAdded    EventType = "output_item_added"
	EventContentPartAdded   EventType = "content_part_added"
	EventTextDelta          EventType = "text_delta"
	EventReasoningDelta     EventType = "reasoning_delta"
	EventRefusalDelta       EventType = "refusal_delta"
	EventToolCallStart      EventType = "tool_call_start"
	EventToolArgumentsDelta EventType = "tool_arguments_delta"
	EventToolArgumentsDone  EventType = "tool_arguments_done"
	EventContentPartDone    EventType = "content_part_done"
	EventOutputItemDone     EventType = "output_item_done"
	EventUsage              EventType = "usage"
	EventCompleted          EventType = "completed"
	EventFailed             EventType = "failed"
	EventOpaque             EventType = "opaque"
)

// StreamEvent 是流适配器之间传递的事件。
type StreamEvent struct {
	Type           EventType
	Response       *Response
	Item           *Item
	Delta          string
	Refusal        string
	ResponseID     string
	Model          string
	OutputIndex    int
	ContentIndex   int
	CallID         string
	ToolName       string
	ToolType       string
	Arguments      string
	FinishReason   string
	Usage          *Usage
	SequenceNumber *int64
	Raw            json.RawMessage
}

// StreamState 保存一次转换所需的跨 chunk 状态。
type StreamState struct {
	ResponseID       string
	Model            string
	CreatedAt        int64
	NextOutputIndex  int
	NextContentIndex int
	MessageItemID    string
	MessageStarted   bool
	ToolCalls        map[string]*ToolCallState
	Usage            *Usage
	Finished         bool
}

// ToolCallState 保存一个流式工具调用的身份和参数缓冲区。
type ToolCallState struct {
	ItemID      string
	CallID      string
	Name        string
	Arguments   string
	OutputIndex int
	Started     bool
	Finished    bool
}

// NewStreamState 创建空的流式状态。
func NewStreamState() *StreamState {
	return &StreamState{ToolCalls: make(map[string]*ToolCallState)}
}
