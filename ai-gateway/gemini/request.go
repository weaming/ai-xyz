package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
)

var chatRequestFields = map[string]struct{}{
	"model": {}, "messages": {}, "tools": {}, "tool_choice": {},
	"response_format": {}, "stream_options": {}, "reasoning_effort": {},
	"max_tokens": {}, "max_completion_tokens": {}, "stream": {},
	"temperature": {}, "top_p": {}, "stop": {},
}

var responsesRequestFields = map[string]struct{}{
	"model": {}, "instructions": {}, "input": {}, "tools": {},
	"tool_choice": {}, "reasoning": {}, "reasoning_effort": {},
	"max_output_tokens": {}, "stream": {}, "temperature": {}, "top_p": {},
}

type stopSequences []string

func (sequences *stopSequences) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		*sequences = nil
		return nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		*sequences = stopSequences{single}
		return nil
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err != nil {
		return fmt.Errorf("stop 必须是字符串或字符串数组: %w", err)
	}
	*sequences = multiple
	return nil
}

// decodeKnownRequest 拒绝当前后端无法保留的请求字段。
func decodeKnownRequest(body []byte, target any, allowed map[string]struct{}) error {
	if len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' {
		return fmt.Errorf("请求必须是 JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return fmt.Errorf("解析请求 JSON: %w", err)
	}
	for name := range fields {
		if _, exists := allowed[name]; !exists {
			return fmt.Errorf("不支持请求字段 %q", name)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("解析请求字段: %w", err)
	}
	return nil
}
