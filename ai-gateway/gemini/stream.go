package gemini

import (
	"encoding/json"
	"strings"

	"google.golang.org/genai"
)

// chatChunk OpenAI 流式 chunk。
type chatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usageInfo    `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type chatDelta struct {
	Content          *string         `json:"content,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	ToolCalls        []deltaToolCall `json:"tool_calls,omitempty"`
}

type deltaToolCall struct {
	Index            int            `json:"index"`
	ID               string         `json:"id"`
	Type             string         `json:"type"`
	Function         openaiCallArgs `json:"function"`
	ThoughtSignature string         `json:"thought_signature,omitempty"`
}

type toolCallStreamState struct {
	index        int
	hasToolCalls bool
}

// toOpenAIChunk Gemini 流式 chunk → OpenAI SSE chunk。
func toOpenAIChunk(resp *genai.GenerateContentResponse, model, requestID string, createdAt int64, state *toolCallStreamState) *chatChunk {
	chunk := &chatChunk{
		ID:      requestID,
		Object:  "chat.completion.chunk",
		Created: createdAt,
		Model:   modelVersion(resp, model),
	}

	candidate := firstCandidate(resp)
	if candidate == nil {
		chunk.Choices = []chunkChoice{{Index: 0, Delta: chatDelta{}}}
		chunk.Usage = toUsageInfo(resp.UsageMetadata)
		return chunk
	}

	delta := chatDelta{}
	var toolCalls []deltaToolCall
	var reasoningText []string
	var contentText []string
	if candidate.Content == nil {
		chunk.Choices = []chunkChoice{{Index: 0, Delta: delta}}
		chunk.Usage = toUsageInfo(resp.UsageMetadata)
		return chunk
	}
	for _, part := range candidate.Content.Parts {
		if part == nil {
			continue
		}
		switch {
		case part.Thought && part.Text != "":
			reasoningText = append(reasoningText, part.Text)
		case part.Text != "":
			contentText = append(contentText, part.Text)
		case part.FunctionCall != nil:
			call := part.FunctionCall
			callID := call.ID
			if callID == "" {
				callID = newID("call_")
			}
			argsJSON, err := json.Marshal(call.Args)
			if err != nil {
				argsJSON = []byte("{}")
			}
			index := state.index
			state.index++
			state.hasToolCalls = true
			toolCalls = append(toolCalls, deltaToolCall{
				Index:            index,
				ID:               callID,
				Type:             "function",
				Function:         openaiCallArgs{Name: call.Name, Arguments: string(argsJSON)},
				ThoughtSignature: encodeThoughtSignature(part.ThoughtSignature),
			})
		}
	}
	if len(reasoningText) > 0 {
		delta.ReasoningContent = strPtr(strings.Join(reasoningText, ""))
	}
	if len(contentText) > 0 {
		delta.Content = strPtr(strings.Join(contentText, ""))
	}
	if len(toolCalls) > 0 {
		delta.ToolCalls = toolCalls
	}

	var finishReason *string
	if candidate.FinishReason != genai.FinishReasonUnspecified {
		reason := mapFinishReason(candidate.FinishReason, state.hasToolCalls)
		finishReason = &reason
	}
	chunk.Choices = []chunkChoice{{Index: 0, Delta: delta, FinishReason: finishReason}}
	chunk.Usage = toUsageInfo(resp.UsageMetadata)
	return chunk
}

func strPtr(s string) *string {
	return &s
}
