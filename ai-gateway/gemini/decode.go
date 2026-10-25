package gemini

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/weaming/ai-xyz/ai-gateway/convert"
	"github.com/weaming/ai-xyz/ai-gateway/ir"
	"google.golang.org/genai"
)

// decodeResponse 把 Gemini 响应转换成 IR 响应。
func decodeResponse(response *genai.GenerateContentResponse, model string) *ir.Response {
	decoded := &ir.Response{
		ID:        responseID(response),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     modelVersion(response, model),
		Usage:     usageFromNative(response.UsageMetadata),
	}
	candidate := firstCandidate(response)
	if candidate == nil {
		decoded.Status, decoded.FinishReason, decoded.IncompleteDetails = statusFromFinish(genai.FinishReasonUnspecified, false)
		if isPromptBlocked(response) {
			decoded.Status = "incomplete"
			decoded.FinishReason = "content_filter"
			decoded.IncompleteDetails = mustRaw(map[string]any{"reason": "content_filter"})
		}
		return decoded
	}
	decoded.Output = partsToItems(candidateContentParts(candidate))
	hasToolCalls := false
	for _, item := range decoded.Output {
		if item.Type == "function_call" {
			hasToolCalls = true
		}
	}
	decoded.Status, decoded.FinishReason, decoded.IncompleteDetails = statusFromFinish(candidate.FinishReason, hasToolCalls)
	if isPromptBlocked(response) {
		decoded.Status = "incomplete"
		decoded.FinishReason = "content_filter"
		decoded.IncompleteDetails = mustRaw(map[string]any{"reason": "content_filter"})
	}
	return decoded
}

func partsToItems(parts []*genai.Part) []ir.Item {
	items := make([]ir.Item, 0, len(parts))
	for _, part := range parts {
		if part == nil {
			continue
		}
		switch {
		case part.Text != "" && part.Thought:
			items = appendTextItem(items, "reasoning", "summary_text", part.Text)
		case part.Text != "":
			items = appendTextItem(items, "message", "output_text", part.Text)
		case part.FunctionCall != nil:
			items = append(items, functionCallItem(part))
		}
	}
	return items
}

func appendTextItem(items []ir.Item, itemType, partType, text string) []ir.Item {
	if len(items) > 0 {
		last := &items[len(items)-1]
		if last.Type == itemType {
			if len(last.Content) == 0 {
				last.Content = []ir.ContentPart{{Type: partType}}
			}
			last.Content[len(last.Content)-1].Text += text
			return items
		}
	}
	item := ir.Item{Type: itemType, Role: "assistant", Status: "completed", Content: []ir.ContentPart{{Type: partType, Text: text}}}
	return append(items, item)
}

func functionCallItem(part *genai.Part) ir.Item {
	call := part.FunctionCall
	callID := call.ID
	if callID == "" {
		callID = newID("call_")
	}
	arguments, err := json.Marshal(call.Args)
	if err != nil {
		arguments = []byte("{}")
	}
	item := ir.Item{Type: "function_call", CallID: callID, Name: call.Name, Arguments: string(arguments), Status: "completed"}
	if signature := encodeBase64(part.ThoughtSignature); signature != "" {
		item.Extensions = map[string]json.RawMessage{"thought_signature": mustRaw(signature)}
	}
	return item
}

// statusFromFinish 把 Gemini finishReason 映射成 IR 状态与 finish reason。
func statusFromFinish(reason genai.FinishReason, hasToolCalls bool) (string, string, json.RawMessage) {
	if hasToolCalls {
		return "completed", "tool_calls", nil
	}
	switch reason {
	case genai.FinishReasonMaxTokens:
		return "incomplete", "length", mustRaw(map[string]any{"reason": "max_output_tokens"})
	case genai.FinishReasonSafety, genai.FinishReasonRecitation, genai.FinishReasonBlocklist,
		genai.FinishReasonProhibitedContent, genai.FinishReasonSPII, genai.FinishReasonImageSafety,
		genai.FinishReasonLanguage, genai.FinishReasonImageProhibitedContent, genai.FinishReasonImageRecitation,
		genai.FinishReasonImageOther:
		return "incomplete", "content_filter", mustRaw(map[string]any{"reason": "content_filter"})
	default:
		return "completed", "stop", nil
	}
}

func usageFromNative(usage *genai.GenerateContentResponseUsageMetadata) *ir.Usage {
	if usage == nil {
		return nil
	}
	return &ir.Usage{
		InputTokens:       int(usage.PromptTokenCount),
		OutputTokens:      int(usage.CandidatesTokenCount),
		TotalTokens:       int(usage.TotalTokenCount),
		ReasoningTokens:   int(usage.ThoughtsTokenCount),
		CachedInputTokens: int(usage.CachedContentTokenCount),
	}
}

