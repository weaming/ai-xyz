package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

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
