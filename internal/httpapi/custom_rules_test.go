package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/config"
)

func customRulesServer(t *testing.T) *testServer {
	t.Helper()
	cfg, err := config.Load("../../configs/custom-rules.json")
	if err != nil {
		t.Fatal(err)
	}
	return newServer(t, func(target *config.Config) {
		target.Scenarios = cfg.Scenarios
	})
}

func TestCustomImageLimit(t *testing.T) {
	server := customRulesServer(t)
	for _, test := range []struct {
		name, model string
		images      int
		want        int
	}{
		{"boundary below", "glm-5.3-flash", 31, http.StatusOK},
		{"boundary", "glm-5.3-flash", 32, http.StatusOK},
		{"exceed", "glm-5.3-flash", 33, http.StatusBadRequest},
		{"other model", "another-model", 33, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := []any{map[string]any{"type": "text", "text": "describe"}}
			for range test.images {
				content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}})
			}
			response, body := server.post(t, "/v1/chat/completions", map[string]any{
				"model": test.model, "messages": []any{map[string]any{"role": "user", "content": content}},
			}, nil)
			if response.StatusCode != test.want {
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, test.want, body)
			}
		})
	}
	content := []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}}}
	first := make([]any, 16)
	second := make([]any, 17)
	for i := range first {
		first[i] = content[0]
	}
	for i := range second {
		second[i] = content[0]
	}
	response, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":  "glm-5.3-flash",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": first},
			map[string]any{"role": "user", "content": second},
		},
	}, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("images across messages: status = %d: %s", response.StatusCode, body)
	}
}

func TestCustomMissingToolReasoning(t *testing.T) {
	server := customRulesServer(t)
	for _, test := range []struct {
		name, model string
		reasoning   any
		present     bool
		want        int
	}{
		{"omitted", "deepseek-v4-pro", nil, false, 400},
		{"null", "deepseek-v4-pro", nil, true, 400},
		{"empty", "deepseek-v4-pro", "", true, 400},
		{"noise workaround", "deepseek-v4-pro", ".", true, 200},
		{"normal", "deepseek-v4-pro", "thinking", true, 200},
		{"other model", "fake-gpt-4o", "", true, 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			assistant := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "test", "arguments": "{}"}},
			}}
			if test.present {
				assistant["reasoning_content"] = test.reasoning
			}
			body := map[string]any{
				"model": test.model,
				"messages": []any{
					map[string]any{"role": "user", "content": "run"},
					assistant,
					map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
				},
				"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "test", "parameters": map[string]any{"type": "object"}}}},
			}
			response, data := server.post(t, "/v1/chat/completions", body, nil)
			if response.StatusCode != test.want {
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, test.want, data)
			}
			if test.want == 400 && !strings.Contains(string(data), "missing_reasoning_content") {
				t.Fatalf("wrong error: %s", data)
			}
		})
	}
}

func TestCustomRulesKeepBuiltinScenarios(t *testing.T) {
	server := customRulesServer(t)
	res, body := server.post(t, "/v1/chat/completions", map[string]any{
		"model":    "fake-gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "[[fakeagents:thinking]]"}},
	}, nil)
	if res.StatusCode != 200 || !strings.Contains(string(body), "reasoning_content") {
		t.Fatalf("builtin thinking unavailable: %d %s", res.StatusCode, body)
	}
}
