package httpapi_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/app"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/httpapi"
	"github.com/jahrulnr/fakeagents/internal/scenario"
)

type testServer struct {
	app  *app.App
	http *httptest.Server
	cfg  *config.Config
}

func newServer(t *testing.T, mutate func(*config.Config)) *testServer {
	t.Helper()
	cfg := config.Default()
	disabled := false
	cfg.Defaults.Latency.Enabled = &disabled
	cfg.Encryption.Key = "integration-test-key"
	cfg.Logging.Level = "error"
	cfg.Logging.Output = "discard"
	cfg.Logging.AccessLog = &disabled
	cfg.Debug.Enabled = boolPointer(true)
	if mutate != nil {
		mutate(cfg)
	}
	application, err := app.New(cfg, httpapi.BuildInfo{Version: "test"})
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	server := httptest.NewServer(application.Server.Handler())
	t.Cleanup(func() {
		server.Close()
		_ = application.Close()
	})
	return &testServer{app: application, http: server, cfg: cfg}
}

func boolPointer(value bool) *bool { return &value }

func (s *testServer) post(t *testing.T, path string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, s.http.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := s.http.Client().Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response, data
}

func (s *testServer) postStream(t *testing.T, path string, body any, headers map[string]string) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, s.http.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := s.http.Client().Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return response
}

func (s *testServer) get(t *testing.T, path string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, s.http.URL+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := s.http.Client().Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response, data
}

type sseEvent struct {
	Name string
	Data string
}

func readSSE(t *testing.T, response *http.Response) []sseEvent {
	t.Helper()
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var events []sseEvent
	var name string
	var data []string
	flush := func() {
		if len(data) == 0 {
			name = ""
			return
		}
		events = append(events, sseEvent{Name: name, Data: strings.Join(data, "\n")})
		name = ""
		data = nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if after, ok := strings.CutPrefix(line, "event:"); ok {
			name = strings.TrimSpace(after)
			continue
		}
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(after))
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	return events
}

func decodeMap(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode JSON %q: %v", data, err)
	}
	return out
}

func errorCode(t *testing.T, body map[string]any) string {
	t.Helper()
	detail, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object: %v", body)
	}
	code, _ := detail["code"].(string)
	return code
}

func TestChatCompletions(t *testing.T) {
	server := newServer(t, nil)
	response, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if response.Header.Get("x-request-id") == "" {
		t.Fatal("missing x-request-id")
	}
	payload := decodeMap(t, body)
	if !strings.HasPrefix(payload["id"].(string), "chatcmpl-") {
		t.Fatalf("id = %v", payload["id"])
	}
	if payload["object"] != "chat.completion" {
		t.Fatalf("object = %v", payload["object"])
	}
	choices := payload["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if !strings.Contains(fmt.Sprint(message["content"]), "hello") {
		t.Fatalf("content = %v", message["content"])
	}
	usage := payload["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) <= 0 || usage["completion_tokens"].(float64) <= 0 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestChatThinkingOnOff(t *testing.T) {
	server := newServer(t, nil)
	response, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":            "fake-gpt-4o",
		"reasoning_effort": "high",
		"messages":         []map[string]any{{"role": "user", "content": "think"}},
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	message := payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["reasoning_content"] == nil || message["reasoning_content"] == "" {
		t.Fatalf("missing reasoning_content: %v", message)
	}
	details := payload["usage"].(map[string]any)["completion_tokens_details"].(map[string]any)
	if details["reasoning_tokens"].(float64) <= 0 {
		t.Fatalf("reasoning tokens = %v", details)
	}

	_, body = server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "think"}},
	}, nil)
	payload = decodeMap(t, body)
	message = payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := message["reasoning_content"]; ok {
		t.Fatalf("reasoning_content must be absent when thinking is off: %v", message)
	}
}

