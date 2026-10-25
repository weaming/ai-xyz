package convert

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/weaming/ai-xyz/ai-gateway/ir"
)

var (
	idCounter          atomic.Uint64
	errMissingMessages = fmt.Errorf("缺少 messages")
)

// newID 生成带前缀的唯一 id。
func newID(prefix string) string {
	return fmt.Sprintf("%s%d-%d", prefix, time.Now().UnixNano(), idCounter.Add(1))
}

func floatPtr(raw json.RawMessage) *float64 {
	if !hasJSONValue(raw) {
		return nil
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}

func intPtr(raw json.RawMessage) *int {
	if !hasJSONValue(raw) {
		return nil
	}
	var value int
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}

func boolPtr(raw json.RawMessage) *bool {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}

func boolPointer(value bool) *bool {
	return &value
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if !hasJSONValue(raw) {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func objectAnys(raw json.RawMessage) (map[string]any, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func unknownKeys(source map[string]json.RawMessage, known map[string]struct{}) map[string]json.RawMessage {
	unknown := map[string]json.RawMessage{}
	for key, value := range source {
		if _, exists := known[key]; exists {
			continue
		}
		unknown[key] = value
	}
	if len(unknown) == 0 {
		return nil
	}
	return unknown
}

func rejectUnknown(unknown map[string]json.RawMessage, direction string) error {
	if len(unknown) == 0 {
		return nil
	}
	keys := make([]string, 0, len(unknown))
	for key := range unknown {
		keys = append(keys, key)
	}
	return fmt.Errorf("%s 不支持字段 %q，避免静默丢失语义", direction, strings.Join(keys, ","))
}

func stringValueAnyString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64, bool:
		return fmt.Sprint(typed)
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(data)
	}
}

func ensureReasoning(config *ir.ReasoningConfig) *ir.ReasoningConfig {
	if config == nil {
		return &ir.ReasoningConfig{}
	}
	return config
}

func isTextOnly(parts []ir.ContentPart) bool {
	for _, part := range parts {
		if part.Type != "text" && part.Type != "output_text" {
			return false
		}
	}
	return true
}

func joinParts(parts []ir.ContentPart) string {
	var builder strings.Builder
	for _, part := range parts {
		if part.Type == "text" || part.Type == "output_text" || part.Type == "summary_text" || part.Type == "reasoning_text" {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

func setAny(target map[string]any, key string, value any) {
	switch typed := value.(type) {
	case *float64:
		if typed != nil {
			target[key] = *typed
		}
	case *int:
		if typed != nil {
			target[key] = *typed
		}
	case *bool:
		if typed != nil {
			target[key] = *typed
		}
	}
}

func incompleteReason(raw json.RawMessage) string {
	return stringValue(objectMap(raw)["reason"])
}

func usageFromChat(raw json.RawMessage) *ir.Usage {
	object := objectMap(raw)
	if len(object) == 0 {
		return nil
	}
	details := objectMap(object["prompt_tokens_details"])
	completion := objectMap(object["completion_tokens_details"])
	return &ir.Usage{
		InputTokens:       int(intValue(object["prompt_tokens"])),
		OutputTokens:      int(intValue(object["completion_tokens"])),
		TotalTokens:       int(intValue(object["total_tokens"])),
		ReasoningTokens:   int(intValue(completion["reasoning_tokens"])),
		CachedInputTokens: int(intValue(details["cached_tokens"])),
	}
}

func usageFromResponses(raw json.RawMessage) *ir.Usage {
	object := objectMap(raw)
	if len(object) == 0 {
		return nil
	}
	inputDetails := objectMap(object["input_tokens_details"])
	outputDetails := objectMap(object["output_tokens_details"])
	return &ir.Usage{
		InputTokens:       int(intValue(object["input_tokens"])),
		OutputTokens:      int(intValue(object["output_tokens"])),
		TotalTokens:       int(intValue(object["total_tokens"])),
		ReasoningTokens:   int(intValue(outputDetails["reasoning_tokens"])),
		CachedInputTokens: int(intValue(inputDetails["cached_tokens"])),
	}
}

func chatUsage(usage *ir.Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":             usage.InputTokens,
		"completion_tokens":         usage.OutputTokens,
		"total_tokens":              usage.TotalTokens,
		"prompt_tokens_details":     map[string]any{"cached_tokens": usage.CachedInputTokens},
		"completion_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens},
	}
}

func responsesUsage(usage *ir.Usage) map[string]any {
	return map[string]any{
		"input_tokens":         usage.InputTokens,
		"output_tokens":        usage.OutputTokens,
		"total_tokens":         usage.TotalTokens,
		"input_tokens_details": map[string]any{"cached_tokens": usage.CachedInputTokens},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": usage.ReasoningTokens,
		},
	}
}

func responsesStatusFromChatFinish(finish string) (string, json.RawMessage) {
	switch finish {
	case "length":
		return "incomplete", mustJSON(map[string]any{"reason": "max_output_tokens"})
	case "content_filter":
		return "incomplete", mustJSON(map[string]any{"reason": "content_filter"})
	case "error":
		return "failed", nil
	default:
		return "completed", nil
	}
}

// unsupportedItem 报告 IR item 无法被目标协议表达。
func unsupportedItem(protocol, itemType string) error {
	if itemType == "" {
		itemType = "<empty>"
	}
	return fmt.Errorf("%s 无法表达 item type %q", protocol, itemType)
}

func responsesFinishReason(status string, incomplete json.RawMessage) string {
	switch status {
	case "failed", "cancelled", "queued", "in_progress":
		return "error"
	case "incomplete":
		switch incompleteReason(incomplete) {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		default:
			return "error"
		}
	default:
		return "stop"
	}
}
