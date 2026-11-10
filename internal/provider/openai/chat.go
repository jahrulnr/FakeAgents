// Package openai implements the OpenAI Chat Completions, Responses, and Models
// endpoints.
package openai

import (
	"context"
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
	"github.com/jahrulnr/fakeagents/internal/store"
	"github.com/jahrulnr/fakeagents/internal/wire"
)

// Deps are the handler dependencies.
type Deps struct {
	Config    *config.Config
	Engine    *engine.Engine
	Sealer    envelope.Sealer
	Sessions  session.Tracker
	Limiter   *ratelimit.Limiter
	Responses store.Store
	Clock     clock.Clock
	IDs       id.Generator
	Logger    *slog.Logger
}

// Handler serves OpenAI-compatible endpoints.
type Handler struct {
	cfg       *config.Config
	engine    *engine.Engine
	sealer    envelope.Sealer
	sessions  session.Tracker
	limiter   *ratelimit.Limiter
	responses store.Store
	clock     clock.Clock
	ids       id.Generator
	logger    *slog.Logger
}

// New builds an OpenAI handler.
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
		cfg:       deps.Config,
		engine:    deps.Engine,
		sealer:    deps.Sealer,
		sessions:  deps.Sessions,
		limiter:   deps.Limiter,
		responses: deps.Responses,
		clock:     clk,
		ids:       ids,
		logger:    logger,
	}
}

// ChatCompletions handles POST /v1/chat/completions.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	opts := h.options(r)
	raw, apiErr := httpx.ReadBody(w, r, h.cfg.Server.MaxBodyBytes)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	var body ChatRequest
	if apiErr := httpx.DecodeBytes(raw, &body); apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	rawMap := httpx.DecodeMap(raw)

	sessionKey, turn, turnSet := shared.Session(h.cfg, r, h.sessions,
		body.PromptCacheKey, body.User, body.Metadata["session_id"])
	req, apiErr := buildChatRequest(h.cfg, &body, rawMap, shared.RequestID(r), sessionKey, turn, turnSet)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	req.Headers = r.Header
	req.RawBody = raw
	shared.RecordTools(r, h.cfg.Debug.RecordToolsEnabled(), req.Tools)

	model, apiErr := h.engine.ResolveModel(apierr.ProviderOpenAI, req.Model)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	req.Model = model
	shared.Record(r, apierr.ProviderOpenAI, "chat.completions", model, "", sessionKey, turn, body.Stream)

	requestLimit := ratelimit.DecisionFrom(r.Context())
	tokenLimit := h.limiter.AdmitTokens(sessionKey, h.engine.CountInputTokens(req))
	if !tokenLimit.Allowed {
		wire.SetRateLimitHeaders(w, apierr.ProviderOpenAI, requestLimit, tokenLimit)
		apiErr := apierr.MustPreset(apierr.ProviderOpenAI, "rate_limit")
		apiErr.RetryAfter = tokenLimit.RetryAfter
		h.fail(w, r, opts, apiErr)
		return
	}
	if h.cfg.RateLimit.HeadersEnabled() {
		wire.SetRateLimitHeaders(w, apierr.ProviderOpenAI, requestLimit, tokenLimit)
	}

	plan, err := h.engine.Generate(r.Context(), req)
	if err != nil {
		h.failGenerate(w, r, opts, err)
		return
	}
	shared.Record(r, apierr.ProviderOpenAI, "chat.completions", plan.Model, plan.Meta.Scenario, sessionKey, plan.Meta.Turn, body.Stream)
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
		h.streamChat(w, r, req, plan, body.StreamOptions)
		return
	}
	if err := h.clock.Sleep(r.Context(), plan.Latency.Total); err != nil {
		return
	}
	h.writeChat(w, plan)
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

func (h *Handler) writeChat(w http.ResponseWriter, plan *engine.Plan) {
	h.prepareHeaders(w, plan)
	if plan.Latency.Total > 0 {
		w.Header().Set("openai-processing-ms", formatMillis(plan.Latency.Total))
	}
	response := buildChatResponse(h.cfg, plan)
	if err := httpx.WriteJSON(w, http.StatusOK, response); err != nil {
		h.logger.Error("write chat response", "error", err, "request_id", plan.Meta.RequestID)
	}
}

