package httpapi

import (
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/httpx"
)

func (s *Server) openRouterModels(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	entries := s.engine.Models(apierr.ProviderOpenAI)
	limit := len(entries)
	if query.Has("offset") {
		limit = 500
	}
	offset := 0
	if query.Has("offset") {
		value, err := strconv.Atoi(query.Get("offset"))
		if err != nil || value < 0 {
			openRouterModelError(w, http.StatusBadRequest, "offset must be a non-negative integer.")
			return
		}
		offset = value
	}
	if query.Has("limit") {
		value, err := strconv.Atoi(query.Get("limit"))
		if err != nil || value < 1 || value > 1000 {
			openRouterModelError(w, http.StatusBadRequest, "limit must be an integer between 1 and 1000.")
			return
		}
		limit = value
	}
	parameters := strings.Split(query.Get("supported_parameters"), ",")
	modalities := strings.Split(query.Get("output_modalities"), ",")
	data := make([]map[string]any, 0, len(entries))
	if query.Get("output_modalities") == "" || slices.Contains(modalities, "all") || slices.Contains(modalities, "text") {
		for _, entry := range entries {
			supported := openRouterParameters(entry)
			if slices.ContainsFunc(parameters, func(param string) bool {
				param = strings.TrimSpace(param)
				return param != "" && !slices.Contains(supported, param)
			}) {
				continue
			}
			data = append(data, openRouterModel(entry, supported))
		}
	}
	total := len(data)
	offset = min(offset, total)
	end := offset + min(limit, total-offset)
	var next any
	if end < total {
		query.Set("offset", strconv.Itoa(end))
		query.Set("limit", strconv.Itoa(limit))
		next = strings.TrimSuffix(s.cfg.Server.BasePath, "/") + "/api/v1/models?" + query.Encode()
	}
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"data": data[offset:end], "total_count": total, "links": map[string]any{"next": next},
	})
}

func (s *Server) openRouterModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, entry := range s.engine.Models(apierr.ProviderOpenAI) {
		if entry.ID == id {
			_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": openRouterModel(entry, openRouterParameters(entry))})
			return
		}
	}
	openRouterModelError(w, http.StatusNotFound, "Model not found.")
}

func openRouterParameters(model config.Model) []string {
	params := []string{"max_tokens", "stop"}
	if model.HasCapability("tools") {
		params = append(params, "tools", "tool_choice")
	}
	if model.HasCapability("structured_output") {
		params = append(params, "response_format", "structured_outputs")
	}
	if model.HasCapability("reasoning") {
		params = append(params, "reasoning", "reasoning_effort")
	}
	return params
}

func openRouterModel(model config.Model, params []string) map[string]any {
	input := catalogInputModalities(model)
	name := model.DisplayName
	if name == "" {
		name = model.ID
	}
	pricing := map[string]string{"prompt": "0", "completion": "0"}
	maps.Copy(pricing, model.Pricing)
	return map[string]any{
		"id": model.ID, "canonical_slug": model.ID, "name": name, "created": model.Created,
		"description": "Simulated model served by FakeAgents.", "context_length": model.ContextWindow,
		"architecture": map[string]any{
			"modality": strings.Join(input, "+") + "->text", "input_modalities": input, "output_modalities": []string{"text"},
			"tokenizer": nil, "instruct_type": nil,
		},
		"top_provider": map[string]any{
			"context_length": model.ContextWindow, "max_completion_tokens": model.MaxOutputTokens, "is_moderated": false,
		},
		"pricing": pricing, "supported_parameters": params, "per_request_limits": nil, "default_parameters": nil,
		"endpoints": []string{"/api/v1/chat/completions"},
	}
}

func openRouterModelError(w http.ResponseWriter, status int, message string) {
	_ = httpx.WriteJSON(w, status, map[string]any{"error": map[string]any{"code": status, "message": message}})
}
