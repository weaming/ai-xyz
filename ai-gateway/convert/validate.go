package convert

import (
	"encoding/json"
	"fmt"
)

func validateChatRequest(source map[string]json.RawMessage) error {
	if raw := source["n"]; len(raw) > 0 && string(raw) != "null" {
		if intValue(raw) > 1 {
			return fmt.Errorf("Chat -> Responses 不支持 n 大于 1")
		}
	}
	if boolValue(source["logprobs"]) {
		return fmt.Errorf("Chat -> Responses 不支持 logprobs")
	}
	if hasJSONValue(source["top_logprobs"]) {
		return fmt.Errorf("Chat -> Responses 不支持 top_logprobs")
	}
	if hasJSONValue(source["audio"]) || hasAudioModality(source["modalities"]) {
		return fmt.Errorf("Chat -> Responses 暂不支持音频输出")
	}
	if err := validateChatStreamOptions(source["stream_options"]); err != nil {
		return err
	}
	// functions/function_call 是旧工具协议，语义无法映射；其余 Chat 独有调参字段见 chatDroppableKeys。
	return rejectFields(source, "Chat -> Responses", "functions", "function_call")
}

// validateChatStreamOptions 校验 Chat 的流式 usage 选项是否能由 Responses 表达。
func validateChatStreamOptions(raw json.RawMessage) error {
	if !hasJSONValue(raw) {
		return nil
	}
	var options map[string]json.RawMessage
	if err := json.Unmarshal(raw, &options); err != nil || options == nil {
		return fmt.Errorf("Chat -> Responses 的 stream_options 必须是 JSON 对象")
	}
	for field := range options {
		if field != "include_usage" {
			return fmt.Errorf("Chat -> Responses 的 stream_options 不支持字段 %q，避免静默丢失语义", field)
		}
	}
	includeUsage, exists := options["include_usage"]
	if !exists {
		return nil
	}
	var enabled bool
	if err := json.Unmarshal(includeUsage, &enabled); err != nil {
		return fmt.Errorf("Chat -> Responses 的 stream_options.include_usage 必须是布尔值")
	}
	return nil
}

// validateResponsesRequest 只拒绝会改变语义的字段：会话状态、后台运行、服务端 prompt/审核与上下文管理。
// 调参和元数据字段（include、prompt_cache_*、safety_identifier）见 responsesDroppableKeys。
func validateResponsesRequest(source map[string]json.RawMessage) error {
	return rejectFields(source, "Responses -> Chat", "previous_response_id", "conversation", "background", "prompt", "moderation", "context_management", "max_tool_calls")
}

// chatDroppableKeys 是 Chat 独有的调参/元数据字段：目标协议不表达时丢弃，不改变语义。
var chatDroppableKeys = []string{"user", "seed", "logit_bias", "prediction", "prompt_cache_key", "prompt_cache_retention", "safety_identifier"}

// responsesDroppableKeys 是 Responses 独有的调参/元数据字段，处理方式同上。
var responsesDroppableKeys = []string{"include", "prompt_cache_key", "prompt_cache_retention", "prompt_cache_options", "safety_identifier"}

// collectDropped 记录源请求里实际出现、将被目标协议忽略的字段名。
func collectDropped(source map[string]json.RawMessage, keys []string) []string {
	var dropped []string
	for _, key := range keys {
		if hasJSONValue(source[key]) {
			dropped = append(dropped, key)
		}
	}
	return dropped
}

func rejectFields(source map[string]json.RawMessage, direction string, fields ...string) error {
	for _, field := range fields {
		if hasJSONValue(source[field]) {
			return fmt.Errorf("%s 不支持字段 %q，避免静默丢失语义", direction, field)
		}
	}
	return nil
}

func hasAudioModality(raw json.RawMessage) bool {
	for _, value := range rawListStrings(raw) {
		if value == "audio" {
			return true
		}
	}
	return false
}
