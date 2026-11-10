package httpapi_test

import (
	"math"
	"net/http"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/scenario"
)

func accountingServer(t *testing.T) *testServer {
	t.Helper()
	return newServer(t, func(cfg *config.Config) {
		cfg.Models = append(cfg.Models, config.Model{
			ID: "fixture-model", Provider: "openai",
			Pricing: map[string]string{"prompt": "0.000001", "completion": "0.000004", "input_cache_read": "0.0000001", "input_cache_write": "0.000002"},
		})
		input, output, read, write := 100, 20, 10, 5
		cfg.Scenarios = append(cfg.Scenarios, scenario.Spec{
			Name: "accounting", Respond: scenario.Respond{
				Text:  "done",
				Usage: &scenario.UsageOverride{Mode: "fixed", InputTokens: &input, OutputTokens: &output, CachedTokens: &read, CacheWriteTokens: &write},
			},
		})
	})
}

func TestOpenRouterChatFinalUsageBeforeDone(t *testing.T) {
	server := accountingServer(t)
	response := server.postStream(t, "/api/v1/chat/completions", map[string]any{
		"model": "fixture-model", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"X-FakeAgents-Scenario": "accounting"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	events := readSSE(t, response)
	if len(events) < 4 || events[len(events)-1].Data != "[DONE]" {
		t.Fatalf("missing sentinel: %v", events)
	}
	before := decodeMap(t, []byte(events[len(events)-3].Data))
	final := decodeMap(t, []byte(events[len(events)-2].Data))
	previousChoice := before["choices"].([]any)[0].(map[string]any)
	finalChoice := final["choices"].([]any)[0].(map[string]any)
	if previousChoice["finish_reason"] != "stop" || finalChoice["finish_reason"] != "stop" || finalChoice["native_finish_reason"] != "stop" {
		t.Fatalf("finish chunks = %v %v", before, final)
	}
	usage := final["usage"].(map[string]any)
	details := usage["prompt_tokens_details"].(map[string]any)
	if usage["prompt_tokens"] != float64(115) || details["cached_tokens"] != float64(10) || details["cache_write_tokens"] != float64(5) {
		t.Fatalf("usage = %v", usage)
	}
	if math.Abs(usage["cost"].(float64)-0.000191) > 1e-12 {
		t.Fatalf("cost = %v", usage["cost"])
	}
}

func TestOpenRouterResponsesTerminalAndSentinel(t *testing.T) {
	server := accountingServer(t)
	for _, test := range []struct {
		name, path, scenario, terminal string
		done                           bool
	}{
		{"openrouter success", "/api/v1/responses", "accounting", "response.completed", true},
		{"openrouter truncated", "/api/v1/responses", "truncated", "response.incomplete", true},
		{"openai", "/v1/responses", "accounting", "response.completed", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := server.postStream(t, test.path, map[string]any{"model": "fixture-model", "input": "hello", "stream": true}, map[string]string{"X-FakeAgents-Scenario": test.scenario})
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", response.StatusCode)
			}
			events := readSSE(t, response)
			terminalIndex := len(events) - 1
			if test.done {
				if events[terminalIndex].Data != "[DONE]" {
					t.Fatalf("missing DONE: %v", events)
				}
				terminalIndex--
			}
			if events[terminalIndex].Name != test.terminal {
				t.Fatalf("last event = %v", events[terminalIndex])
			}
			if test.scenario == "accounting" {
				event := decodeMap(t, []byte(events[terminalIndex].Data))
				usage := event["response"].(map[string]any)["usage"].(map[string]any)
				if usage["input_tokens"] != float64(115) {
					t.Fatalf("usage = %v", usage)
				}
				if test.done && usage["input_tokens_details"].(map[string]any)["cache_write_tokens"] != float64(5) {
					t.Fatalf("cache write missing: %v", usage)
				}
				if test.done && math.Abs(usage["cost"].(float64)-0.000191) > 1e-12 {
					t.Fatalf("cost = %v", usage["cost"])
				}
			}
		})
	}
}

func TestOpenRouterNoDoneAfterAbort(t *testing.T) {
	server := newServer(t, nil)
	response := server.postStream(t, "/api/v1/responses", map[string]any{"model": "fake-gpt-5", "input": "hello", "stream": true}, map[string]string{"X-FakeAgents-Scenario": "mid-stream-abort"})
	events := readSSE(t, response)
	for _, event := range events {
		if event.Data == "[DONE]" || event.Name == "response.completed" || event.Name == "response.incomplete" {
			t.Fatalf("unexpected terminal after abort: %v", events)
		}
	}
}

func TestOpenRouterChatAbortAfterFinishBeforeUsage(t *testing.T) {
	server := newServer(t, func(cfg *config.Config) {
		cfg.Scenarios = append(cfg.Scenarios, scenario.Spec{
			Name: "abort-after-finish",
			Respond: scenario.Respond{
				Text:   "done",
				Stream: &scenario.StreamOverride{AbortAfterChunks: 3},
			},
		})
	})
	response := server.postStream(t, "/api/v1/chat/completions", map[string]any{
		"model": "fake-gpt-4o", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"X-FakeAgents-Scenario": "abort-after-finish"})
	events := readSSE(t, response)
	if len(events) != 3 {
		t.Fatalf("events = %v", events)
	}
	last := decodeMap(t, []byte(events[2].Data))
	if last["choices"].([]any)[0].(map[string]any)["finish_reason"] != "stop" || last["usage"] != nil {
		t.Fatalf("expected finish without usage or DONE: %v", last)
	}
}
