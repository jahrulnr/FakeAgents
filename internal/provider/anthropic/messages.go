// Package anthropic implements the Anthropic Messages API.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/chunk"
	"github.com/jahrulnr/fakeagents/internal/clock"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/envelope"
	"github.com/jahrulnr/fakeagents/internal/httpx"
	"github.com/jahrulnr/fakeagents/internal/id"
	"github.com/jahrulnr/fakeagents/internal/observe"
	"github.com/jahrulnr/fakeagents/internal/provider/shared"
	"github.com/jahrulnr/fakeagents/internal/ratelimit"
	"github.com/jahrulnr/fakeagents/internal/session"
	"github.com/jahrulnr/fakeagents/internal/sse"
	"github.com/jahrulnr/fakeagents/internal/wire"
)

// Deps are the handler dependencies.
type Deps struct {
	Config   *config.Config
	Engine   *engine.Engine
	Sealer   envelope.Sealer
	Sessions session.Tracker
	Limiter  *ratelimit.Limiter
	Clock    clock.Clock
	IDs      id.Generator
	Logger   *slog.Logger
}

// Handler serves Anthropic-compatible endpoints.
type Handler struct {
	cfg      *config.Config
	engine   *engine.Engine
	sealer   envelope.Sealer
	sessions session.Tracker
	limiter  *ratelimit.Limiter
	clock    clock.Clock
	ids      id.Generator
	logger   *slog.Logger
}

// New builds an Anthropic handler.
func New(deps Deps) *Handler {
	clk := deps.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	ids := deps.IDs
	if ids == nil {
		ids = id.New()
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		cfg:      deps.Config,
		engine:   deps.Engine,
		sealer:   deps.Sealer,
		sessions: deps.Sessions,
		limiter:  deps.Limiter,
		clock:    clk,
		ids:      ids,
		logger:   logger,
	}
}

// Messages handles POST /v1/messages.
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	opts := h.options()
	if !h.checkVersion(w, r, opts) {
		return
	}
	raw, apiErr := httpx.ReadBody(w, r, h.cfg.Server.MaxBodyBytes)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	var body MessagesRequest
	if apiErr := httpx.DecodeBytes(raw, &body); apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	rawMap := httpx.DecodeMap(raw)
	cacheHint := bytes.Contains(raw, []byte("cache_control"))

	userID := ""
	if body.Metadata != nil {
		userID = body.Metadata.UserID
	}
	sessionKey, turn, turnSet := shared.Session(h.cfg, r, h.sessions, userID)
	req, apiErr := buildMessagesRequest(h.cfg, h.sealer, &body, rawMap, cacheHint, shared.RequestID(r), sessionKey, turn, turnSet, true)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	req.Headers = r.Header
	req.RawBody = raw
	shared.RecordTools(r, h.cfg.Debug.RecordToolsEnabled(), req.Tools)

	model, apiErr := h.engine.ResolveModel(apierr.ProviderAnthropic, req.Model)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	req.Model = model
	shared.Record(r, apierr.ProviderAnthropic, "messages", model, "", sessionKey, turn, body.Stream)

	requestLimit := ratelimit.DecisionFrom(r.Context())
	tokenLimit := h.limiter.AdmitTokens(sessionKey, h.engine.CountInputTokens(req))
	if !tokenLimit.Allowed {
		wire.SetRateLimitHeaders(w, apierr.ProviderAnthropic, requestLimit, tokenLimit)
		apiErr := apierr.MustPreset(apierr.ProviderAnthropic, "rate_limit")
		apiErr.RetryAfter = tokenLimit.RetryAfter
		h.fail(w, r, opts, apiErr)
		return
	}
	if h.cfg.RateLimit.HeadersEnabled() {
		wire.SetRateLimitHeaders(w, apierr.ProviderAnthropic, requestLimit, tokenLimit)
	}

	plan, err := h.engine.Generate(r.Context(), req)
	if err != nil {
		h.failGenerate(w, r, opts, err)
		return
	}
	shared.Record(r, apierr.ProviderAnthropic, "messages", plan.Model, plan.Meta.Scenario, sessionKey, plan.Meta.Turn, body.Stream)
	shared.RecordStep(r, plan.Meta.Step)
	shared.RecordUsage(r, plan.Usage.InputTokens+plan.Usage.CachedTokens+plan.Usage.CacheWriteTokens, plan.Usage.OutputTokens)

	if plan.RawJSON != nil {
		if err := h.clock.Sleep(r.Context(), plan.Latency.Total); err != nil {
			return
		}
		h.writeRaw(w, plan)
		return
	}
	if body.Stream {
		h.streamMessages(w, r, plan)
		return
	}
	if err := h.clock.Sleep(r.Context(), plan.Latency.Total); err != nil {
		return
	}
	h.prepareHeaders(w, plan)
	if plan.Latency.Total > 0 {
		w.Header().Set("anthropic-processing-ms", formatMillis(plan.Latency.Total))
	}
	if err := httpx.WriteJSON(w, http.StatusOK, buildMessageResponse(plan)); err != nil {
		h.logger.Error("write messages response", "error", err, "request_id", plan.Meta.RequestID)
	}
}

