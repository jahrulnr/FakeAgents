package anthropic

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/httpx"
)

type modelEntry struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

// Models handles GET /v1/models for Anthropic clients.
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	entries := h.engine.Models(apierr.ProviderAnthropic)
	slices.SortStableFunc(entries, func(a, b config.Model) int {
		return cmp.Compare(modelCreated(b.Created), modelCreated(a.Created))
	})
	query := r.URL.Query()
	limit := 20
	fail := func(param, message string) {
		apiErr := apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter", message).WithParam(param)
		h.fail(w, r, h.options(), apiErr)
	}
	if query.Has("limit") {
		var err error
		limit, err = strconv.Atoi(query.Get("limit"))
		if err != nil || limit < 1 || limit > 1000 {
			fail("limit", "limit must be an integer between 1 and 1000.")
			return
		}
	}
	after, before := query.Get("after_id"), query.Get("before_id")
	if after != "" && before != "" {
		fail("before_id", "before_id and after_id cannot be used together.")
		return
	}
	start, end := 0, len(entries)
	if after != "" || before != "" {
		param, cursor := "after_id", after
		if before != "" {
			param, cursor = "before_id", before
		}
		index := slices.IndexFunc(entries, func(entry config.Model) bool { return entry.ID == cursor })
		if index < 0 {
			fail(param, "The model cursor was not found.")
			return
		}
		if before != "" {
			end = index
		} else {
			start = index + 1
		}
	}
	hasMore := end-start > limit
	if before != "" {
		start = max(start, end-limit)
	} else {
		end = min(end, start+limit)
	}
	entries = entries[start:end]
	data := make([]modelEntry, 0, len(entries))
	for _, entry := range entries {
		data = append(data, toModelEntry(entry.ID, entry.DisplayName, entry.Created))
	}
	response := map[string]any{
		"data":     data,
		"has_more": hasMore,
		"first_id": nil,
		"last_id":  nil,
	}
	if len(data) > 0 {
		response["first_id"] = data[0].ID
		response["last_id"] = data[len(data)-1].ID
	}
	_ = httpx.WriteJSON(w, http.StatusOK, response)
}

// Model handles GET /v1/models/{id} for Anthropic clients.
func (h *Handler) Model(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, entry := range h.engine.Models(apierr.ProviderAnthropic) {
		if strings.EqualFold(entry.ID, id) {
			_ = httpx.WriteJSON(w, http.StatusOK, toModelEntry(entry.ID, entry.DisplayName, entry.Created))
			return
		}
		for _, alias := range entry.Aliases {
			if strings.EqualFold(alias, id) {
				_ = httpx.WriteJSON(w, http.StatusOK, toModelEntry(entry.ID, entry.DisplayName, entry.Created))
				return
			}
		}
	}
	apiErr := apierr.MustPreset(apierr.ProviderAnthropic, "model_not_found")
	apiErr.Message = "model: The requested model '" + id + "' was not found."
	h.fail(w, r, h.options(), apiErr)
}

func toModelEntry(id, displayName string, created int64) modelEntry {
	if displayName == "" {
		displayName = id
	}
	return modelEntry{
		ID:          id,
		Type:        "model",
		DisplayName: displayName,
		CreatedAt:   time.Unix(modelCreated(created), 0).UTC().Format(time.RFC3339),
	}
}

func modelCreated(created int64) int64 {
	if created == 0 {
		return 1700000000
	}
	return created
}
