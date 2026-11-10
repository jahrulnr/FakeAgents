package openai

import (
	"net/http"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/httpx"
)

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// Models handles GET /v1/models.
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	entries := h.engine.Models(apierr.ProviderOpenAI)
	data := make([]modelObject, 0, len(entries))
	for _, entry := range entries {
		data = append(data, toModelObject(entry))
	}
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

// Model handles GET /v1/models/{id}.
func (h *Handler) Model(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, entry := range h.engine.Models(apierr.ProviderOpenAI) {
		if strings.EqualFold(entry.ID, id) {
			_ = httpx.WriteJSON(w, http.StatusOK, toModelObject(entry))
			return
		}
		for _, alias := range entry.Aliases {
			if strings.EqualFold(alias, id) {
				_ = httpx.WriteJSON(w, http.StatusOK, toModelObject(entry))
				return
			}
		}
	}
	apiErr := apierr.MustPreset(apierr.ProviderOpenAI, "model_not_found")
	apiErr.Message = "The model '" + id + "' does not exist or you do not have access to it."
	h.fail(w, r, h.options(r), apiErr)
}

func toModelObject(entry config.Model) modelObject {
	owned := entry.OwnedBy
	if owned == "" {
		owned = "fakeagents"
	}
	created := entry.Created
	if created == 0 {
		created = 1700000000
	}
	return modelObject{ID: entry.ID, Object: "model", Created: created, OwnedBy: owned}
}