func (h *Handler) prepareHeaders(w http.ResponseWriter, plan *engine.Plan) {
	wire.SetBaseHeaders(w, optsFor(plan.Meta.RequestID))
	if h.cfg.Debug.HeadersEnabled() {
		wire.SetDebugHeaders(w, plan.Meta.Scenario, plan.Meta.Session, plan.Meta.Turn, plan.Meta.Step, plan.Meta.Seed, plan.Model)
		wire.SetSimulatedLatency(w, plan.Latency.Total, plan.Latency.FirstToken, plan.Latency.Chunk)
	}
	wire.ApplyHeaders(w, plan.Headers)
	w.Header().Set("openai-model", plan.Model)
}

type chatMessageOut struct {
	Role             string            `json:"role"`
	Content          *string           `json:"content"`
	Refusal          *string           `json:"refusal,omitempty"`
	Annotations      []any             `json:"annotations"`
	ToolCalls        []chatToolCallOut `json:"tool_calls,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	Reasoning        string            `json:"reasoning,omitempty"`
}

type chatToolCallOut struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatChoice struct {
	Index        int            `json:"index"`
	Message      chatMessageOut `json:"message"`
	Logprobs     any            `json:"logprobs"`
	FinishReason string         `json:"finish_reason"`
}

type chatResponse struct {
	ID                string       `json:"id"`
	Object            string       `json:"object"`
	Created           int64        `json:"created"`
	Model             string       `json:"model"`
	SystemFingerprint string       `json:"system_fingerprint,omitempty"`
	Choices           []chatChoice `json:"choices"`
	Usage             chatUsage    `json:"usage"`
	ServiceTier       string       `json:"service_tier,omitempty"`
}

type chatUsage struct {
	PromptTokens            int                  `json:"prompt_tokens"`
	CompletionTokens        int                  `json:"completion_tokens"`
	TotalTokens             int                  `json:"total_tokens"`
	PromptTokensDetails     chatPromptDetails    `json:"prompt_tokens_details"`
	CompletionTokensDetails chatCompletionDetail `json:"completion_tokens_details"`
}

type chatPromptDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

type chatCompletionDetail struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func buildChatResponse(cfg *config.Config, plan *engine.Plan) chatResponse {
	reasoning := reasoningText(plan)
	content := plan.Text
	message := chatMessageOut{
		Role:        "assistant",
		Annotations: []any{},
	}
	switch {
	case plan.Refusal != "":
		message.Content = nil
		refusal := plan.Refusal
		message.Refusal = &refusal
	case plan.Text == "" && len(plan.ToolCalls) > 0:
		message.Content = nil
	default:
		message.Content = &content
	}
	for _, call := range plan.ToolCalls {
		converted := chatToolCallOut{ID: call.ID, Type: "function"}
		converted.Function.Name = call.Name
		converted.Function.Arguments = call.Arguments
		message.ToolCalls = append(message.ToolCalls, converted)
	}
	if reasoning != "" {
		switch strings.ToLower(cfg.Providers.OpenAI.ChatReasoningField) {
		case "reasoning":
			message.Reasoning = reasoning
		case "both":
			message.ReasoningContent = reasoning
			message.Reasoning = reasoning
		case "none":
		default:
			message.ReasoningContent = reasoning
		}
	}
	return chatResponse{
		ID:                plan.ID,
		Object:            "chat.completion",
		Created:           plan.CreatedAt.Unix(),
		Model:             plan.Model,
		SystemFingerprint: cfg.Providers.OpenAI.SystemFingerprint,
		Choices: []chatChoice{{
			Index:        0,
			Message:      message,
			Logprobs:     nil,
			FinishReason: chatFinishReason(plan),
		}},
		Usage:       buildChatUsage(plan.Usage),
		ServiceTier: "default",
	}
}

func buildChatUsage(usage engine.Usage) chatUsage {
	prompt := usage.InputTokens + usage.CachedTokens + usage.CacheWriteTokens
	return chatUsage{
		PromptTokens:     prompt,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      prompt + usage.OutputTokens,
		PromptTokensDetails: chatPromptDetails{
			CachedTokens:     usage.CachedTokens,
			CacheWriteTokens: usage.CacheWriteTokens,
		},
		CompletionTokensDetails: chatCompletionDetail{ReasoningTokens: usage.ReasoningTokens},
	}
}

func chatFinishReason(plan *engine.Plan) string {
	switch plan.Finish {
	case engine.FinishLength:
		return "length"
	case engine.FinishToolCalls:
		return "tool_calls"
	case engine.FinishContentFilter:
		return "content_filter"
	case engine.FinishRefusal:
		return "stop"
	default:
		if len(plan.ToolCalls) > 0 {
			return "tool_calls"
		}
		return "stop"
	}
}

func reasoningText(plan *engine.Plan) string {
	var builder strings.Builder
	for _, block := range plan.Reasoning {
		if block.Redacted {
			continue
		}
		builder.WriteString(block.Text)
	}
	return builder.String()
}

func (h *Handler) streamChat(w http.ResponseWriter, r *http.Request, req *engine.Request, plan *engine.Plan, streamOptions *ChatStreamOptions) {
	h.prepareHeaders(w, plan)
	if plan.Latency.Total > 0 {
		w.Header().Set("openai-processing-ms", formatMillis(plan.Latency.Total))
	}
	writer := sse.New(w)
	if err := h.clock.Sleep(r.Context(), plan.Latency.FirstToken); err != nil {
		return
	}

	openRouter := r.URL.Path == "/api/v1/chat/completions"
	includeUsage := openRouter || streamOptions != nil && streamOptions.IncludeUsage
	if h.cfg.Defaults.Stream.IncludeUsage != nil {
		includeUsage = *h.cfg.Defaults.Stream.IncludeUsage
	}
	if plan.Stream.OmitUsage {
		includeUsage = false
	}
	sendDone := h.cfg.Defaults.Stream.SendDoneEnabled() && !plan.Stream.OmitDone

	streamer := &chatStreamer{
		writer:         writer,
		plan:           plan,
		chunkSize:      plan.Latency.ChunkSize,
		chunkUnit:      plan.Latency.ChunkUnit,
		chunkDelay:     plan.Latency.Chunk,
		ctx:            r.Context(),
		fingerprint:    h.cfg.Providers.OpenAI.SystemFingerprint,
		reasoningField: h.reasoningField(),
		includeUsage:   includeUsage,
		openRouter:     openRouter,
	}
	if openRouter && includeUsage {
		streamer.openRouterUsage = h.openRouterChatUsage(plan.Model, plan.Usage)
	}

	if !streamer.emitRole() {
		return
	}
	for _, block := range plan.Reasoning {
		if block.Redacted || block.Text == "" {
			continue
		}
		if !streamer.emitReasoning(block.Text) {
			return
		}
	}
	if plan.Refusal != "" {
		if !streamer.emitRefusal(plan.Refusal) {
			return
		}
	} else if plan.Text != "" {
		if !streamer.emitText(plan.Text) {
			return
		}
	}
	if len(plan.ToolCalls) > 0 {
		if !streamer.emitToolCalls(plan.ToolCalls) {
			return
		}
	}
	if !streamer.emitFinish(chatFinishReason(plan)) {
		return
	}
	if includeUsage {
		if !streamer.emitUsage(buildChatUsage(plan.Usage)) {
			return
		}
	}
	if sendDone {
		_ = writer.RawData([]byte("[DONE]"))
	}
}

type chatStreamer struct {
	writer          *sse.Writer
	plan            *engine.Plan
	chunkSize       int
	chunkUnit       string
	chunkDelay      time.Duration
	ctx             context.Context
	fingerprint     string
	reasoningField  string
	includeUsage    bool
	openRouter      bool
	openRouterUsage map[string]any
	emitted         int
}

func (s *chatStreamer) base() map[string]any {
	payload := map[string]any{
		"id":                 s.plan.ID,
		"object":             "chat.completion.chunk",
		"created":            s.plan.CreatedAt.Unix(),
		"model":              s.plan.Model,
		"system_fingerprint": s.fingerprint,
	}
	if s.includeUsage {
		payload["usage"] = nil
	}
	return payload
}

func (s *chatStreamer) emit(delta map[string]any) bool {
	if s.ctx.Err() != nil {
		return false
	}
	payload := s.base()
	choice := map[string]any{"index": 0, "delta": delta, "logprobs": nil, "finish_reason": nil}
	payload["choices"] = []any{choice}
	if !s.write(payload) {
		return false
	}
	return true
}

func (s *chatStreamer) write(payload any) bool {
	if limit := s.plan.Stream.AbortAfterChunks; limit > 0 && s.emitted >= limit {
		return false
	}
	if err := s.writer.Data(payload); err != nil {
		return false
	}
	s.emitted++
	if s.plan.Stream.Error != nil && s.plan.Stream.ErrorAfterChunks > 0 && s.emitted >= s.plan.Stream.ErrorAfterChunks {
		s.writeError()
		return false
	}
	if s.chunkDelay > 0 {
		select {
		case <-time.After(s.chunkDelay):
		case <-s.ctx.Done():
			return false
		}
	}
	return true
}

func (s *chatStreamer) writeError() {
	if s.plan.Stream.Error == nil {
		return
	}
	detail := map[string]any{
		"message": s.plan.Stream.Error.Message,
		"type":    s.plan.Stream.Error.Type,
		"param":   nil,
		"code":    nil,
	}
	if s.plan.Stream.Error.Param != "" {
		detail["param"] = s.plan.Stream.Error.Param
	}
	if s.plan.Stream.Error.Code != "" {
		detail["code"] = s.plan.Stream.Error.Code
	}
	_ = s.writer.Data(map[string]any{"error": detail})
}

func (s *chatStreamer) emitRole() bool {
	return s.emit(map[string]any{"role": "assistant", "content": ""})
}

func (s *chatStreamer) emitReasoning(text string) bool {
	parts := chunk.Split(text, s.chunkSize, s.chunkUnit)
	if len(parts) == 0 {
		return true
	}
	field := s.reasoningField
	if field == "" || field == "none" {
		return true
	}
	fields := []string{field}
	if field == "both" {
		fields = []string{"reasoning_content", "reasoning"}
	}
	for _, part := range parts {
		delta := make(map[string]any, len(fields))
		for _, name := range fields {
			delta[name] = part
		}
		if !s.emit(delta) {
			return false
		}
	}
	return true
}

func (s *chatStreamer) emitText(text string) bool {
	for _, part := range chunk.Split(text, s.chunkSize, s.chunkUnit) {
		if !s.emit(map[string]any{"content": part}) {
			return false
		}
	}
	return true
}

func (s *chatStreamer) emitRefusal(text string) bool {
	for _, part := range chunk.Split(text, s.chunkSize, s.chunkUnit) {
		if !s.emit(map[string]any{"refusal": part}) {
			return false
		}
	}
	return true
}

func (s *chatStreamer) emitToolCalls(calls []engine.ToolCall) bool {
	for index, call := range calls {
		first := map[string]any{
			"tool_calls": []any{map[string]any{
				"index": index,
				"id":    call.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      call.Name,
					"arguments": "",
				},
			}},
		}
		if !s.emit(first) {
			return false
		}
		for _, part := range chunk.Split(call.Arguments, s.chunkSize, s.chunkUnit) {
			fragment := map[string]any{
				"tool_calls": []any{map[string]any{
					"index": index,
					"function": map[string]any{
						"arguments": part,
					},
				}},
			}
			if !s.emit(fragment) {
				return false
			}
		}
	}
	return true
}

func (s *chatStreamer) emitFinish(reason string) bool {
	payload := s.base()
	choice := map[string]any{
		"index":         0,
		"delta":         map[string]any{},
		"logprobs":      nil,
		"finish_reason": reason,
	}
	if s.openRouter {
		choice["native_finish_reason"] = reason
	}
	payload["choices"] = []any{choice}
	return s.write(payload)
}

func (s *chatStreamer) emitUsage(usage chatUsage) bool {
	payload := s.base()
	payload["choices"] = []any{}
	payload["usage"] = usage
	if s.openRouter {
		reason := chatFinishReason(s.plan)
		payload["choices"] = []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": reason, "native_finish_reason": reason,
		}}
		payload["usage"] = s.openRouterUsage
	}
	return s.write(payload)
}

func (h *Handler) options(r *http.Request) wire.Options {
	return wire.Options{
		Provider:  apierr.ProviderOpenAI,
		RequestID: shared.RequestID(r),
	}
}

func optsFor(requestID string) wire.Options {
	return wire.Options{Provider: apierr.ProviderOpenAI, RequestID: requestID}
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
	h.fail(w, r, opts, apierr.New(http.StatusInternalServerError, "server_error", "server_error", err.Error()))
}

func (h *Handler) reasoningField() string {
	field := strings.ToLower(strings.TrimSpace(h.cfg.Providers.OpenAI.ChatReasoningField))
	if field == "" {
		return "reasoning_content"
	}
	return field
}

func formatMillis(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}
