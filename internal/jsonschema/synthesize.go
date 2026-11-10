// Package jsonschema synthesizes deterministic example values from JSON Schema
// documents. FakeAgents uses it for tool-call arguments and structured outputs
// so clients receive well-formed data without a real model.
package jsonschema

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
)

// MaxDepth bounds recursion for hostile or deeply nested schemas.
const MaxDepth = 24

// Sample returns a value that validates against the supplied schema in the
// common case. It understands local $ref, object properties, arrays, enums,
// const/default/examples and the usual primitive formats.
func Sample(schema any, rng *rand.Rand) any {
	if rng == nil {
		rng = rand.New(rand.NewPCG(1, 2))
	}
	return sample(schema, rng, 0, nil)
}

// SampleJSON returns the compact JSON encoding of Sample.
func SampleJSON(schema any, rng *rand.Rand) (string, error) {
	b, err := json.Marshal(Sample(schema, rng))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func sample(schema any, rng *rand.Rand, depth int, root map[string]any) any {
	if depth > MaxDepth {
		return nil
	}
	switch s := schema.(type) {
	case nil:
		return nil
	case bool:
		if !s {
			return nil
		}
		return map[string]any{}
	case map[string]any:
		return sampleObject(s, rng, depth, root)
	default:
		return nil
	}
}

func sampleObject(schema map[string]any, rng *rand.Rand, depth int, root map[string]any) any {
	if root == nil {
		root = schema
	}
	if ref, ok := schema["$ref"].(string); ok {
		if resolved, ok := resolveRef(root, ref); ok {
			return sample(resolved, rng, depth+1, root)
		}
	}
	for _, key := range []string{"const", "default"} {
		if v, ok := schema[key]; ok {
			return v
		}
	}
	if examples, ok := schema["examples"].([]any); ok && len(examples) > 0 {
		return examples[0]
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		return enum[rng.IntN(len(enum))]
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		if list, ok := schema[key].([]any); ok && len(list) > 0 {
			return sample(list[0], rng, depth+1, root)
		}
	}
	if list, ok := schema["allOf"].([]any); ok && len(list) > 0 {
		merged := map[string]any{}
		for _, entry := range list {
			if m, ok := entry.(map[string]any); ok {
				mergeSchema(merged, m)
			}
		}
		merged = mergeSchema(merged, schema)
		return sample(merged, rng, depth+1, root)
	}

	typ := schemaType(schema)
	switch typ {
	case "object":
		return sampleStruct(schema, rng, depth, root)
	case "array":
		return sampleArray(schema, rng, depth, root)
	case "string":
		return sampleString(schema, rng)
	case "integer":
		return sampleNumber(schema, true, rng)
	case "number":
		return sampleNumber(schema, false, rng)
	case "boolean":
		return true
	case "null":
		return nil
	default:
		if _, ok := schema["properties"]; ok {
			return sampleStruct(schema, rng, depth, root)
		}
		if _, ok := schema["items"]; ok {
			return sampleArray(schema, rng, depth, root)
		}
		return "example"
	}
}

func schemaType(schema map[string]any) string {
	if t, ok := schema["type"].(string); ok {
		return t
	}
	if list, ok := schema["type"].([]any); ok && len(list) > 0 {
		if t, ok := list[0].(string); ok {
			return t
		}
	}
	return ""
}

func sampleStruct(schema map[string]any, rng *rand.Rand, depth int, root map[string]any) any {
	out := map[string]any{}
	props, _ := schema["properties"].(map[string]any)
	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out[key] = sample(props[key], rng, depth+1, root)
	}
	if len(props) == 0 {
		if additional, ok := schema["additionalProperties"].(map[string]any); ok {
			out["key"] = sample(additional, rng, depth+1, root)
		}
	}
	return out
}

