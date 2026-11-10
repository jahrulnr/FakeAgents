package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func weatherTools() []map[string]any {
	return []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "Get the weather",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string"},
				},
				"required": []any{"city"},
			},
		},
	}}
}

func chatToolCall(t *testing.T, payload map[string]any) (map[string]any, string) {
	t.Helper()
	choice := payload["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if len(calls) == 0 {
		t.Fatalf("no tool calls: %v", payload)
	}
	call := calls[0].(map[string]any)
	return call, choice["finish_reason"].(string)
}

func TestOpenAIToolErrorMultiTurnScript(t *testing.T) {
	server := newServer(t, nil)
	tools := weatherTools()
	headers := map[string]string{"x-fakeagents-session": "tool-error-openai"}
	user := "please test tool error"

	response, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": user}},
		"tools":    tools,
	}, headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("turn 1 status = %d: %s", response.StatusCode, body)
	}
	if response.Header.Get("X-FakeAgents-Step") != "0" {
		t.Fatalf("turn 1 step = %q", response.Header.Get("X-FakeAgents-Step"))
	}
	payload := decodeMap(t, body)
	firstCall, finish := chatToolCall(t, payload)
	if finish != "tool_calls" {
		t.Fatalf("turn 1 finish = %q", finish)
	}
	if firstCall["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("turn 1 tool = %v", firstCall)
	}

	firstID := firstCall["id"].(string)
	history := []map[string]any{
		{"role": "user", "content": user},
		{"role": "assistant", "content": nil, "tool_calls": []any{firstCall}},
		{"role": "tool", "tool_call_id": firstID, "name": "get_weather", "content": "weather: sunny"},
	}
	response, body = server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": history,
		"tools":    tools,
	}, headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("turn 2 status = %d: %s", response.StatusCode, body)
	}
	if response.Header.Get("X-FakeAgents-Step") != "1" {
		t.Fatalf("turn 2 step = %q", response.Header.Get("X-FakeAgents-Step"))
	}
	payload = decodeMap(t, body)
	secondCall, finish := chatToolCall(t, payload)
	if finish != "tool_calls" {
		t.Fatalf("turn 2 finish = %q", finish)
	}
	function := secondCall["function"].(map[string]any)
	if function["name"] != "fakeagents_missing_tool" {
		t.Fatalf("turn 2 tool = %v", function)
	}
	arguments := function["arguments"].(string)
	if !strings.HasPrefix(arguments, `{"broken":`) {
		t.Fatalf("turn 2 arguments = %q", arguments)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(arguments), &parsed); err == nil {
		t.Fatalf("expected malformed arguments, got %q", arguments)
	}

	secondID := secondCall["id"].(string)
	history = append(history,
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{secondCall}},
		map[string]any{"role": "tool", "tool_call_id": secondID, "name": "fakeagents_missing_tool", "content": "Error: tool not found"},
	)
	response, body = server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": history,
		"tools":    tools,
	}, headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("turn 3 status = %d: %s", response.StatusCode, body)
	}
	if response.Header.Get("X-FakeAgents-Step") != "2" {
		t.Fatalf("turn 3 step = %q", response.Header.Get("X-FakeAgents-Step"))
	}
	payload = decodeMap(t, body)
	choice := payload["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("turn 3 finish = %v", choice["finish_reason"])
	}
	content := choice["message"].(map[string]any)["content"].(string)
	if !strings.Contains(content, "Error: tool not found") {
		t.Fatalf("turn 3 content = %q", content)
	}
}

func TestAnthropicToolErrorMultiTurnScript(t *testing.T) {
	server := newServer(t, nil)
	tools := []map[string]any{{
		"name":         "get_weather",
		"description":  "Get the weather",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"}},
	}}
	user := "please test tool error"
	headers := anthropicHeaders()

	response, body := server.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 512,
		"messages":   []map[string]any{{"role": "user", "content": user}},
		"tools":      tools,
	}, headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("turn 1 status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	var firstUse map[string]any
	for _, raw := range payload["content"].([]any) {
		block := raw.(map[string]any)
		if block["type"] == "tool_use" {
			firstUse = block
		}
	}
	if firstUse == nil || firstUse["name"] != "get_weather" {
		t.Fatalf("turn 1 content = %v", payload["content"])
	}
	firstID := firstUse["id"].(string)

	history := []map[string]any{
		{"role": "user", "content": user},
		{"role": "assistant", "content": []any{firstUse}},
		{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": firstID, "content": "weather: sunny",
		}}},
	}
	response, body = server.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 512,
		"messages":   history,
		"tools":      tools,
	}, headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("turn 2 status = %d: %s", response.StatusCode, body)
	}
	payload = decodeMap(t, body)
	var secondUse map[string]any
	for _, raw := range payload["content"].([]any) {
		block := raw.(map[string]any)
		if block["type"] == "tool_use" {
			secondUse = block
		}
	}
	if secondUse == nil || secondUse["name"] != "fakeagents_missing_tool" {
		t.Fatalf("turn 2 content = %v", payload["content"])
	}
	secondID := secondUse["id"].(string)

	history = append(history,
		map[string]any{"role": "assistant", "content": []any{secondUse}},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": secondID, "content": "Error: tool not found",
		}}},
	)
	response, body = server.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 512,
		"messages":   history,
		"tools":      tools,
	}, headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("turn 3 status = %d: %s", response.StatusCode, body)
	}
	payload = decodeMap(t, body)
	var text strings.Builder
	for _, raw := range payload["content"].([]any) {
		block := raw.(map[string]any)
		if block["type"] == "text" {
			text.WriteString(block["text"].(string))
		}
	}
	if !strings.Contains(text.String(), "Error: tool not found") {
		t.Fatalf("turn 3 text = %q", text.String())
	}
}
