package httpapi

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/httpx"
)

// modelsDev serves the Models.dev registry shape consumed by OpenCode.
func (s *Server) modelsDev(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	api := url.URL{Scheme: scheme, Host: r.Host, Path: strings.TrimSuffix(s.cfg.Server.BasePath, "/") + "/v1"}
	models := map[string]any{}
	for _, entry := range s.engine.Models(apierr.ProviderOpenAI) {
		name := entry.DisplayName
		if name == "" {
			name = entry.ID
		}
		release := ""
		if entry.Created != 0 {
			release = time.Unix(entry.Created, 0).UTC().Format(time.DateOnly)
		}
		cost := map[string]float64{
			"input": perMillion(entry.Pricing["prompt"]), "output": perMillion(entry.Pricing["completion"]),
		}
		for upstream, field := range map[string]string{"input_cache_read": "cache_read", "input_cache_write": "cache_write"} {
			if price, ok := entry.Pricing[upstream]; ok {
				cost[field] = perMillion(price)
			}
		}
		models[entry.ID] = map[string]any{
			"id": entry.ID, "name": name, "release_date": release, "last_updated": release,
			"attachment": entry.HasCapability("vision"), "reasoning": entry.HasCapability("reasoning"),
			"tool_call": entry.HasCapability("tools"), "structured_output": entry.HasCapability("structured_output"),
			"temperature": false, "open_weights": false, "cost": cost,
			"limit":      map[string]int{"context": entry.ContextWindow, "output": entry.MaxOutputTokens},
			"modalities": map[string][]string{"input": catalogInputModalities(entry), "output": {"text"}},
		}
	}
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{"fakeagents": map[string]any{
		"id": "fakeagents", "name": "FakeAgents", "api": api.String(), "npm": "@ai-sdk/openai-compatible",
		"env": []string{"FAKEAGENTS_API_KEY"}, "models": models,
	}})
}

func catalogInputModalities(entry config.Model) []string {
	if entry.HasCapability("vision") {
		return []string{"text", "image"}
	}
	return []string{"text"}
}

func perMillion(perToken string) float64 {
	// Pricing is validated when the config is loaded.
	value, _ := strconv.ParseFloat(perToken, 64)
	return value * 1e6
}
