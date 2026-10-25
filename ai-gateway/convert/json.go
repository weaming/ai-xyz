package convert

import "encoding/json"

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
		return nil, errMissingMessages
	}
	var values []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	return values, nil
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

func stringValueAny(value any) string {
	text, _ := value.(string)
	return text
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

func boolValue(raw json.RawMessage) bool {
	var value bool
	return json.Unmarshal(raw, &value) == nil && value
}

// hasJSONValue 判断字段是否携带了需要注意的值（null/false/0/空串/空容器视为缺省）。
func hasJSONValue(raw json.RawMessage) bool {
	value := string(raw)
	return len(raw) > 0 && value != "null" && value != "false" && value != "0" && value != `""` && value != "{}" && value != "[]"
}

func rawListStrings(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
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

func deltaOrEmpty(delta map[string]any) map[string]any {
	if delta == nil {
		return map[string]any{}
	}
	return delta
}
