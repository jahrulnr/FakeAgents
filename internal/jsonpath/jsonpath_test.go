package jsonpath_test

import (
	"testing"

	"github.com/jahrulnr/fakeagents/internal/jsonpath"
)

func TestLookup(t *testing.T) {
	root := map[string]any{
		"reasoning": map[string]any{"effort": "high"},
		"messages": []any{
			map[string]any{"role": "user"},
			map[string]any{"role": "assistant"},
		},
	}
	if value, ok := jsonpath.Lookup(root, "reasoning.effort"); !ok || value != "high" {
		t.Fatalf("value = %v, ok = %v", value, ok)
	}
	if value, ok := jsonpath.Lookup(root, "messages.1.role"); !ok || value != "assistant" {
		t.Fatalf("value = %v, ok = %v", value, ok)
	}
	if value, ok := jsonpath.Lookup(root, "messages[0].role"); !ok || value != "user" {
		t.Fatalf("bracket value = %v, ok = %v", value, ok)
	}
	if _, ok := jsonpath.Lookup(root, "missing.path"); ok {
		t.Fatal("missing path resolved")
	}
	if _, ok := jsonpath.Lookup(root, "messages.9"); ok {
		t.Fatal("out-of-range index resolved")
	}
}

func TestEqual(t *testing.T) {
	if !jsonpath.Equal(float64(1), 1) {
		t.Fatal("numeric equality failed")
	}
	if !jsonpath.Equal([]any{"a", float64(2)}, []any{"a", 2}) {
		t.Fatal("array equality failed")
	}
	if !jsonpath.Equal(map[string]any{"a": true}, map[string]any{"a": true}) {
		t.Fatal("map equality failed")
	}
	if jsonpath.Equal("1", 1) {
		t.Fatal("string and number must not compare equal")
	}
}