func TestChatStreamAndUsage(t *testing.T) {
	server := newServer(t, nil)
	response := server.postStream(t, "/v1/chat/completions", map[string]any{
		"model":          "fake-gpt-4o",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []map[string]any{{"role": "user", "content": "stream me [[fakeagents:thinking]]"}},
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type = %q", response.Header.Get("Content-Type"))
	}
	events := readSSE(t, response)
	if len(events) < 5 {
		t.Fatalf("too few events: %d", len(events))
	}
	var content, reasoning strings.Builder
	var finish string
	var usage map[string]any
	var done bool
	for _, event := range events {
		if event.Data == "[DONE]" {
			done = true
			continue
		}
		chunk := decodeMap(t, []byte(event.Data))
		choices, _ := chunk["choices"].([]any)
		if len(choices) > 0 {
			if usage, ok := chunk["usage"]; !ok || usage != nil {
				t.Fatalf("ordinary chunk must include null usage: %v", chunk)
			}
			choice := choices[0].(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if value, ok := delta["content"].(string); ok {
				content.WriteString(value)
			}
			if value, ok := delta["reasoning_content"].(string); ok {
				reasoning.WriteString(value)
			}
			if value, ok := choice["finish_reason"].(string); ok {
				finish = value
			}
		}
		if chunk["usage"] != nil {
			usage, _ = chunk["usage"].(map[string]any)
		}
	}
	if !done {
		t.Fatal("missing [DONE]")
	}
	if !strings.Contains(content.String(), "stream me") {
		t.Fatalf("content = %q", content.String())
	}
	if !strings.Contains(reasoning.String(), "asking about") {
		t.Fatalf("reasoning = %q", reasoning.String())
	}
	if finish != "stop" {
		t.Fatalf("finish = %q", finish)
	}
	if usage == nil || usage["completion_tokens"].(float64) <= 0 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestChatToolLoop(t *testing.T) {
	server := newServer(t, nil)
	headers := map[string]string{"x-fakeagents-session": "tool-loop"}
	tools := []map[string]any{{
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
	response, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "weather? [[fakeagents:tool-call]]"}},
		"tools":    tools,
	}, headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	choice := payload["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish = %v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if message["content"] != nil {
		t.Fatalf("content must be null for tool-only responses: %v", message["content"])
	}
	calls := message["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	callID := call["id"].(string)
	var arguments map[string]any
	if err := json.Unmarshal([]byte(call["function"].(map[string]any)["arguments"].(string)), &arguments); err != nil {
		t.Fatalf("arguments: %v", err)
	}
	if arguments["city"] != "example" {
		t.Fatalf("arguments = %v", arguments)
	}

	history := []map[string]any{
		{"role": "user", "content": "weather?"},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{call}},
		{"role": "tool", "tool_call_id": callID, "name": "get_weather", "content": `{"temp":31}`},
	}
	follow := map[string]any{"model": "fake-gpt-4o", "messages": history, "tools": tools}
	response, body = server.post(t, "/v1/chat/completions", follow, headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload = decodeMap(t, body)
	choice = payload["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish = %v", choice["finish_reason"])
	}
	content := fmt.Sprint(choice["message"].(map[string]any)["content"])
	if !strings.Contains(content, `"temp":31`) || !strings.Contains(content, "get_weather") {
		t.Fatalf("final content = %q", content)
	}

	follow["messages"] = append(history,
		choice["message"].(map[string]any),
		map[string]any{"role": "user", "content": "check again"},
	)
	follow["tool_choice"] = "required"
	response, body = server.post(t, "/v1/chat/completions", follow, headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("new user turn status = %d: %s", response.StatusCode, body)
	}
	choice = decodeMap(t, body)["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("new user turn should call the tool: %v", choice)
	}
}

func TestRejectsUnmatchedToolResult(t *testing.T) {
	server := newServer(t, nil)
	tests := []struct {
		path, body string
		headers    map[string]string
	}{
		{"/v1/chat/completions", `{"model":"fake-gpt-4o","messages":[{"role":"tool","tool_call_id":"missing","content":"result"}]}`, nil},
		{"/v1/responses", `{"model":"fake-gpt-5","input":[{"type":"function_call_output","call_id":"missing","output":"result"}]}`, nil},
		{"/v1/messages", `{"model":"fake-claude-sonnet","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"missing","content":"result"}]}]}`, anthropicHeaders()},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			res, body := server.post(t, tt.path, json.RawMessage(tt.body), tt.headers)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", res.StatusCode, body)
			}
		})
	}
}