func sampleArray(schema map[string]any, rng *rand.Rand, depth int, root map[string]any) any {
	items, _ := schema["items"].(map[string]any)
	minItems := max(int(readNumber(schema["minItems"], 1)), 0)
	if maxItems, ok := schema["maxItems"]; ok {
		if int(readNumber(maxItems, 0)) < minItems {
			minItems = int(readNumber(maxItems, 0))
		}
	}
	count := minItems
	if count == 0 {
		// An empty array is a valid but uninteresting sample; one element makes
		// streaming and rendering tests more useful.
		count = 1
	}
	if count > 3 {
		count = 3
	}
	out := make([]any, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, sample(items, rng, depth+1, root))
	}
	return out
}

func sampleString(schema map[string]any, rng *rand.Rand) string {
	format, _ := schema["format"].(string)
	var value string
	switch format {
	case "date":
		value = "2026-01-15"
	case "date-time":
		value = "2026-01-15T10:30:00Z"
	case "time":
		value = "10:30:00Z"
	case "email", "idn-email":
		value = "user@example.com"
	case "uuid":
		value = "9f1c2b3a-4d5e-4f60-8a7b-1c2d3e4f5a6b"
	case "uri", "url", "iri":
		value = "https://example.com/resource"
	case "hostname":
		value = "example.com"
	case "ipv4":
		value = "192.0.2.10"
	case "ipv6":
		value = "2001:db8::1"
	default:
		value = "example"
	}
	if pattern, ok := schema["pattern"].(string); ok && pattern != "" {
		// Patterns are arbitrary regular expressions; a generic value is the
		// safest deterministic sample.
		value = "example"
	}
	if min := int(readNumber(schema["minLength"], 0)); min > len(value) {
		value += strings.Repeat("x", min-len(value))
	}
	if max, ok := schema["maxLength"]; ok {
		limit := int(readNumber(max, 0))
		if limit > 0 && len(value) > limit {
			value = value[:limit]
		}
	}
	_ = rng
	return value
}

func sampleNumber(schema map[string]any, integer bool, rng *rand.Rand) any {
	min, hasMin := schema["minimum"]
	max, hasMax := schema["maximum"]
	value := 1.0
	if hasMin {
		value = readNumber(min, 0)
	}
	if hasMax && value > readNumber(max, 0) {
		value = readNumber(max, 0)
	}
	if excl, ok := schema["exclusiveMinimum"]; ok {
		value = math.Max(value, readNumber(excl, 0)+1)
	}
	if excl, ok := schema["exclusiveMaximum"]; ok {
		candidate := readNumber(excl, 0) - 1
		if candidate >= value {
			value = candidate
		}
	}
	if multiple, ok := schema["multipleOf"]; ok {
		step := readNumber(multiple, 0)
		if step > 0 {
			value = math.Ceil(value/step) * step
		}
	}
	if integer {
		return int(value)
	}
	return value
}

func readNumber(v any, fallback float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, err := n.Float64()
		if err == nil {
			return f
		}
	}
	return fallback
}

func resolveRef(root map[string]any, ref string) (any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	path := strings.TrimPrefix(ref, "#/")
	segments := strings.Split(path, "/")
	var current any = root
	for _, segment := range segments {
		segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func mergeSchema(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for key, value := range src {
		if key == "properties" {
			props, _ := value.(map[string]any)
			existing, _ := dst["properties"].(map[string]any)
			if existing == nil {
				existing = map[string]any{}
			}
			maps.Copy(existing, props)
			dst["properties"] = existing
			continue
		}
		if key == "required" {
			existing, _ := dst["required"].([]any)
			next, _ := value.([]any)
			dst["required"] = append(existing, next...)
			continue
		}
		if _, exists := dst[key]; !exists {
			dst[key] = value
		}
	}
	return dst
}

// Explain describes the synthesis strategy for diagnostics.
func Explain(schema any) string {
	if m, ok := schema.(map[string]any); ok {
		if typ := schemaType(m); typ != "" {
			return fmt.Sprintf("synthesized %s value", typ)
		}
	}
	return "synthesized value"
}
