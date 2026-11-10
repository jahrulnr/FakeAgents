// Package wire renders provider-native HTTP envelopes and headers. Middleware
// uses it too, so authentication and required-header failures look exactly like
// the upstream API.
package wire

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/httpx"
	"github.com/jahrulnr/fakeagents/internal/ratelimit"
)

// Options carries request identity used in envelopes.
type Options struct {
	Provider   string
	RequestID  string
	APIVersion string
}

// DetectProvider infers the provider from the path and authentication headers.
func DetectProvider(r *http.Request) string {
	if r == nil {
		return apierr.ProviderOpenAI
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/v1/messages") {
		return apierr.ProviderAnthropic
	}
	if strings.HasPrefix(path, "/api/v1/") {
		return apierr.ProviderOpenAI
	}
	if r.Header.Get("x-api-key") != "" || r.Header.Get("anthropic-version") != "" {
		return apierr.ProviderAnthropic
	}
	return apierr.ProviderOpenAI
}

// EndpointFromPath maps a request path to a canonical endpoint name.
func EndpointFromPath(path string) string {
	switch {
	case path == "/v1/images/generations":
		return "images.generations"
	case path == "/v1/images/edits":
		return "images.edits"
	case path == "/v1/audio/speech":
		return "audio.speech"
	case path == "/v1/audio/transcriptions":
		return "audio.transcriptions"
	case path == "/v1/chat/completions", path == "/api/v1/chat/completions":
		return "chat.completions"
	case path == "/v1/messages/count_tokens":
		return "count_tokens"
	case path == "/v1/messages":
		return "messages"
	case path == "/v1/responses", path == "/api/v1/responses":
		return "responses"
	case strings.HasPrefix(path, "/v1/responses/"):
		return "responses"
	case strings.HasPrefix(path, "/v1/models"), path == "/api/v1/models", strings.HasPrefix(path, "/api/v1/model/"):
		return "models"
	default:
		return ""
	}
}

// SetBaseHeaders installs the request identity headers both APIs expose.
func SetBaseHeaders(w http.ResponseWriter, opts Options) {
	if opts.Provider == apierr.ProviderAnthropic {
		if opts.RequestID != "" {
			w.Header().Set("request-id", opts.RequestID)
		}
		version := opts.APIVersion
		if version == "" {
			version = "2023-06-01"
		}
		w.Header().Set("anthropic-version", version)
		return
	}
	if opts.RequestID != "" {
		w.Header().Set("x-request-id", opts.RequestID)
	}
	w.Header().Set("openai-version", "2020-10-01")
}

// WriteError renders an error envelope.
func WriteError(w http.ResponseWriter, opts Options, apiErr *apierr.Error) {
	if apiErr == nil {
		apiErr = apierr.PresetOrGeneric(opts.Provider, "server_error")
	}
	SetBaseHeaders(w, opts)
	if apiErr.RetryAfter > 0 {
		seconds := int64(apiErr.RetryAfter / time.Second)
		if apiErr.RetryAfter%time.Second != 0 {
			seconds++
		}
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
	for key, value := range apiErr.Headers {
		w.Header().Set(key, value)
	}
	status := apiErr.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	if opts.Provider == apierr.ProviderAnthropic {
		body := map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    apiErr.Type,
				"message": apiErr.Message,
			},
		}
		if opts.RequestID != "" {
			body["request_id"] = opts.RequestID
		}
		_ = httpx.WriteJSON(w, status, body)
		return
	}
	detail := map[string]any{
		"message": apiErr.Message,
		"type":    apiErr.Type,
		"param":   nil,
		"code":    nil,
	}
	if apiErr.Param != "" {
		detail["param"] = apiErr.Param
	}
	if apiErr.Code != "" {
		detail["code"] = apiErr.Code
	}
	_ = httpx.WriteJSON(w, status, map[string]any{"error": detail})
}

// SetRateLimitHeaders emits the provider-native limiter headers. Zero limits
// are omitted.
func SetRateLimitHeaders(w http.ResponseWriter, provider string, request, tokens ratelimit.Decision) {
	now := time.Now().UTC()
	if provider == apierr.ProviderAnthropic {
		setAnthropicLimit(w, "requests", request, now)
		setAnthropicLimit(w, "tokens", tokens, now)
		return
	}
	setOpenAILimit(w, "requests", request)
	setOpenAILimit(w, "tokens", tokens)
}

func setOpenAILimit(w http.ResponseWriter, name string, decision ratelimit.Decision) {
	if decision.Limit <= 0 {
		return
	}
	w.Header().Set("x-ratelimit-limit-"+name, strconv.Itoa(decision.Limit))
	remaining := max(decision.Remaining, 0)
	w.Header().Set("x-ratelimit-remaining-"+name, strconv.Itoa(remaining))
	if !decision.ResetAt.IsZero() {
		reset := max(time.Until(decision.ResetAt), 0)
		w.Header().Set("x-ratelimit-reset-"+name, reset.Round(time.Millisecond).String())
	}
}

func setAnthropicLimit(w http.ResponseWriter, name string, decision ratelimit.Decision, now time.Time) {
	if decision.Limit <= 0 {
		return
	}
	w.Header().Set("anthropic-ratelimit-"+name+"-limit", strconv.Itoa(decision.Limit))
	remaining := max(decision.Remaining, 0)
	w.Header().Set("anthropic-ratelimit-"+name+"-remaining", strconv.Itoa(remaining))
	if !decision.ResetAt.IsZero() {
		w.Header().Set("anthropic-ratelimit-"+name+"-reset", decision.ResetAt.UTC().Format(time.RFC3339))
	}
}

// SetDebugHeaders adds FakeAgents correlation headers. These are additive and
// safe for clients that ignore unknown headers.
func SetDebugHeaders(w http.ResponseWriter, scenario, session string, turn, step int, seed int64, model string) {
	if scenario != "" {
		w.Header().Set("X-FakeAgents-Scenario", scenario)
	}
	if session != "" {
		w.Header().Set("X-FakeAgents-Session", session)
	}
	w.Header().Set("X-FakeAgents-Turn", strconv.Itoa(turn))
	w.Header().Set("X-FakeAgents-Step", strconv.Itoa(step))
	if seed != 0 {
		w.Header().Set("X-FakeAgents-Seed", strconv.FormatInt(seed, 10))
	}
	if model != "" {
		w.Header().Set("X-FakeAgents-Model", model)
	}
}

// SetSimulatedLatency records planned timing for test assertions.
func SetSimulatedLatency(w http.ResponseWriter, total, ttft, chunk time.Duration) {
	if total > 0 {
		w.Header().Set("X-FakeAgents-Simulated-Ms", strconv.FormatInt(total.Milliseconds(), 10))
	}
	if ttft > 0 {
		w.Header().Set("X-FakeAgents-TTFT-Ms", strconv.FormatInt(ttft.Milliseconds(), 10))
	}
	if chunk > 0 {
		w.Header().Set("X-FakeAgents-Chunk-Ms", strconv.FormatInt(chunk.Milliseconds(), 10))
	}
}

// ApplyHeaders applies scenario-provided response headers.
func ApplyHeaders(w http.ResponseWriter, headers map[string]string) {
	for key, value := range headers {
		w.Header().Set(key, value)
	}
}