func firstCandidate(response *genai.GenerateContentResponse) *genai.Candidate {
	if response == nil || len(response.Candidates) == 0 {
		return nil
	}
	return response.Candidates[0]
}

func candidateContentParts(candidate *genai.Candidate) []*genai.Part {
	if candidate == nil || candidate.Content == nil {
		return nil
	}
	return candidate.Content.Parts
}

func isPromptBlocked(response *genai.GenerateContentResponse) bool {
	return response != nil && response.PromptFeedback != nil &&
		response.PromptFeedback.BlockReason != "" &&
		response.PromptFeedback.BlockReason != genai.BlockedReasonUnspecified
}

func modelVersion(response *genai.GenerateContentResponse, fallback string) string {
	if response != nil && response.ModelVersion != "" {
		return response.ModelVersion
	}
	return fallback
}

func responseID(response *genai.GenerateContentResponse) string {
	if response != nil && response.ResponseID != "" {
		return response.ResponseID
	}
	return newID("resp_")
}

// chunkState 保存一次 Gemini 流式响应的跨 chunk 状态。
type chunkState struct {
	started    bool
	nextTool   int
	finish     genai.FinishReason
	blocked    bool
	usage      *ir.Usage
	responseID string
	model      string
	created    int64
}

func newChunkState() *chunkState {
	return &chunkState{nextTool: convert.OutputIndexFirstTool}
}

// events 把一个 Gemini chunk 转换成 IR 事件。
func (state *chunkState) events(response *genai.GenerateContentResponse, model string) []ir.StreamEvent {
	events := []ir.StreamEvent{}
	if !state.started {
		state.started = true
		state.responseID = responseID(response)
		state.model = modelVersion(response, model)
		state.created = time.Now().Unix()
		events = append(events, ir.StreamEvent{Type: ir.EventCreated, ResponseID: state.responseID, Model: state.model, CreatedAt: state.created})
	}
	if isPromptBlocked(response) {
		state.blocked = true
	}
	if usage := usageFromNative(response.UsageMetadata); usage != nil {
		state.usage = usage
	}
	candidate := firstCandidate(response)
	if candidate != nil && candidate.FinishReason != genai.FinishReasonUnspecified {
		state.finish = candidate.FinishReason
	}
	for _, part := range candidateContentParts(candidate) {
		if part == nil {
			continue
		}
		switch {
		case part.Text != "" && part.Thought:
			events = append(events, ir.StreamEvent{Type: ir.EventReasoningDelta, OutputIndex: convert.OutputIndexReasoning, Delta: part.Text})
		case part.Text != "":
			events = append(events, ir.StreamEvent{Type: ir.EventTextDelta, OutputIndex: convert.OutputIndexMessage, Delta: part.Text})
		case part.FunctionCall != nil:
			item := functionCallItem(part)
			outputIndex := state.nextTool
			state.nextTool++
			events = append(events,
				ir.StreamEvent{Type: ir.EventToolCallStart, OutputIndex: outputIndex, CallID: item.CallID, ToolName: item.Name, ToolType: "function"},
				ir.StreamEvent{Type: ir.EventToolArgumentsDone, OutputIndex: outputIndex, CallID: item.CallID, ToolName: item.Name, ToolType: "function", Arguments: item.Arguments},
				ir.StreamEvent{Type: ir.EventOutputItemDone, OutputIndex: outputIndex, Item: &item},
			)
		}
	}
	return events
}

// finishEvents 在流结束时补齐 usage 与终止事件。
func (state *chunkState) finishEvents() []ir.StreamEvent {
	events := []ir.StreamEvent{}
	if state.usage != nil {
		events = append(events, ir.StreamEvent{Type: ir.EventUsage, Usage: state.usage})
	}
	if state.blocked {
		events = append(events, ir.StreamEvent{
			Type:              ir.EventCompleted,
			ResponseID:        state.responseID,
			Status:            "incomplete",
			FinishReason:      "content_filter",
			IncompleteDetails: mustRaw(map[string]any{"reason": "content_filter"}),
			Usage:             state.usage,
		})
		return events
	}
	status, finish, incomplete := statusFromFinish(state.finish, false)
	events = append(events, ir.StreamEvent{
		Type:              ir.EventCompleted,
		ResponseID:        state.responseID,
		Status:            status,
		FinishReason:      finish,
		IncompleteDetails: incomplete,
		Usage:             state.usage,
	})
	return events
}

func mustRaw(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("null")
	}
	return data
}

// dedupeStrings 去重保持顺序，供 allowed tools 名单使用。
func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// textParts 拼接 part 文本，供 codec 解析使用。
func textParts(parts []*genai.Part) string {
	var builder strings.Builder
	for _, part := range parts {
		if part != nil {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}