// CountTokens handles POST /v1/messages/count_tokens.
func (h *Handler) CountTokens(w http.ResponseWriter, r *http.Request) {
	opts := h.options()
	if !h.checkVersion(w, r, opts) {
		return
	}
	raw, apiErr := httpx.ReadBody(w, r, h.cfg.Server.MaxBodyBytes)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	var body MessagesRequest
	if apiErr := httpx.DecodeBytes(raw, &body); apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	cacheHint := bytes.Contains(raw, []byte("cache_control"))
	req, apiErr := buildMessagesRequest(h.cfg, h.sealer, &body, httpx.DecodeMap(raw), cacheHint, shared.RequestID(r), "", 0, true, false)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	model, apiErr := h.engine.ResolveModel(apierr.ProviderAnthropic, req.Model)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	req.Model = model
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{"input_tokens": h.engine.CountInputTokens(req)})
}

func (h *Handler) checkVersion(w http.ResponseWriter, r *http.Request, opts wire.Options) bool {
	if !h.cfg.Providers.Anthropic.RequireVersionEnabled() {
		return true
	}
	if strings.TrimSpace(r.Header.Get("anthropic-version")) == "" {
		h.fail(w, r, opts, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
			"anthropic-version: header is required"))
		return false
	}
	return true
}

func (h *Handler) prepareHeaders(w http.ResponseWriter, plan *engine.Plan) {
	wire.SetBaseHeaders(w, wire.Options{
		Provider:   apierr.ProviderAnthropic,
		RequestID:  plan.Meta.RequestID,
		APIVersion: h.cfg.Providers.Anthropic.APIVersion,
	})
	if h.cfg.Debug.HeadersEnabled() {
		wire.SetDebugHeaders(w, plan.Meta.Scenario, plan.Meta.Session, plan.Meta.Turn, plan.Meta.Step, plan.Meta.Seed, plan.Model)
		wire.SetSimulatedLatency(w, plan.Latency.Total, plan.Latency.FirstToken, plan.Latency.Chunk)
	}
	wire.ApplyHeaders(w, plan.Headers)
}

func (h *Handler) writeRaw(w http.ResponseWriter, plan *engine.Plan) {
	h.prepareHeaders(w, plan)
	w.Header().Set("Content-Type", "application/json")
	status := plan.RawStatus
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if _, err := w.Write(plan.RawJSON); err != nil {
		h.logger.Error("write raw response", "error", err, "request_id", plan.Meta.RequestID)
	}
}

func buildMessageResponse(plan *engine.Plan) map[string]any {
	content := make([]any, 0)
	for _, block := range plan.Reasoning {
		if block.Redacted {
			content = append(content, map[string]any{
				"type": "redacted_thinking",
				"data": block.RedactedData,
			})
			continue
		}
		if block.Text == "" && block.Signature == "" {
			continue
		}
		content = append(content, map[string]any{
			"type":      "thinking",
			"thinking":  block.Text,
			"signature": block.Signature,
		})
	}
	if plan.Refusal != "" {
		content = append(content, map[string]any{"type": "text", "text": plan.Refusal})
	} else if plan.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": plan.Text})
	}
	for _, call := range plan.ToolCalls {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Name,
			"input": toolInput(call.Arguments),
		})
	}
	return map[string]any{
		"id":            plan.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         plan.Model,
		"content":       content,
		"stop_reason":   stopReason(plan),
		"stop_sequence": nilIfEmpty(plan.StopSequence),
		"stop_details":  nil,
		"usage":         messageUsage(plan.Usage),
	}
}