func TestStopSequenceCutsOutputAtEarliestMatch(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		cfg.Scenarios = append(cfg.Scenarios, scenario.Spec{
			Name:    "stop-fixture",
			Respond: scenario.Respond{Text: "prefix 🛑 skipped <END> suffix"},
		})
	})
	headers := anthropicHeaders()
	headers["X-FakeAgents-Scenario"] = "stop-fixture"
	res, body := server.post(t, "/v1/messages", map[string]any{
		"model":          "fake-claude-sonnet",
		"max_tokens":     128,
		"messages":       []map[string]any{{"role": "user", "content": "hello"}},
		"stop_sequences": []string{"<END>", "🛑"},
	}, headers)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	reply := decodeMap(t, body)
	text := reply["content"].([]any)[0].(map[string]any)["text"]
	if text != "prefix " || reply["stop_reason"] != "stop_sequence" || reply["stop_sequence"] != "🛑" {
		t.Fatalf("unexpected stop response: %v", reply)
	}
}

func TestChatScenarioErrors(t *testing.T) {
	server := newServer(t, nil)
	response, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "[[fakeagents:billing]]"}},
	}, nil)
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	if errorCode(t, payload) != "insufficient_quota" {
		t.Fatalf("body = %v", payload)
	}

	response, body = server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "[[fakeagents:rate-limit]]"}},
	}, nil)
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if response.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}

	response, body = server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "[[fakeagents:does-not-exist]]"}},
	}, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if errorCode(t, decodeMap(t, body)) != "scenario_not_found" {
		t.Fatalf("body = %s", body)
	}
}

func TestRequiredHeader(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		cfg.RequiredHeaders = []config.HeaderRule{{
			Name:      "x-opencode-session",
			Providers: []string{"openai"},
		}}
	})
	response, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if errorCode(t, decodeMap(t, body)) != "missing_required_header" {
		t.Fatalf("body = %s", body)
	}

	response, body = server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, map[string]string{"x-opencode-session": "session-abc"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if response.Header.Get("X-FakeAgents-Session") == "" {
		t.Fatal("session header missing")
	}

	// The rule is scoped to OpenAI; Anthropic requests still work.
	response, body = server.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 128,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}, map[string]string{"anthropic-version": "2023-06-01"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("anthropic status = %d: %s", response.StatusCode, body)
	}
}

func TestAuthModes(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		cfg.Auth.Mode = "required"
		cfg.Auth.Keys = []config.APIKey{{Key: "sk-test", Name: "test"}}
	})
	response, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d: %s", response.StatusCode, body)
	}
	response, body = server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, map[string]string{"Authorization": "Bearer wrong"})
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad key status = %d: %s", response.StatusCode, body)
	}
	response, body = server.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 64,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}, map[string]string{"x-api-key": "sk-test", "anthropic-version": "2023-06-01"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("valid key status = %d: %s", response.StatusCode, body)
	}
	response, _ = server.get(t, "/healthz", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health should be exempt, got %d", response.StatusCode)
	}
}

func TestRateLimit(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		cfg.RateLimit.Enabled = true
		cfg.RateLimit.RequestsPerMinute = 1
		cfg.RateLimit.TokensPerMinute = 0
	})
	body := map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	response, payload := server.post(t, "/v1/chat/completions", body, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d: %s", response.StatusCode, payload)
	}
	if response.Header.Get("x-ratelimit-remaining-requests") != "0" {
		t.Fatalf("remaining = %q", response.Header.Get("x-ratelimit-remaining-requests"))
	}
	response, payload = server.post(t, "/v1/chat/completions", body, nil)
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second status = %d: %s", response.StatusCode, payload)
	}
	if response.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}

