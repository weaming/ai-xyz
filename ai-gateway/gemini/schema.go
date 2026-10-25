package gemini

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/genai"
)

const maxRefInlineDepth = 10

// SanitizeSchema 对 JSON Schema 执行 Gemini 兼容性清洗：
// 1. 内联 $ref 并移除 $defs / $schema / $id 等 meta 声明；
// 2. 将 const 转为单值 enum；
// 3. 将 nullable type 数组 (如 ["string", "null"]) 转为 type + nullable: true；
// 4. 过滤 properties 中不存在的 required 字段；
// 5. 确保 array items 有明确 type；
// 6. 清理 Gemini 不支持的 OpenAPI 关键字。
func SanitizeSchema(raw any) any {
	obj, ok := raw.(map[string]any)
	if !ok {
		return raw
	}
	defs := extractDefs(obj)
	inlined := inlineRefsAndStripMeta(obj, defs, 0)
	return sanitizeCompat(inlined)
}

func extractDefs(schema map[string]any) map[string]any {
	defs := make(map[string]any)
	if d, ok := schema["$defs"].(map[string]any); ok {
		for k, v := range d {
			defs[k] = v
		}
	}
	if d, ok := schema["definitions"].(map[string]any); ok {
		for k, v := range d {
			if _, exists := defs[k]; !exists {
				defs[k] = v
			}
		}
	}
	return defs
}

func inlineRefsAndStripMeta(obj any, defs map[string]any, depth int) any {
	switch v := obj.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			if depth >= maxRefInlineDepth {
				return map[string]any{"type": "object"}
			}
			if target := resolveDefRef(ref, defs); target != nil {
				return inlineRefsAndStripMeta(target, defs, depth+1)
			}
			return map[string]any{"type": "object"}
		}
		result := make(map[string]any, len(v))
		for k, val := range v {
			switch k {
			case "$defs", "definitions", "if", "then", "else",
				"$schema", "$id", "$anchor", "$dynamicAnchor", "$vocabulary", "$comment":
				continue
			}
			result[k] = inlineRefsAndStripMeta(val, defs, depth)
		}
		return result
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = inlineRefsAndStripMeta(item, defs, depth)
		}
		return out
	default:
		return obj
	}
}

func resolveDefRef(ref string, defs map[string]any) any {
	const prefixDefs = "#/$defs/"
	const prefixDefinitions = "#/definitions/"
	if strings.HasPrefix(ref, prefixDefs) {
		return defs[ref[len(prefixDefs):]]
	}
	if strings.HasPrefix(ref, prefixDefinitions) {
		return defs[ref[len(prefixDefinitions):]]
	}
	return nil
}

func sanitizeCompat(obj any) any {
	if obj == nil {
		return nil
	}
	switch v := obj.(type) {
	case []any:
		res := make([]any, len(v))
		for i, item := range v {
			res[i] = sanitizeCompat(item)
		}
		return res
	case map[string]any:
		res := make(map[string]any, len(v))

		// 规范化 nullable type 数组: ["string", "null"] -> "string" + nullable: true
		if rawType, ok := v["type"]; ok {
			switch tt := rawType.(type) {
			case []string:
				var nonNull string
				for _, s := range tt {
					if s != "null" {
						nonNull = s
					}
				}
				if nonNull != "" {
					res["type"] = nonNull
					res["nullable"] = true
				}
			case []any:
				var nonNull string
				for _, it := range tt {
					if s, ok := it.(string); ok && s != "null" {
						nonNull = s
					}
				}
				if nonNull != "" {
					res["type"] = nonNull
					res["nullable"] = true
				}
			}
		}

		for k, val := range v {
			if k == "type" {
				if _, set := res["type"]; set {
					continue
				}
			}
			if k == "enum" {
				if enumArr, ok := val.([]any); ok {
					strEnum := make([]any, len(enumArr))
					for i, ev := range enumArr {
						strEnum[i] = fmt.Sprint(ev)
					}
					res[k] = strEnum
					continue
				}
			}
			res[k] = sanitizeCompat(val)
		}

		// const -> enum
		if constVal, ok := res["const"]; ok {
			delete(res, "const")
			if _, hasEnum := res["enum"]; !hasEnum {
				res["enum"] = []any{fmt.Sprint(constVal)}
				if _, hasType := res["type"]; !hasType {
					res["type"] = "string"
				}
			}
		}

		// 如果 enum 存在且 type 是 integer/number，统一转为 string
		if _, hasEnum := res["enum"]; hasEnum {
			if t, ok := res["type"].(string); ok && (t == "integer" || t == "number") {
				res["type"] = "string"
			}
		}

		// 过滤 properties 中不存在的 required 字段
		if t, ok := res["type"].(string); ok && t == "object" {
			if props, ok := res["properties"].(map[string]any); ok {
				switch req := res["required"].(type) {
				case []any:
					filtered := make([]any, 0, len(req))
					for _, r := range req {
						if s, ok := r.(string); ok {
							if _, exists := props[s]; exists {
								filtered = append(filtered, r)
							}
						}
					}
					res["required"] = filtered
				case []string:
					filtered := make([]any, 0, len(req))
					for _, s := range req {
						if _, exists := props[s]; exists {
							filtered = append(filtered, s)
						}
					}
					res["required"] = filtered
				}
			}
		} else if t, ok := res["type"].(string); ok && t != "object" {
			delete(res, "properties")
			delete(res, "required")
		}

		// 确保 array items 有 type
		if t, ok := res["type"].(string); ok && t == "array" {
			if res["items"] == nil {
				res["items"] = map[string]any{"type": "string"}
			} else if items, ok := res["items"].(map[string]any); ok {
				if _, hasType := items["type"]; !hasType {
					items["type"] = "string"
					res["items"] = items
				}
			}
		}

		// 移除 Gemini 不支持的关键字
		delete(res, "additionalProperties")
		delete(res, "propertyNames")
		delete(res, "patternProperties")
		delete(res, "exclusiveMinimum")
		delete(res, "exclusiveMaximum")

		return res
	default:
		return obj
	}
}

