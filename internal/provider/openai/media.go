package openai

import (
	"errors"
	"net/http"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/provider/shared"
	"github.com/jahrulnr/fakeagents/internal/ratelimit"
	"github.com/jahrulnr/fakeagents/internal/sse"
	"github.com/jahrulnr/fakeagents/internal/wire"
)

func (h *Handler) mediaEvent(r *http.Request, writer *sse.Writer, plan *engine.Plan, name string, payload any) bool {
	if r.Context().Err() != nil || plan.Stream.AbortAfterChunks > 0 && writer.Count() >= plan.Stream.AbortAfterChunks {
		return false
	}
	if writer.Event(name, payload) != nil {
		return false
	}
	if plan.Stream.Error != nil && plan.Stream.ErrorAfterChunks > 0 && writer.Count() >= plan.Stream.ErrorAfterChunks {
		_ = writer.Event("error", map[string]any{"type": "error", "error": map[string]any{
			"type": plan.Stream.Error.Type, "code": plan.Stream.Error.Code, "message": plan.Stream.Error.Message,
		}})
		return false
	}
	return h.clock.Sleep(r.Context(), plan.Latency.Chunk) == nil
}

func (h *Handler) multipartError(w http.ResponseWriter, r *http.Request, field string, err error) {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		h.fail(w, r, h.options(r), apierr.MustPreset(apierr.ProviderOpenAI, "request_too_large"))
		return
	}
	h.mediaError(w, r, field, "Invalid multipart request.")
}

func (h *Handler) mediaPlan(w http.ResponseWriter, r *http.Request, endpoint, model, prompt string, stream bool, raw map[string]any) *engine.Plan {
	opts := h.options(r)
	resolved, apiErr := h.engine.ResolveModel(apierr.ProviderOpenAI, model)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return nil
	}
	sessionKey, turn, turnSet := shared.Session(h.cfg, r, h.sessions, "", "", "")
	req := &engine.Request{
		Provider: apierr.ProviderOpenAI, Endpoint: endpoint, Model: resolved, Stream: stream,
		Messages: []engine.Message{{Role: engine.RoleUser, Content: prompt}},
		Raw:      raw, Headers: r.Header, RequestID: shared.RequestID(r),
		SessionKey: sessionKey, Turn: turn, TurnSet: turnSet,
	}
	decision := h.limiter.AdmitTokens(sessionKey, h.engine.CountInputTokens(req))
	if !decision.Allowed {
		apiErr := apierr.MustPreset(apierr.ProviderOpenAI, "rate_limit")
		apiErr.RetryAfter = decision.RetryAfter
		h.fail(w, r, opts, apiErr)
		return nil
	}
	if h.cfg.RateLimit.HeadersEnabled() {
		wire.SetRateLimitHeaders(w, apierr.ProviderOpenAI, ratelimit.DecisionFrom(r.Context()), decision)
	}
	plan, err := h.engine.Generate(r.Context(), req)
	if err != nil {
		h.failGenerate(w, r, opts, err)
		return nil
	}
	shared.Record(r, apierr.ProviderOpenAI, endpoint, plan.Model, plan.Meta.Scenario, sessionKey, plan.Meta.Turn, stream)
	shared.RecordUsage(r, plan.Usage.InputTokens+plan.Usage.CachedTokens+plan.Usage.CacheWriteTokens, plan.Usage.OutputTokens)
	h.prepareHeaders(w, plan)
	return plan
}

func (h *Handler) mediaError(w http.ResponseWriter, r *http.Request, field, message string) {
	h.fail(w, r, h.options(r), apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter", message).WithParam(field))
}

func (h *Handler) fixtureError(w http.ResponseWriter, r *http.Request, message string) {
	h.fail(w, r, h.options(r), apierr.New(http.StatusInternalServerError, "server_error", "fixture_error", message))
}
