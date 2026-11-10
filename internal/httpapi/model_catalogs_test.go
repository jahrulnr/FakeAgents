package httpapi_test

import (
	"maps"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/config"
)

func catalogServer(t *testing.T) *testServer {
	t.Helper()
	fixture, err := config.Load("../../configs/model-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	return newServer(t, func(cfg *config.Config) {
		cfg.Models = fixture.Models
		cfg.Defaults.StrictModels = fixture.Defaults.StrictModels
	})
}

func TestOpenRouterModelCatalog(t *testing.T) {
	server := catalogServer(t)
	res, body := server.get(t, "/api/v1/models?limit=1", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	page := decodeMap(t, body)
	data := page["data"].([]any)
	if len(data) != 1 || page["total_count"] != float64(2) {
		t.Fatalf("unexpected first page: %v", page)
	}
	next := page["links"].(map[string]any)["next"].(string)
	res, body = server.get(t, next, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("next page status = %d: %s", res.StatusCode, body)
	}
	second := decodeMap(t, body)
	if second["data"].([]any)[0].(map[string]any)["id"] != "fake-lab/pro" || second["links"].(map[string]any)["next"] != nil {
		t.Fatalf("unexpected final page: %v", second)
	}

	res, body = server.get(t, "/api/v1/models?supported_parameters=tools,reasoning", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("filtered page status = %d: %s", res.StatusCode, body)
	}
	filtered := decodeMap(t, body)["data"].([]any)
	if len(filtered) != 1 {
		t.Fatalf("unexpected filtered entries: %v", filtered)
	}
	entry := filtered[0].(map[string]any)
	if entry["id"] != "fake-lab/pro" || entry["context_length"] != float64(128000) ||
		entry["pricing"].(map[string]any)["prompt"] != "0.000001" {
		t.Fatalf("unexpected model entry: %v", entry)
	}

	for _, tt := range []struct {
		path   string
		status int
	}{
		{"/api/v1/model/fake-lab/pro", 200},
		{"/api/v1/model/fake-lab/missing", 404},
		{"/api/v1/models?offset=-1", 400},
		{"/api/v1/models?limit=1001", 400},
	} {
		res, body = server.get(t, tt.path, nil)
		if res.StatusCode != tt.status {
			t.Errorf("%s returned %d: %s", tt.path, res.StatusCode, body)
		}
	}
}

func TestAnthropicModelCatalogCursors(t *testing.T) {
	server := catalogServer(t)
	for _, tt := range []struct {
		query, id string
		more      bool
	}{
		{"limit=1", "fake-claude-pro", true},
		{"limit=1&after_id=fake-claude-pro", "fake-claude-fast", false},
		{"limit=1&before_id=fake-claude-fast", "fake-claude-pro", false},
	} {
		res, body := server.get(t, "/v1/models?"+tt.query, anthropicHeaders())
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %s", res.StatusCode, body)
		}
		page := decodeMap(t, body)
		if len(page["data"].([]any)) != 1 || page["first_id"] != tt.id || page["last_id"] != tt.id || page["has_more"] != tt.more {
			t.Errorf("%s: unexpected page %v", tt.query, page)
		}
	}
	res, body := server.get(t, "/v1/models?limit=0", anthropicHeaders())
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d: %s", res.StatusCode, body)
	}
}

func TestModelsDevAndOpenAIExposeDifferentMetadata(t *testing.T) {
	server := catalogServer(t)
	res, body := server.get(t, "/api.json", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	provider := decodeMap(t, body)["fakeagents"].(map[string]any)
	if provider["api"] != server.http.URL+"/v1" {
		t.Fatalf("OpenCode API URL = %v", provider["api"])
	}
	models := provider["models"].(map[string]any)
	pro := models["fake-lab/pro"].(map[string]any)
	cost := pro["cost"].(map[string]any)
	if pro["tool_call"] != true || pro["reasoning"] != true || pro["limit"].(map[string]any)["context"] != float64(128000) ||
		cost["input"] != float64(1) || cost["output"] != float64(4) || math.Abs(cost["cache_read"].(float64)-0.1) > 1e-9 {
		t.Fatalf("unexpected OpenCode model: %v", pro)
	}

	res, body = server.get(t, "/v1/models/"+url.PathEscape("fake-lab/pro"), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	entry := decodeMap(t, body)
	if len(entry) != 4 || entry["id"] != "fake-lab/pro" || entry["object"] != "model" {
		t.Fatalf("unexpected OpenAI model: %v", entry)
	}
}

func TestCatalogModelLimitsApplyToChat(t *testing.T) {
	server := catalogServer(t)
	path := "/api/v1/chat/completions"
	for _, tt := range []struct {
		input string
		extra map[string]any
		code  string
	}{
		{"hello", nil, ""},
		{strings.Repeat("abcd ", 100), nil, "context_length_exceeded"},
		{"hello", map[string]any{"max_tokens": 33}, "invalid_parameter"},
		{"hello", map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}}}, "unsupported_parameter"},
	} {
		req := map[string]any{"model": "fake-lab/text", "messages": []any{map[string]any{"role": "user", "content": tt.input}}}
		maps.Copy(req, tt.extra)
		res, body := server.post(t, path, req, nil)
		if tt.code == "" {
			if res.StatusCode != http.StatusOK {
				t.Errorf("positive request status = %d: %s", res.StatusCode, body)
			}
			continue
		}
		if res.StatusCode != http.StatusBadRequest || errorCode(t, decodeMap(t, body)) != tt.code {
			t.Errorf("expected %s, got %d: %s", tt.code, res.StatusCode, body)
		}
	}
}
