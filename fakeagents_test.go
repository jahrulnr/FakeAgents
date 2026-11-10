package fakeagents_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jahrulnr/fakeagents"
)

func testConfig() *fakeagents.Config {
	cfg := fakeagents.DefaultConfig()
	disabled := false
	cfg.Defaults.Latency.Enabled = &disabled
	cfg.Logging.AccessLog = &disabled
	cfg.Logging.Output = "discard"
	return cfg
}

func requestJSON(t *testing.T, client *http.Client, method, url string, payload any, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return res, result
}

func TestEmbeddedServerUsesIndependentConfigAndState(t *testing.T) {
	cfg := testConfig()
	cfg.Scenarios = append(cfg.Scenarios, fakeagents.Scenario{
		Name:    "unit-fixture",
		Respond: fakeagents.ScenarioResponse{Text: "configured response"},
	})
	first, err := fakeagents.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fakeagents.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close(); _ = second.Close() })
	one, two := httptest.NewServer(first), httptest.NewServer(second)
	t.Cleanup(one.Close)
	t.Cleanup(two.Close)

	cfg.Scenarios[len(cfg.Scenarios)-1].Respond.Text = "mutated response"
	headers := map[string]string{"X-FakeAgents-Scenario": "unit-fixture"}
	res, result := requestJSON(t, one.Client(), "POST", one.URL+"/v1/chat/completions", map[string]any{
		"model": "fake-gpt-4o", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, headers)
	if res.StatusCode != 200 {
		t.Fatalf("chat status = %d: %v", res.StatusCode, result)
	}
	message := result["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "configured response" {
		t.Fatalf("configuration was changed after creation: %v", message)
	}

	res, result = requestJSON(t, one.Client(), "POST", one.URL+"/v1/responses", map[string]any{
		"model": "fake-gpt-4o", "input": "hello", "store": true,
	}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("responses status = %d: %v", res.StatusCode, result)
	}
	id := result["id"].(string)
	res, _ = requestJSON(t, one.Client(), "GET", one.URL+"/v1/responses/"+id, nil, nil)
	if res.StatusCode != 200 {
		t.Fatalf("stored response status = %d", res.StatusCode)
	}
	res, _ = requestJSON(t, two.Client(), "GET", two.URL+"/v1/responses/"+id, nil, nil)
	if res.StatusCode != 404 {
		t.Fatalf("response leaked to another instance: %d", res.StatusCode)
	}
}

func TestEmbeddedScenarioErrorAndConcurrentClose(t *testing.T) {
	cfg := testConfig()
	cfg.Scenarios = append(cfg.Scenarios, fakeagents.Scenario{
		Name:    "offline",
		Respond: fakeagents.ScenarioResponse{Type: "error", Error: &fakeagents.ErrorSpec{Preset: "overloaded"}},
	})
	fake, err := fakeagents.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(fake)
	res, result := requestJSON(t, server.Client(), "POST", server.URL+"/v1/chat/completions", map[string]any{
		"model": "fake-gpt-4o", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"X-FakeAgents-Scenario": "offline"})
	if res.StatusCode != 503 || result["error"].(map[string]any)["type"] == "" {
		t.Fatalf("scenario error = %d %v", res.StatusCode, result)
	}
	server.Close()
	var group sync.WaitGroup
	for range 10 {
		group.Go(func() {
			if err := fake.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
}

func TestLoadConfigResolvesRelativeFixtures(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "template.txt"), []byte("loaded {{ .UserMessage }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "speech.mp3"), []byte("audio fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := `defaults:
  latency:
    enabled: false
templates:
  local: "@file:template.txt"
media:
  speech_files:
    mp3: speech.mp3
scenarios:
  - name: local-fixture
    respond:
      text_template: local
`
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := fakeagents.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	fake, err := fakeagents.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fake.Close() })
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	res, result := requestJSON(t, server.Client(), "POST", server.URL+"/v1/chat/completions", map[string]any{
		"model": "fake-gpt-4o", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"X-FakeAgents-Scenario": "local-fixture"})
	if res.StatusCode != 200 || !strings.Contains(result["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string), "loaded hello") {
		t.Fatalf("relative template = %d %v", res.StatusCode, result)
	}
	speech, err := server.Client().Post(server.URL+"/v1/audio/speech", "application/json",
		strings.NewReader(`{"model":"tts-1","input":"hello","voice":"alloy"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer speech.Body.Close()
	audio, err := io.ReadAll(speech.Body)
	if err != nil || speech.StatusCode != 200 || string(audio) != "audio fixture" {
		t.Fatalf("relative audio fixture = %d %q %v", speech.StatusCode, audio, err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Server.Port = -1
	if _, err := fakeagents.New(cfg); err == nil {
		t.Fatal("expected invalid config error")
	}
}

func TestDefaultServerCanServeInProcess(t *testing.T) {
	fake, err := fakeagents.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fake.Close() })
	response := httptest.NewRecorder()
	fake.ServeHTTP(response, httptest.NewRequest("GET", "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health = %d: %s", response.Code, response.Body)
	}
}
