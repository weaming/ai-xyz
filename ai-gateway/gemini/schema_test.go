package gemini

import (
	"testing"

	"google.golang.org/genai"
)

func TestSanitizeSchema(t *testing.T) {
	input := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$defs": map[string]any{
			"City": map[string]any{
				"type": "string",
				"enum": []any{"HK", "SZ"},
			},
		},
		"type": "object",
		"properties": map[string]any{
			"city": map[string]any{"$ref": "#/$defs/City"},
			"version": map[string]any{
				"const": "v1.0",
			},
			"description": map[string]any{
				"type": []any{"string", "null"},
			},
			"tags": map[string]any{
				"type":  "array",
				"items": map[string]any{},
			},
			"status": map[string]any{
				"type": "integer",
				"enum": []any{1, 2, 3},
			},
		},
		"required":             []any{"city", "version", "non_existent_field"},
		"additionalProperties": false,
	}

	sanitized := SanitizeSchema(input).(map[string]any)

	// Check meta keywords stripped
	if _, ok := sanitized["$schema"]; ok {
		t.Fatal("$schema should be stripped")
	}
	if _, ok := sanitized["$defs"]; ok {
		t.Fatal("$defs should be stripped")
	}
	if _, ok := sanitized["additionalProperties"]; ok {
		t.Fatal("additionalProperties should be stripped")
	}

	// Check required fields filtered
	req := sanitized["required"].([]any)
	if len(req) != 2 {
		t.Fatalf("required count = %d, want 2 (%v)", len(req), req)
	}

	props := sanitized["properties"].(map[string]any)

	// Check $ref inlined
	cityProp := props["city"].(map[string]any)
	if cityProp["type"] != "string" {
		t.Fatalf("city type = %v", cityProp["type"])
	}

	// Check const -> enum
	versionProp := props["version"].(map[string]any)
	if _, ok := versionProp["const"]; ok {
		t.Fatal("const should be removed")
	}
	if enums, ok := versionProp["enum"].([]any); !ok || len(enums) != 1 || enums[0] != "v1.0" {
		t.Fatalf("version enum = %v", versionProp["enum"])
	}

	// Check nullable
	descProp := props["description"].(map[string]any)
	if descProp["type"] != "string" || descProp["nullable"] != true {
		t.Fatalf("desc nullable type = %v, nullable = %v", descProp["type"], descProp["nullable"])
	}

	// Check array items default type
	tagsProp := props["tags"].(map[string]any)
	tagsItems := tagsProp["items"].(map[string]any)
	if tagsItems["type"] != "string" {
		t.Fatalf("tags items type = %v", tagsItems["type"])
	}

	// Check integer enum converted to string
	statusProp := props["status"].(map[string]any)
	if statusProp["type"] != "string" {
		t.Fatalf("status type = %v, want string", statusProp["type"])
	}
}

func TestToGenaiSchema(t *testing.T) {
	schema, ok := toGenaiSchema(map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"title":   "CounterParams",
		"type":    "object",
		"properties": map[string]any{
			"count": map[string]any{"type": "integer", "minimum": float64(1)},
			"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": float64(1), "maxItems": float64(10)},
		},
		"required": []any{"count"},
	})
	if !ok || schema == nil {
		t.Fatal("schema conversion failed")
	}
	if schema.Title != "CounterParams" {
		t.Fatalf("schema title = %q", schema.Title)
	}
	if schema.Type != genai.TypeObject {
		t.Fatalf("schema type = %q", schema.Type)
	}
	if len(schema.PropertyOrdering) != 2 || schema.PropertyOrdering[0] != "count" {
		t.Fatalf("property ordering = %#v", schema.PropertyOrdering)
	}
	if schema.Properties["tags"].Items.Type != genai.TypeString {
		t.Fatalf("array item type = %q", schema.Properties["tags"].Items.Type)
	}
	if schema.Properties["tags"].MinItems == nil || *schema.Properties["tags"].MinItems != 1 {
		t.Fatalf("minItems = %#v", schema.Properties["tags"].MinItems)
	}
	if schema.Properties["tags"].MaxItems == nil || *schema.Properties["tags"].MaxItems != 10 {
		t.Fatalf("maxItems = %#v", schema.Properties["tags"].MaxItems)
	}
	if schema.Properties["count"].Minimum == nil || *schema.Properties["count"].Minimum != 1 {
		t.Fatalf("minimum = %#v", schema.Properties["count"].Minimum)
	}
}

func TestNormalizeToolCallID(t *testing.T) {
	got := normalizeToolCallID("call/with spaces and a very long suffix that should be truncated")
	if len(got) != 64 {
		t.Fatalf("normalized ID length = %d", len(got))
	}
	if got[4] != '_' {
		t.Fatalf("normalized ID = %q", got)
	}
}
