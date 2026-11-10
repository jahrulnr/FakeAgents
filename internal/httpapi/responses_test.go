package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/config"
)

func responsesRequest(content string) map[string]any {
	return map[string]any{
		"model": "fake-gpt-5",
		"input": content,
	}
}

func TestResponsesNonStreamAndStore(t *testing.T) {
	server := newServer(t, nil)
	response, body := server.post(t, "/v1/responses", map[string]any{
		"model":     "fake-gpt-5",
		"input":     "hello responses",
		"reasoning": map[string]any{"effort": "high", "summary": "auto"},
		"include":   []string{"reasoning.encrypted_content"},
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	if payload["object"] != "response" || payload["status"] != "completed" {
		t.Fatalf("payload = %v", payload)
	}
	output := payload["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output items = %d: %v", len(output), output)
	}
	reasoning := output[0].(map[string]any)
	if reasoning["type"] != "reasoning" {
		t.Fatalf("first item = %v", reasoning)
	}
	if reasoning["encrypted_content"] == nil || reasoning["encrypted_content"] == "" {
		t.Fatalf("missing encrypted_content: %v", reasoning)
	}
	summary := reasoning["summary"].([]any)[0].(map[string]any)
	if summary["text"] == "" {
		t.Fatalf("empty reasoning summary: %v", reasoning)
	}
	message := output[1].(map[string]any)
	content := message["content"].([]any)[0].(map[string]any)
	if content["type"] != "output_text" || !strings.Contains(content["text"].(string), "hello responses") {
		t.Fatalf("message = %v", message)
	}
	usage := payload["usage"].(map[string]any)
	if usage["input_tokens"].(float64) <= 0 || usage["output_tokens"].(float64) <= 0 {
		t.Fatalf("usage = %v", usage)
	}

	id := payload["id"].(string)
	response, body = server.get(t, "/v1/responses/"+id, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("retrieve status = %d: %s", response.StatusCode, body)
	}
	stored := decodeMap(t, body)
	if stored["id"] != id {
		t.Fatalf("stored id = %v", stored["id"])
	}

	request, err := http.NewRequest(http.MethodDelete, server.http.URL+"/v1/responses/"+id, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	deleteResponse, err := server.http.Client().Do(request)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer deleteResponse.Body.Close()
	if deleteResponse.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", deleteResponse.StatusCode)
	}
	response, _ = server.get(t, "/v1/responses/"+id, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted response still available: %d", response.StatusCode)
	}
}

func TestResponsesStream(t *testing.T) {
	server := newServer(t, nil)
	response := server.postStream(t, "/v1/responses", map[string]any{
		"model":     "fake-gpt-5",
		"input":     "stream responses",
		"stream":    true,
		"reasoning": map[string]any{"effort": "medium"},
		"include":   []string{"reasoning.encrypted_content"},
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	events := readSSE(t, response)
	if len(events) < 10 {
		t.Fatalf("too few events: %d", len(events))
	}
	if events[0].Name != "response.created" {
		t.Fatalf("first event = %q", events[0].Name)
	}
	if events[len(events)-1].Name != "response.completed" {
		t.Fatalf("last event = %q", events[len(events)-1].Name)
	}
	var text strings.Builder
	var sawReasoning bool
	var sawEncrypted bool
	lastSequence := -1
	for _, event := range events {
		payload := decodeMap(t, []byte(event.Data))
		sequence := int(payload["sequence_number"].(float64))
		if sequence != lastSequence+1 {
			t.Fatalf("sequence jump: %d -> %d", lastSequence, sequence)
		}
		lastSequence = sequence
		switch event.Name {
		case "response.reasoning_summary_text.delta":
			sawReasoning = true
		case "response.output_text.delta":
			text.WriteString(payload["delta"].(string))
		case "response.output_item.done":
			item := payload["item"].(map[string]any)
			if item["type"] == "reasoning" && item["encrypted_content"] != nil {
				sawEncrypted = true
			}
		case "response.completed":
			completed := payload["response"].(map[string]any)
			if completed["status"] != "completed" {
				t.Fatalf("status = %v", completed["status"])
			}
			if len(completed["output"].([]any)) < 2 {
				t.Fatalf("completed output = %v", completed["output"])
			}
		}
	}
	if !strings.Contains(text.String(), "stream responses") {
		t.Fatalf("text = %q", text.String())
	}
	if !sawReasoning {
		t.Fatal("missing reasoning summary deltas")
	}
	if !sawEncrypted {
		t.Fatal("missing encrypted reasoning content")
	}
}

func TestResponsesPreviousResponseID(t *testing.T) {
	server := newServer(t, nil)
	_, body := server.post(t, "/v1/responses", responsesRequest("my name is Alice"), nil)
	first := decodeMap(t, body)
	id := first["id"].(string)

	response, body := server.post(t, "/v1/responses", map[string]any{
		"model":                "fake-gpt-5",
		"input":                "what did I say?",
		"previous_response_id": id,
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	second := decodeMap(t, body)
	if second["previous_response_id"] != id {
		t.Fatalf("previous_response_id = %v", second["previous_response_id"])
	}
	if second["id"] == id {
		t.Fatal("response id must be unique")
	}

	response, body = server.post(t, "/v1/responses", map[string]any{
		"model":                "fake-gpt-5",
		"input":                "hello",
		"previous_response_id": "resp_missing",
	}, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if errorCode(t, decodeMap(t, body)) != "previous_response_id_not_found" {
		t.Fatalf("body = %s", body)
	}
}

func TestResponsesTamperedEncryptedContent(t *testing.T) {
	server := newServer(t, nil)
	response, body := server.post(t, "/v1/responses", map[string]any{
		"model": "fake-gpt-5",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "hello"},
			{
				"type":              "reasoning",
				"id":                "rs_1",
				"summary":           []map[string]any{{"type": "summary_text", "text": "thinking"}},
				"encrypted_content": "definitely-not-valid",
			},
			{"type": "message", "role": "assistant", "content": "hi"},
		},
	}, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if errorCode(t, decodeMap(t, body)) != "invalid_encrypted_content" {
		t.Fatalf("body = %s", body)
	}
}

func TestResponsesIncomplete(t *testing.T) {
	server := newServer(t, nil)
	response, body := server.post(t, "/v1/responses", map[string]any{
		"model":             "fake-gpt-5",
		"input":             "[[fakeagents:long]]",
		"max_output_tokens": 10,
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	if payload["status"] != "incomplete" {
		t.Fatalf("status = %v", payload["status"])
	}
	details := payload["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Fatalf("details = %v", details)
	}
}

func TestResponsesToolLoop(t *testing.T) {
	server := newServer(t, nil)
	tools := []map[string]any{{
		"type":        "function",
		"name":        "lookup",
		"description": "Look something up",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
			"required": []any{"query"},
		},
	}}
	response, body := server.post(t, "/v1/responses", map[string]any{
		"model": "fake-gpt-5",
		"input": "look it up [[fakeagents:tool-call]]",
		"tools": tools,
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	output := payload["output"].([]any)
	var call map[string]any
	for _, raw := range output {
		item := raw.(map[string]any)
		if item["type"] == "function_call" {
			call = item
		}
	}
	if call == nil {
		t.Fatalf("missing function_call item: %v", output)
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(call["arguments"].(string)), &arguments); err != nil {
		t.Fatalf("arguments: %v", err)
	}
	if arguments["query"] != "example" {
		t.Fatalf("arguments = %v", arguments)
	}

	response, body = server.post(t, "/v1/responses", map[string]any{
		"model": "fake-gpt-5",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "look it up"},
			call,
			{"type": "function_call_output", "call_id": call["call_id"], "output": "result: 42"},
		},
		"tools": tools,
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("follow-up status = %d: %s", response.StatusCode, body)
	}
	payload = decodeMap(t, body)
	text := responsesText(payload)
	if !strings.Contains(text, "result: 42") {
		t.Fatalf("final text = %q", text)
	}
}

func responsesText(payload map[string]any) string {
	var builder strings.Builder
	output, _ := payload["output"].([]any)
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item["type"] != "message" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, rawPart := range content {
			part, _ := rawPart.(map[string]any)
			if text, ok := part["text"].(string); ok {
				builder.WriteString(text)
			}
			if refusal, ok := part["refusal"].(string); ok {
				builder.WriteString(refusal)
			}
		}
	}
	return builder.String()
}

func TestResponsesStreamFailure(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		cfg.Defaults.Latency.Enabled = boolPointer(false)
	})
	response := server.postStream(t, "/v1/responses", map[string]any{
		"model":  "fake-gpt-5",
		"input":  "[[fakeagents:mid-stream-error]]",
		"stream": true,
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	events := readSSE(t, response)
	last := events[len(events)-1]
	if last.Name != "response.failed" {
		t.Fatalf("last event = %q", last.Name)
	}
	payload := decodeMap(t, []byte(last.Data))
	failed := payload["response"].(map[string]any)
	if failed["status"] != "failed" {
		t.Fatalf("status = %v", failed["status"])
	}
}

func TestResponsesEncryptedRoundTrip(t *testing.T) {
	server := newServer(t, nil)
	_, body := server.post(t, "/v1/responses", map[string]any{
		"model":     "fake-gpt-5",
		"input":     "remember this",
		"reasoning": map[string]any{"effort": "medium"},
		"include":   []string{"reasoning.encrypted_content"},
	}, nil)
	first := decodeMap(t, body)
	var reasoning map[string]any
	for _, raw := range first["output"].([]any) {
		item := raw.(map[string]any)
		if item["type"] == "reasoning" {
			reasoning = item
			break
		}
	}
	if reasoning == nil {
		t.Fatal("missing reasoning item")
	}

	response, body := server.post(t, "/v1/responses", map[string]any{
		"model": "fake-gpt-5",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "remember this"},
			reasoning,
			{"type": "message", "role": "assistant", "content": "noted"},
			{"type": "message", "role": "user", "content": "what did I say?"},
		},
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d: %s", response.StatusCode, body)
	}
}
