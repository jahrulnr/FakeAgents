package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/scenario"
)

func anthropicHeaders() map[string]string {
	return map[string]string{
		"x-api-key":         "sk-ant-fake",
		"anthropic-version": "2023-06-01",
	}
}

func TestAnthropicMessages(t *testing.T) {
	server := newServer(t, nil)
	response, body := server.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 256,
		"system":     "You are concise.",
		"messages":   []map[string]any{{"role": "user", "content": "hello claude"}},
	}, anthropicHeaders())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if response.Header.Get("request-id") == "" {
		t.Fatal("missing request-id header")
	}
	payload := decodeMap(t, body)
	if payload["type"] != "message" || payload["role"] != "assistant" {
		t.Fatalf("payload = %v", payload)
	}
	content := payload["content"].([]any)
	text := content[0].(map[string]any)
	if !strings.Contains(text["text"].(string), "hello claude") {
		t.Fatalf("content = %v", text)
	}
	if payload["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", payload["stop_reason"])
	}
	usage := payload["usage"].(map[string]any)
	if usage["input_tokens"].(float64) <= 0 || usage["output_tokens"].(float64) <= 0 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestAnthropicThinkingStreamAndReplay(t *testing.T) {
	server := newServer(t, nil)
	response := server.postStream(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 1024,
		"stream":     true,
		"thinking":   map[string]any{"type": "adaptive", "display": "summarized"},
		"messages":   []map[string]any{{"role": "user", "content": "why is the sky blue?"}},
	}, anthropicHeaders())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	events := readSSE(t, response)
	if len(events) == 0 || events[0].Name != "message_start" {
		t.Fatalf("first event = %+v", events)
	}
	var thinking, signature, text, stopReason strings.Builder
	for _, event := range events {
		if event.Data == "" {
			continue
		}
		payload := decodeMap(t, []byte(event.Data))
		switch event.Name {
		case "content_block_delta":
			delta := payload["delta"].(map[string]any)
			switch delta["type"] {
			case "thinking_delta":
				thinking.WriteString(delta["thinking"].(string))
			case "signature_delta":
				signature.WriteString(delta["signature"].(string))
			case "text_delta":
				text.WriteString(delta["text"].(string))
			}
		case "message_delta":
			delta := payload["delta"].(map[string]any)
			stopReason.WriteString(delta["stop_reason"].(string))
		}
	}
	if thinking.Len() == 0 {
		t.Fatal("missing thinking deltas")
	}
	if signature.Len() == 0 {
		t.Fatal("missing signature delta")
	}
	if text.Len() == 0 {
		t.Fatal("missing text deltas")
	}
	if stopReason.String() != "end_turn" {
		t.Fatalf("stop_reason = %q", stopReason.String())
	}

	replay := map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 256,
		"messages": []map[string]any{
			{"role": "user", "content": "why is the sky blue?"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "thinking", "thinking": thinking.String(), "signature": signature.String()},
				{"type": "text", "text": text.String()},
			}},
			{"role": "user", "content": "summarize that"},
		},
	}
	response, body := server.post(t, "/v1/messages", replay, anthropicHeaders())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d: %s", response.StatusCode, body)
	}

	replayedThinking := replay["messages"].([]map[string]any)[1]["content"].([]map[string]any)[0]
	replayedThinking["thinking"] = thinking.String() + " changed"
	response, body = server.post(t, "/v1/messages", replay, anthropicHeaders())
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("changed thinking text status = %d: %s", response.StatusCode, body)
	}
	replayedThinking["thinking"] = thinking.String()

	tampered := signature.String()
	index := len(tampered) / 2
	replacement := byte('A')
	if tampered[index] == 'A' {
		replacement = 'B'
	}
	tampered = tampered[:index] + string(replacement) + tampered[index+1:]
	replayedThinking["signature"] = tampered
	response, body = server.post(t, "/v1/messages", replay, anthropicHeaders())
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("tampered status = %d: %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "cannot be modified") {
		t.Fatalf("body = %s", body)
	}
}

func TestAnthropicToolUse(t *testing.T) {
	server := newServer(t, nil)
	tools := []map[string]any{{
		"name":         "get_weather",
		"description":  "Get weather",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"}},
	}}
	response, body := server.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 512,
		"tools":      tools,
		"messages":   []map[string]any{{"role": "user", "content": "weather? [[fakeagents:tool-call]]"}},
	}, anthropicHeaders())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	if payload["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", payload["stop_reason"])
	}
	content := payload["content"].([]any)
	var toolUse map[string]any
	for _, raw := range content {
		block := raw.(map[string]any)
		if block["type"] == "tool_use" {
			toolUse = block
		}
	}
	if toolUse == nil {
		t.Fatalf("missing tool_use block: %v", content)
	}
	input := toolUse["input"].(map[string]any)
	if input["city"] != "example" {
		t.Fatalf("input = %v", input)
	}

	response = server.postStream(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 512,
		"stream":     true,
		"tools":      tools,
		"messages":   []map[string]any{{"role": "user", "content": "weather? [[fakeagents:tool-call]]"}},
	}, anthropicHeaders())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", response.StatusCode)
	}
	events := readSSE(t, response)
	var partial strings.Builder
	var stop string
	for _, event := range events {
		payload := decodeMap(t, []byte(event.Data))
		switch event.Name {
		case "content_block_delta":
			delta := payload["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				partial.WriteString(delta["partial_json"].(string))
			}
		case "message_delta":
			stop = payload["delta"].(map[string]any)["stop_reason"].(string)
		}
	}
	if stop != "tool_use" {
		t.Fatalf("stop_reason = %q", stop)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(partial.String()), &decoded); err != nil {
		t.Fatalf("input_json_delta is not valid JSON: %q: %v", partial.String(), err)
	}
	if decoded["city"] != "example" {
		t.Fatalf("decoded = %v", decoded)
	}
}

