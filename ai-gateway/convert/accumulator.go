package convert

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/weaming/ai-xyz/ai-gateway/ir"
)

// Accumulator 把 IR 流事件聚合成一个完整响应。
type Accumulator struct {
	response   ir.Response
	items      map[int]*accItem
	order      []int
	usage      *ir.Usage
	status     string
	finish     string
	incomplete json.RawMessage
	finished   bool
}

type accItem struct {
	item      ir.Item
	text      strings.Builder
	refusal   strings.Builder
	reasoning strings.Builder
	args      strings.Builder
	input     strings.Builder
}

// NewAccumulator 创建空聚合器。
func NewAccumulator() *Accumulator {
	return &Accumulator{items: make(map[int]*accItem)}
}

// Apply 接收一个流事件。
func (accumulator *Accumulator) Apply(event ir.StreamEvent) error {
	switch event.Type {
	case ir.EventCreated:
		accumulator.response.ID = event.ResponseID
		accumulator.response.Model = event.Model
		accumulator.response.CreatedAt = event.CreatedAt
	case ir.EventOutputItemAdded:
		if event.Item != nil {
			entry := accumulator.entry(event.OutputIndex)
			entry.item = cloneItem(*event.Item)
		}
	case ir.EventTextDelta:
		entry := accumulator.entry(event.OutputIndex)
		entry.ensureMessage()
		entry.text.WriteString(event.Delta)
	case ir.EventReasoningDelta:
		entry := accumulator.entry(event.OutputIndex)
		entry.ensureReasoning()
		entry.reasoning.WriteString(event.Delta)
	case ir.EventRefusalDelta:
		entry := accumulator.entry(event.OutputIndex)
		entry.ensureMessage()
		entry.refusal.WriteString(event.Refusal + event.Delta)
	case ir.EventToolCallStart:
		entry := accumulator.toolEntry(event)
		entry.ensureTool(event)
	case ir.EventToolArgumentsDelta:
		entry := accumulator.toolEntry(event)
		entry.ensureTool(event)
		if event.ToolType == "custom" {
			entry.input.WriteString(event.Delta)
		} else {
			entry.args.WriteString(event.Delta)
		}
	case ir.EventToolArgumentsDone:
		entry := accumulator.toolEntry(event)
		entry.ensureTool(event)
		if event.ToolType == "custom" {
			entry.input.Reset()
			entry.input.WriteString(event.Arguments)
		} else {
			entry.args.Reset()
			entry.args.WriteString(event.Arguments)
		}
	case ir.EventOutputItemDone:
		if event.Item != nil {
			entry := accumulator.entry(event.OutputIndex)
			entry.item = cloneItem(*event.Item)
			if !entry.hasDeltas() {
				entry.text.Reset()
				entry.reasoning.Reset()
				entry.refusal.Reset()
			}
		}
	case ir.EventUsage:
		if event.Usage != nil {
			accumulator.usage = event.Usage
		}
	case ir.EventCompleted:
		accumulator.finished = true
		accumulator.status = event.Status
		accumulator.finish = event.FinishReason
		accumulator.incomplete = event.IncompleteDetails
		if event.Usage != nil {
			accumulator.usage = event.Usage
		}
		if accumulator.response.ID == "" {
			accumulator.response.ID = event.ResponseID
		}
	case ir.EventFailed:
		accumulator.finished = true
		accumulator.status = "failed"
		accumulator.finish = "error"
	}
	return nil
}

// Response 返回聚合结果，流缺少终止事件时报错。
func (accumulator *Accumulator) Response() (*ir.Response, error) {
	if !accumulator.finished {
		return nil, fmt.Errorf("流在终止事件前结束")
	}
	indices := make([]int, 0, len(accumulator.items))
	for index := range accumulator.items {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	output := make([]ir.Item, 0, len(indices))
	for _, index := range indices {
		output = append(output, accumulator.items[index].build())
	}
	output, _ = sanitizeToolItems(output)
	response := accumulator.response
	response.Output = output
	response.Usage = accumulator.usage
	response.Status = accumulator.status
	if response.Status == "" {
		response.Status = "completed"
	}
	response.FinishReason = accumulator.finish
	response.IncompleteDetails = accumulator.incomplete
	if response.Object == "" {
		response.Object = "response"
	}
	return &response, nil
}

func (accumulator *Accumulator) entry(index int) *accItem {
	entry, exists := accumulator.items[index]
	if !exists {
		entry = &accItem{}
		accumulator.items[index] = entry
		accumulator.order = append(accumulator.order, index)
	}
	return entry
}

func (accumulator *Accumulator) toolEntry(event ir.StreamEvent) *accItem {
	entry := accumulator.entry(event.OutputIndex)
	if event.CallID != "" || entry.item.CallID == "" {
		entry.item.CallID = event.CallID
	}
	return entry
}

func (entry *accItem) ensureMessage() {
	if entry.item.Type == "" {
		entry.item.Type = "message"
		entry.item.Role = "assistant"
	}
}

func (entry *accItem) ensureReasoning() {
	if entry.item.Type == "" {
		entry.item.Type = "reasoning"
		entry.item.Role = "assistant"
	}
}

func (entry *accItem) ensureTool(event ir.StreamEvent) {
	if entry.item.Type == "" || entry.item.Type == "message" {
		entry.item.Type = "function_call"
		if event.ToolType == "custom" {
			entry.item.Type = "custom_tool_call"
		}
		entry.item.Role = ""
	}
	if event.ToolName != "" {
		entry.item.Name = event.ToolName
	}
}

func (entry *accItem) hasDeltas() bool {
	return entry.text.Len() > 0 || entry.refusal.Len() > 0 || entry.reasoning.Len() > 0 ||
		entry.args.Len() > 0 || entry.input.Len() > 0
}

func (entry *accItem) build() ir.Item {
	item := entry.item
	switch item.Type {
	case "message":
		content := make([]ir.ContentPart, 0, 2)
		if entry.text.Len() > 0 {
			content = append(content, ir.ContentPart{Type: "output_text", Text: entry.text.String()})
		}
		if entry.refusal.Len() > 0 {
			content = append(content, ir.ContentPart{Type: "refusal", Refusal: entry.refusal.String()})
		}
		if len(content) > 0 {
			item.Content = content
		}
		if item.Role == "" {
			item.Role = "assistant"
		}
	case "reasoning":
		if entry.reasoning.Len() > 0 && len(item.Content) == 0 {
			item.Content = []ir.ContentPart{{Type: "summary_text", Text: entry.reasoning.String()}}
		}
		if item.Role == "" {
			item.Role = "assistant"
		}
	case "function_call":
		if entry.args.Len() > 0 {
			item.Arguments = entry.args.String()
		}
	case "custom_tool_call":
		if entry.input.Len() > 0 {
			item.Input = entry.input.String()
		}
	}
	return item
}

func cloneItem(item ir.Item) ir.Item {
	clone := item
	if len(item.Content) > 0 {
		clone.Content = make([]ir.ContentPart, len(item.Content))
		copy(clone.Content, item.Content)
	}
	if len(item.Output) > 0 {
		clone.Output = append(json.RawMessage(nil), item.Output...)
	}
	return clone
}