func messageUsage(usage engine.Usage) map[string]any {
	return map[string]any{
		"input_tokens":                usage.InputTokens,
		"output_tokens":               usage.OutputTokens,
		"cache_creation_input_tokens": usage.CacheWriteTokens,
		"cache_read_input_tokens":     usage.CachedTokens,
		"cache_creation": map[string]any{
			"ephemeral_5m_input_tokens": usage.CacheWriteTokens,
			"ephemeral_1h_input_tokens": 0,
		},
		"output_tokens_details": map[string]any{
			"thinking_tokens": usage.ReasoningTokens,
		},
		"service_tier": "standard",
	}
}

func stopReason(plan *engine.Plan) string {
	switch plan.Finish {
	case engine.FinishLength:
		return "max_tokens"
	case engine.FinishToolCalls:
		return "tool_use"
	case engine.FinishRefusal, engine.FinishContentFilter:
		return "refusal"
	case engine.FinishStop:
		if plan.StopSequence != "" {
			return "stop_sequence"
		}
		return "end_turn"
	default:
		if len(plan.ToolCalls) > 0 {
			return "tool_use"
		}
		return "end_turn"
	}
}

func toolInput(arguments string) any {
	if strings.TrimSpace(arguments) == "" {
		return map[string]any{}
	}
	var input any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		return map[string]any{}
	}
	if input == nil {
		return map[string]any{}
	}
	return input
}

// outputBlock is one streamed content block.
type outputBlock struct {
	Type      string
	Text      string
	Signature string
	Data      string
	ID        string
	Name      string
	Arguments string
}

func outputBlocks(plan *engine.Plan) []outputBlock {
	var blocks []outputBlock
	for _, block := range plan.Reasoning {
		if block.Redacted {
			blocks = append(blocks, outputBlock{Type: "redacted_thinking", Data: block.RedactedData})
			continue
		}
		if block.Text == "" && block.Signature == "" {
			continue
		}
		blocks = append(blocks, outputBlock{Type: "thinking", Text: block.Text, Signature: block.Signature})
	}
	if plan.Refusal != "" {
		blocks = append(blocks, outputBlock{Type: "text", Text: plan.Refusal})
	} else if plan.Text != "" {
		blocks = append(blocks, outputBlock{Type: "text", Text: plan.Text})
	}
	for _, call := range plan.ToolCalls {
		blocks = append(blocks, outputBlock{
			Type:      "tool_use",
			ID:        call.ID,
			Name:      call.Name,
			Arguments: call.Arguments,
		})
	}
	return blocks
}

func (h *Handler) streamMessages(w http.ResponseWriter, r *http.Request, plan *engine.Plan) {
	h.prepareHeaders(w, plan)
	if plan.Latency.Total > 0 {
		w.Header().Set("anthropic-processing-ms", formatMillis(plan.Latency.Total))
	}
	writer := sse.New(w)
	if err := h.clock.Sleep(r.Context(), plan.Latency.FirstToken); err != nil {
		return
	}

	streamer := &messagesStreamer{
		writer:    writer,
		ctx:       r.Context(),
		plan:      plan,
		delay:     plan.Latency.Chunk,
		chunkSize: plan.Latency.ChunkSize,
		chunkUnit: plan.Latency.ChunkUnit,
		sendPings: plan.Stream.SendPings || h.cfg.Providers.Anthropic.StreamPingEnabled(),
	}

	startUsage := messageUsage(plan.Usage)
	startUsage["output_tokens"] = 1
	delete(startUsage, "output_tokens_details")
	if !streamer.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            plan.ID,
			"type":          "message",
			"role":          "assistant",
			"model":         plan.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         startUsage,
		},
	}) {
		return
	}
	if streamer.sendPings {
		if !streamer.emit("ping", map[string]any{"type": "ping"}) {
			return
		}
	}

	for index, block := range outputBlocks(plan) {
		if !streamer.block(index, block) {
			return
		}
	}

	deltaEvent := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason(plan),
			"stop_sequence": nilIfEmpty(plan.StopSequence),
			"stop_details":  nil,
		},
	}
	if !plan.Stream.OmitUsage {
		deltaEvent["usage"] = map[string]any{
			"output_tokens": plan.Usage.OutputTokens,
			"output_tokens_details": map[string]any{
				"thinking_tokens": plan.Usage.ReasoningTokens,
			},
		}
	}
	if !streamer.emit("message_delta", deltaEvent) {
		return
	}
	if h.cfg.Defaults.Stream.SendDoneEnabled() && !plan.Stream.OmitDone {
		streamer.emit("message_stop", map[string]any{"type": "message_stop"})
	}
}

