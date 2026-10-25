package ir

import "encoding/json"

// EventType 是跨协议共有的流语义事件类型。
type EventType string

const (
	EventCreated            EventType = "created"
	EventOutputItemAdded    EventType = "output_item_added"
	EventTextDelta          EventType = "text_delta"
	EventReasoningDelta     EventType = "reasoning_delta"
	EventRefusalDelta       EventType = "refusal_delta"
	EventToolCallStart      EventType = "tool_call_start"
	EventToolArgumentsDelta EventType = "tool_arguments_delta"
	EventToolArgumentsDone  EventType = "tool_arguments_done"
	EventOutputItemDone     EventType = "output_item_done"
	EventUsage              EventType = "usage"
	EventCompleted          EventType = "completed"
	EventFailed             EventType = "failed"
	EventOpaque             EventType = "opaque"
)

// StreamEvent 是流适配器之间传递的事件。
// 增量事件用 OutputIndex/CallID 关联到具体 item，工具增量用 ToolType 区分 function 与 custom。
type StreamEvent struct {
	Type              EventType
	ResponseID        string
	Model             string
	CreatedAt         int64
	Item              *Item
	Delta             string
	Refusal           string
	OutputIndex       int
	ContentIndex      int
	CallID            string
	ToolName          string
	ToolType          string
	Arguments         string
	Status            string
	FinishReason      string
	IncompleteDetails json.RawMessage
	Usage             *Usage
	SequenceNumber    *int64
	Raw               json.RawMessage
}