func TestCORS(t *testing.T) {
	server := newServer(t, nil)
	request, err := http.NewRequest(http.MethodOptions, server.http.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Origin", "https://example.com")
	request.Header.Set("Access-Control-Request-Method", "POST")
	request.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	response, err := server.http.Client().Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if response.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("allow origin = %q", response.Header.Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(response.Header.Get("Access-Control-Allow-Headers"), "authorization") {
		t.Fatalf("allow headers = %q", response.Header.Get("Access-Control-Allow-Headers"))
	}
}

func TestModels(t *testing.T) {
	server := newServer(t, nil)
	response, body := server.get(t, "/v1/models", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	if payload["object"] != "list" {
		t.Fatalf("payload = %v", payload)
	}
	if len(payload["data"].([]any)) == 0 {
		t.Fatal("no models")
	}

	response, body = server.get(t, "/v1/models", map[string]string{"x-api-key": "anything"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("anthropic status = %d: %s", response.StatusCode, body)
	}
	payload = decodeMap(t, body)
	if _, ok := payload["has_more"]; !ok {
		t.Fatalf("expected anthropic model list: %v", payload)
	}
}

func TestDebugEndpoints(t *testing.T) {
	server := newServer(t, nil)
	server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hi [[fakeagents:billing]]"}},
	}, nil)
	response, body := server.get(t, "/_fakeagents/scenarios", nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "billing") {
		t.Fatalf("scenarios: %d %s", response.StatusCode, body)
	}
	response, body = server.get(t, "/_fakeagents/requests?limit=10", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("requests: %d %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	if len(payload["data"].([]any)) == 0 {
		t.Fatal("request recorder is empty")
	}
	response, body = server.get(t, "/_fakeagents/config", nil)
	if response.StatusCode != http.StatusOK || strings.Contains(string(body), "integration-test-key") {
		t.Fatalf("config endpoint leaked a key: %s", body)
	}
	response, body = server.post(t, "/_fakeagents/reset", map[string]any{}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reset: %d %s", response.StatusCode, body)
	}
}

func TestTurnHeaderOverride(t *testing.T) {
	server := newServer(t, nil)
	response, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "pinned turn"}},
	}, map[string]string{
		"x-fakeagents-session": "pinned",
		"X-FakeAgents-Turn":    "42",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if response.Header.Get("X-FakeAgents-Turn") != "42" {
		t.Fatalf("turn header = %q", response.Header.Get("X-FakeAgents-Turn"))
	}
	payload := decodeMap(t, body)
	message := payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if !strings.Contains(fmt.Sprint(message["content"]), "turn 42") {
		t.Fatalf("content = %q", message["content"])
	}
}

func TestOpenCodeSessionHeaderDrivesTurns(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		cfg.RequiredHeaders = []config.HeaderRule{{
			Name:      "x-opencode-session",
			Endpoints: []string{"chat.completions"},
		}}
	})
	headers := map[string]string{"x-opencode-session": "same-session"}
	for expectedTurn := range 2 {
		response, body := server.post(t, "/v1/chat/completions", map[string]any{
			"model":    "fake-gpt-4o",
			"messages": []map[string]any{{"role": "user", "content": "hello"}},
		}, headers)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %s", response.StatusCode, body)
		}
		payload := decodeMap(t, body)
		message := payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		want := fmt.Sprintf("turn %d", expectedTurn)
		if !strings.Contains(fmt.Sprint(message["content"]), want) {
			t.Fatalf("expected %q in %q", want, message["content"])
		}
	}
}

func TestPromptCacheAccounting(t *testing.T) {
	server := newServer(t, nil)
	request := map[string]any{
		"model":            "fake-gpt-4o",
		"prompt_cache_key": "cache-key",
		"messages":         []map[string]any{{"role": "user", "content": "cache me"}},
	}
	_, body := server.post(t, "/v1/chat/completions", request, nil)
	first := decodeMap(t, body)
	firstDetails := first["usage"].(map[string]any)["prompt_tokens_details"].(map[string]any)
	if firstDetails["cached_tokens"].(float64) != 0 {
		t.Fatalf("first turn should not read cache: %v", firstDetails)
	}

	_, body = server.post(t, "/v1/chat/completions", request, nil)
	second := decodeMap(t, body)
	secondDetails := second["usage"].(map[string]any)["prompt_tokens_details"].(map[string]any)
	if secondDetails["cached_tokens"].(float64) <= 0 {
		t.Fatalf("second turn should read cache: %v", secondDetails)
	}
}