func TestAnthropicValidationAndErrors(t *testing.T) {
	server := newServer(t, nil)
	response, body := server.post(t, "/v1/messages", map[string]any{
		"model":    "fake-claude-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, anthropicHeaders())
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "max_tokens") {
		t.Fatalf("missing max_tokens: %d %s", response.StatusCode, body)
	}

	response, body = server.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 64,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}, map[string]string{"x-api-key": "x"})
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "anthropic-version") {
		t.Fatalf("missing version: %d %s", response.StatusCode, body)
	}

	response, body = server.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 64,
		"messages":   []map[string]any{{"role": "user", "content": "[[fakeagents:billing]]"}},
	}, anthropicHeaders())
	if response.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("billing status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	detail := payload["error"].(map[string]any)
	if detail["type"] != "billing_error" {
		t.Fatalf("error = %v", detail)
	}
	if payload["request_id"] == nil || response.Header.Get("request-id") == "" {
		t.Fatal("missing request_id")
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	server := newServer(t, nil)
	response, body := server.post(t, "/v1/messages/count_tokens", map[string]any{
		"model":    "fake-claude-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "count these tokens please"}},
	}, anthropicHeaders())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	if payload["input_tokens"].(float64) <= 0 {
		t.Fatalf("payload = %v", payload)
	}
}

func TestChatMidStreamError(t *testing.T) {
	server := newServer(t, nil)
	response := server.postStream(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "[[fakeagents:mid-stream-error]]"}},
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	events := readSSE(t, response)
	var sawError, sawDone bool
	for _, event := range events {
		if event.Data == "[DONE]" {
			sawDone = true
		}
		if strings.Contains(event.Data, `"error"`) {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("missing mid-stream error: %+v", events)
	}
	if sawDone {
		t.Fatal("stream must not send [DONE] after an error")
	}
}

func TestChatMidStreamAbort(t *testing.T) {
	server := newServer(t, nil)
	response := server.postStream(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "[[fakeagents:mid-stream-abort]]"}},
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	events := readSSE(t, response)
	var sawDone, sawFinish bool
	for _, event := range events {
		if event.Data == "[DONE]" {
			sawDone = true
		}
		if strings.Contains(event.Data, `"finish_reason":"`) {
			sawFinish = true
		}
	}
	if sawDone || sawFinish {
		t.Fatalf("aborted stream must not include a terminal frame: %+v", events)
	}
}

func TestAnthropicMidStreamError(t *testing.T) {
	server := newServer(t, nil)
	response := server.postStream(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 512,
		"stream":     true,
		"messages":   []map[string]any{{"role": "user", "content": "[[fakeagents:mid-stream-error]]"}},
	}, anthropicHeaders())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	events := readSSE(t, response)
	last := events[len(events)-1]
	if last.Name != "error" {
		t.Fatalf("last event = %q", last.Name)
	}
	payload := decodeMap(t, []byte(last.Data))
	detail := payload["error"].(map[string]any)
	if detail["type"] != "overloaded_error" {
		t.Fatalf("error = %v", detail)
	}
}

func TestAnthropicRedactedThinking(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		redacted := true
		cfg.Scenarios = append(cfg.Scenarios, scenario.Spec{
			Name:  "redacted",
			Match: scenario.Match{Triggers: []string{"redacted"}},
			Respond: scenario.Respond{
				Type:              scenario.TypeText,
				RedactedReasoning: &redacted,
			},
		})
	})
	response, body := server.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 512,
		"thinking":   map[string]any{"type": "adaptive"},
		"messages":   []map[string]any{{"role": "user", "content": "[[fakeagents:redacted]]"}},
	}, anthropicHeaders())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	var redactedBlock map[string]any
	for _, raw := range payload["content"].([]any) {
		block := raw.(map[string]any)
		if block["type"] == "redacted_thinking" {
			redactedBlock = block
		}
	}
	if redactedBlock == nil || redactedBlock["data"] == "" {
		t.Fatalf("missing redacted_thinking block: %v", payload["content"])
	}

	replay := map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 256,
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": []map[string]any{
				redactedBlock,
				{"type": "text", "text": "answer"},
			}},
			{"role": "user", "content": "continue"},
		},
	}
	response, body = server.post(t, "/v1/messages", replay, anthropicHeaders())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d: %s", response.StatusCode, body)
	}

	data := redactedBlock["data"].(string)
	index := len(data) / 2
	replacement := byte('A')
	if data[index] == 'A' {
		replacement = 'B'
	}
	redactedBlock["data"] = data[:index] + string(replacement) + data[index+1:]
	response, body = server.post(t, "/v1/messages", replay, anthropicHeaders())
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("tampered status = %d: %s", response.StatusCode, body)
	}
}
