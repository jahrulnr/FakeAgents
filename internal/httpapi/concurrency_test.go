package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/duration"
	"github.com/jahrulnr/fakeagents/internal/scenario"
)

func TestConcurrentRoomsAndActiveRequests(t *testing.T) {
	enabled := true
	server := newServer(t, func(cfg *config.Config) {
		cfg.Defaults.Latency.Enabled = &enabled
		cfg.Scenarios = append(cfg.Scenarios, scenario.Spec{
			Name:  "slow-room",
			Match: scenario.Match{Triggers: []string{"slow-room"}},
			Respond: scenario.Respond{
				Text: "room {{ .Session }} ready",
				Latency: &scenario.LatencyOverride{
					TimeToFirstToken: &duration.Range{Min: duration.Duration(500 * time.Millisecond), Max: duration.Duration(500 * time.Millisecond)},
					TokensPerSecond:  &duration.NumberRange{Min: 10000, Max: 10000},
				},
			},
		})
	})

	type result struct {
		session string
		status  int
		body    []byte
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, session := range []string{"room-a", "room-b"} {
		go func(session string) {
			<-start
			payload, _ := json.Marshal(map[string]any{
				"model":    "fake-gpt-4o",
				"messages": []map[string]any{{"role": "user", "content": "hold the room"}},
			})
			request, err := http.NewRequest(http.MethodPost, server.http.URL+"/v1/chat/completions", bytes.NewReader(payload))
			if err != nil {
				results <- result{session: session, err: err}
				return
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("x-fakeagents-session", session)
			request.Header.Set("X-FakeAgents-Scenario", "slow-room")
			response, err := server.http.Client().Do(request)
			if err != nil {
				results <- result{session: session, err: err}
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			results <- result{session: session, status: response.StatusCode, body: body, err: err}
		}(session)
	}
	close(start)

	activeCount := 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, body := server.get(t, "/_fakeagents/active", nil)
		if response.StatusCode == http.StatusOK {
			activeCount = int(decodeMap(t, body)["count"].(float64))
			if activeCount == 2 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if activeCount != 2 {
		t.Fatalf("active requests = %d, want 2", activeCount)
	}

	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("%s: %v", result.session, result.err)
			}
			if result.status != http.StatusOK {
				t.Fatalf("%s status = %d: %s", result.session, result.status, result.body)
			}
			payload := decodeMap(t, result.body)
			content := payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
			if !bytes.Contains([]byte(content), []byte(result.session)) {
				t.Fatalf("%s response = %q", result.session, content)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for room response")
		}
	}
}