func TestScenarioMatcherOnHeader(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		cfg.Scenarios = append(cfg.Scenarios, scenario.Spec{
			Name:     "acme-overloaded",
			Priority: 100,
			Match: scenario.Match{
				Providers: []string{"openai"},
				Headers:   map[string]string{"x-tenant": "acme"},
			},
			Respond: scenario.Respond{
				Type:  scenario.TypeError,
				Error: &scenario.ErrorSpec{Preset: "overloaded"},
			},
		})
	})
	response, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, map[string]string{"x-tenant": "acme"})
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	response, body = server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, map[string]string{"x-tenant": "other"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("non-matching tenant status = %d: %s", response.StatusCode, body)
	}
}

func TestAnthropicRequiredHeaderEnvelope(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		cfg.RequiredHeaders = []config.HeaderRule{{
			Name:      "x-tenant",
			Providers: []string{"anthropic"},
		}}
	})
	response, body := server.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 64,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}, map[string]string{"anthropic-version": "2023-06-01"})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	if payload["type"] != "error" {
		t.Fatalf("payload = %v", payload)
	}
	detail := payload["error"].(map[string]any)
	if detail["type"] != "invalid_request_error" {
		t.Fatalf("detail = %v", detail)
	}
}

func TestAnthropicRateLimitHeaders(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		cfg.RateLimit.Enabled = true
		cfg.RateLimit.RequestsPerMinute = 5
		cfg.RateLimit.TokensPerMinute = 0
	})
	response, body := server.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 64,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}, map[string]string{"anthropic-version": "2023-06-01"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if response.Header.Get("anthropic-ratelimit-requests-limit") != "5" {
		t.Fatalf("limit header = %q", response.Header.Get("anthropic-ratelimit-requests-limit"))
	}
}

func TestChatValidationErrors(t *testing.T) {
	server := newServer(t, nil)
	response, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"n":        2,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("n>1 status = %d: %s", response.StatusCode, body)
	}
	if errorCode(t, decodeMap(t, body)) != "unsupported_parameter" {
		t.Fatalf("body = %s", body)
	}

	response, body = server.post(t, "/v1/chat/completions", map[string]any{
		"model":                 "fake-gpt-4o",
		"max_tokens":            10,
		"max_completion_tokens": 10,
		"messages":              []map[string]any{{"role": "user", "content": "hi"}},
	}, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("dual token limit status = %d: %s", response.StatusCode, body)
	}
}

func TestSendDoneDisabled(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		disabled := false
		cfg.Defaults.Stream.SendDone = &disabled
	})
	response := server.postStream(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "no done"}},
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	events := readSSE(t, response)
	for _, event := range events {
		if event.Data == "[DONE]" {
			t.Fatal("[DONE] must be suppressed")
		}
	}
}

func TestRawScenario(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		cfg.Scenarios = append(cfg.Scenarios, scenario.Spec{
			Name:  "raw",
			Match: scenario.Match{Triggers: []string{"raw"}},
			Respond: scenario.Respond{
				Type:    scenario.TypeRaw,
				Status:  http.StatusCreated,
				RawJSON: `{"custom":true,"choices":null}`,
			},
		})
	})
	response, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "[[fakeagents:raw]]"}},
	}, nil)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if strings.TrimSpace(string(body)) != `{"custom":true,"choices":null}` {
		t.Fatalf("body = %s", body)
	}

	response, body = server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "[[fakeagents:raw]]"}},
	}, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("stream status = %d: %s", response.StatusCode, body)
	}
	if errorCode(t, decodeMap(t, body)) != "unsupported_parameter" {
		t.Fatalf("body = %s", body)
	}
}

func TestDebugRecordsToolCatalog(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		enabled := true
		cfg.Debug.RecordTools = &enabled
	})
	server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "tool catalog"}},
		"tools":    weatherTools(),
	}, nil)
	response, body := server.get(t, "/_fakeagents/requests?limit=1", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	payload := decodeMap(t, body)
	data := payload["data"].([]any)
	if len(data) == 0 {
		t.Fatal("no request recorded")
	}
	tools, _ := data[0].(map[string]any)["tools"].([]any)
	if len(tools) == 0 {
		t.Fatalf("tool catalog not recorded: %v", data[0])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" || tool["schema"] == nil {
		t.Fatalf("tool = %v", tool)
	}
}
