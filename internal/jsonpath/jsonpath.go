// Package jsonpath implements the minimal dotted path lookup used by scenario
// matchers. It intentionally supports only data access, never expressions.
package jsonpath

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Lookup resolves path against root. Path segments are separated by dots;
// array indexes may be written as "items.0.name" or "items[0].name".
func Lookup(root any, path string) (any, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return root, true
	}
	path = strings.ReplaceAll(path, "[", ".")
	path = strings.ReplaceAll(path, "]", "")
	segments := strings.Split(path, ".")
	current := root
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[segment]
			if !ok {
				return nil, false
			}
			current = next
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

// Select returns values at a dotted path, expanding "*" over arrays.
func Select(root any, path string) []any {
	path = strings.ReplaceAll(strings.ReplaceAll(path, "[", "."), "]", "")
	segments := strings.Split(path, ".")
	values := []any{root}
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		var next []any
		for _, value := range values {
			if segment == "*" {
				if array, ok := value.([]any); ok {
					next = append(next, array...)
				}
			} else if child, ok := Lookup(value, segment); ok {
				next = append(next, child)
			}
		}
		values = next
	}
	return values
}

// Equal compares two decoded JSON values. Numbers are normalised through
// float64 so 1, 1.0 and json.Number("1") compare equal.
func Equal(a, b any) bool {
	switch left := a.(type) {
	case nil:
		return b == nil
	case string:
		right, ok := b.(string)
		return ok && left == right
	case bool:
		right, ok := b.(bool)
		return ok && left == right
	case float64:
		right, ok := toFloat(b)
		return ok && left == right
	case int:
		right, ok := toFloat(b)
		return ok && float64(left) == right
	case int64:
		right, ok := toFloat(b)
		return ok && float64(left) == right
	case json.Number:
		leftF, err := left.Float64()
		if err != nil {
			return false
		}
		right, ok := toFloat(b)
		return ok && leftF == right
	case []any:
		right, ok := b.([]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for i := range left {
			if !Equal(left[i], right[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		right, ok := b.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for key, value := range left {
			other, exists := right[key]
			if !exists || !Equal(value, other) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