// toGenaiSchema 把 OpenAI 的 JSON Schema 转换为 genai.Schema。
func toGenaiSchema(raw any) (*genai.Schema, bool) {
	sanitized := SanitizeSchema(raw)
	obj, ok := sanitized.(map[string]any)
	if !ok {
		return nil, false
	}

	schema := &genai.Schema{}
	if typ, ok := obj["type"].(string); ok {
		schema.Type = mapSchemaType(typ)
	}
	if desc, ok := obj["description"].(string); ok {
		schema.Description = desc
	}
	if nullable, ok := obj["nullable"].(bool); ok {
		schema.Nullable = &nullable
	}
	if items, ok := obj["items"]; ok {
		if sub, ok := toGenaiSchema(items); ok {
			schema.Items = sub
		}
	}
	if enum, ok := obj["enum"].([]any); ok {
		for _, e := range enum {
			if s, ok := e.(string); ok {
				schema.Enum = append(schema.Enum, s)
			}
		}
	}
	if required, ok := obj["required"].([]any); ok {
		for _, r := range required {
			if s, ok := r.(string); ok {
				schema.Required = append(schema.Required, s)
			}
		}
	}
	if anyOf, ok := obj["anyOf"].([]any); ok {
		for _, a := range anyOf {
			if sub, ok := toGenaiSchema(a); ok {
				schema.AnyOf = append(schema.AnyOf, sub)
			}
		}
	}
	if properties, ok := obj["properties"].(map[string]any); ok {
		schema.Properties = make(map[string]*genai.Schema, len(properties))
		names := make([]string, 0, len(properties))
		for name, prop := range properties {
			if sub, ok := toGenaiSchema(prop); ok {
				schema.Properties[name] = sub
				names = append(names, name)
			}
		}
		// 稳定排序,保证请求前缀稳定利于上下文缓存
		sort.Strings(names)
		schema.PropertyOrdering = names
	}
	if title, ok := obj["title"].(string); ok {
		schema.Title = title
	}
	if v, ok := obj["example"]; ok {
		schema.Example = v
	}
	if v, ok := obj["minItems"]; ok {
		if i, ok := toInt64(v); ok {
			schema.MinItems = &i
		}
	}
	if v, ok := obj["maxItems"]; ok {
		if i, ok := toInt64(v); ok {
			schema.MaxItems = &i
		}
	}
	if v, ok := obj["minProperties"]; ok {
		if i, ok := toInt64(v); ok {
			schema.MinProperties = &i
		}
	}
	if v, ok := obj["maxProperties"]; ok {
		if i, ok := toInt64(v); ok {
			schema.MaxProperties = &i
		}
	}
	if v, ok := obj["minimum"]; ok {
		if f, ok := toFloat64(v); ok {
			schema.Minimum = &f
		}
	}
	if v, ok := obj["maximum"]; ok {
		if f, ok := toFloat64(v); ok {
			schema.Maximum = &f
		}
	}
	if v, ok := obj["minLength"]; ok {
		if i, ok := toInt64(v); ok {
			schema.MinLength = &i
		}
	}
	if v, ok := obj["maxLength"]; ok {
		if i, ok := toInt64(v); ok {
			schema.MaxLength = &i
		}
	}
	if v, ok := obj["format"].(string); ok {
		schema.Format = v
	}
	if v, ok := obj["pattern"].(string); ok {
		schema.Pattern = v
	}
	if v, ok := obj["default"]; ok {
		schema.Default = v
	}
	return schema, true
}

func mapSchemaType(typ string) genai.Type {
	switch strings.ToUpper(typ) {
	case "STRING":
		return genai.TypeString
	case "NUMBER":
		return genai.TypeNumber
	case "INTEGER":
		return genai.TypeInteger
	case "BOOLEAN":
		return genai.TypeBoolean
	case "ARRAY":
		return genai.TypeArray
	case "OBJECT":
		return genai.TypeObject
	default:
		return genai.TypeUnspecified
	}
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}
