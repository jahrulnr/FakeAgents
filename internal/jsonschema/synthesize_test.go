package jsonschema_test

import (
	"encoding/json"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/jsonschema"
)

func TestSampleObject(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"city": map[string]any{"type": "string"},
			"days": map[string]any{"type": "integer", "minimum": 3},
			"unit": map[string]any{"type": "string", "enum": []any{"celsius", "fahrenheit"}},
		},
		"required": []any{"city", "days"},
	}
	value, err := jsonschema.SampleJSON(schema, nil)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		t.Fatalf("invalid JSON %q: %v", value, err)
	}
	if decoded["city"] != "example" {
		t.Fatalf("city = %v", decoded["city"])
	}
	if decoded["days"].(float64) != 3 {
		t.Fatalf("days = %v", decoded["days"])
	}
	if decoded["unit"] != "celsius" {
		t.Fatalf("unit = %v", decoded["unit"])
	}
}

func TestSampleRefAndArray(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"$defs": map[string]any{
			"item": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string", "format": "uuid"},
				},
			},
		},
		"properties": map[string]any{
			"items": map[string]any{
				"type":     "array",
				"minItems": 2,
				"items":    map[string]any{"$ref": "#/$defs/item"},
			},
		},
	}
	value := jsonschema.Sample(schema, nil).(map[string]any)
	items, ok := value["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %#v", value["items"])
	}
	id := items[0].(map[string]any)["id"]
	if id != "9f1c2b3a-4d5e-4f60-8a7b-1c2d3e4f5a6b" {
		t.Fatalf("uuid = %v", id)
	}
}

func TestSampleConstAndDefault(t *testing.T) {
	if got := jsonschema.Sample(map[string]any{"const": "fixed"}, nil); got != "fixed" {
		t.Fatalf("const = %v", got)
	}
	if got := jsonschema.Sample(map[string]any{"default": 7}, nil); got != 7 {
		t.Fatalf("default = %v", got)
	}
}