type messagesStreamer struct {
	writer    *sse.Writer
	ctx       context.Context
	plan      *engine.Plan
	delay     time.Duration
	chunkSize int
	chunkUnit string
	sendPings bool
	emitted   int
}

func (s *messagesStreamer) emit(name string, payload map[string]any) bool {
	if s.ctx.Err() != nil {
		return false
	}
	if limit := s.plan.Stream.AbortAfterChunks; limit > 0 && s.emitted >= limit {
		return false
	}
	if err := s.writer.Event(name, payload); err != nil {
		return false
	}
	s.emitted++
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-s.ctx.Done():
			return false
		}
	}
	if s.plan.Stream.Error != nil && s.plan.Stream.ErrorAfterChunks > 0 && s.emitted >= s.plan.Stream.ErrorAfterChunks {
		s.writeError()
		return false
	}
	return true
}

func (s *messagesStreamer) writeError() {
	if s.plan.Stream.Error == nil {
		return
	}
	_ = s.writer.Event("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    s.plan.Stream.Error.Type,
			"message": s.plan.Stream.Error.Message,
		},
	})
}

func (s *messagesStreamer) block(index int, block outputBlock) bool {
	switch block.Type {
	case "redacted_thinking":
		if !s.emit("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         index,
			"content_block": map[string]any{"type": "redacted_thinking", "data": block.Data},
		}) {
			return false
		}
		return s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	case "thinking":
		if !s.emit("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         index,
			"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
		}) {
			return false
		}
		for _, delta := range chunk.Split(block.Text, s.chunkSize, s.chunkUnit) {
			if !s.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "thinking_delta", "thinking": delta},
			}) {
				return false
			}
		}
		if block.Signature != "" {
			if !s.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "signature_delta", "signature": block.Signature},
			}) {
				return false
			}
		}
		return s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	case "text":
		if !s.emit("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         index,
			"content_block": map[string]any{"type": "text", "text": ""},
		}) {
			return false
		}
		for _, delta := range chunk.Split(block.Text, s.chunkSize, s.chunkUnit) {
			if !s.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "text_delta", "text": delta},
			}) {
				return false
			}
		}
		return s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	case "tool_use":
		if !s.emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    block.ID,
				"name":  block.Name,
				"input": map[string]any{},
			},
		}) {
			return false
		}
		for _, delta := range chunk.Split(block.Arguments, s.chunkSize, s.chunkUnit) {
			if !s.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": delta},
			}) {
				return false
			}
		}
		return s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	default:
		return true
	}
}

func (h *Handler) options() wire.Options {
	return wire.Options{
		Provider:   apierr.ProviderAnthropic,
		APIVersion: h.cfg.Providers.Anthropic.APIVersion,
	}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, opts wire.Options, apiErr *apierr.Error) {
	if opts.RequestID == "" {
		opts.RequestID = shared.RequestID(r)
	}
	wire.WriteError(w, opts, apiErr)
	if trace := observe.From(r.Context()); trace != nil {
		trace.SetStatus(apiErr.Status)
		trace.SetError(apiErr.Code)
	}
}

func (h *Handler) failGenerate(w http.ResponseWriter, r *http.Request, opts wire.Options, err error) {
	if apiErr, ok := err.(*apierr.Error); ok {
		h.fail(w, r, opts, apiErr)
		return
	}
	h.fail(w, r, opts, apierr.New(http.StatusInternalServerError, "api_error", "internal_error", err.Error()))
}

func formatMillis(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
