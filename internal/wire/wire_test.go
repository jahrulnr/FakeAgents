package wire_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/ratelimit"
	"github.com/jahrulnr/fakeagents/internal/wire"
)

func TestWriteOpenAIError(t *testing.T) {
	recorder := httptest.NewRecorder()
	apiErr := apierr.MustPreset(apierr.ProviderOpenAI, "billing")
	apiErr.RetryAfter = 1500 * time.Millisecond
	wire.WriteError(recorder, wire.Options{Provider: apierr.ProviderOpenAI, RequestID: "req_1"}, apiErr)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", recorder.Code)
	}
	if recorder.Header().Get("x-request-id") != "req_1" {
		t.Fatalf("request id = %q", recorder.Header().Get("x-request-id"))
	}
	if recorder.Header().Get("Retry-After") != "2" {
		t.Fatalf("retry after = %q", recorder.Header().Get("Retry-After"))
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json: %v", err)
	}
	detail := payload["error"].(map[string]any)
	if detail["code"] != "insufficient_quota" {
		t.Fatalf("payload = %v", payload)
	}
}

func TestWriteAnthropicError(t *testing.T) {
	recorder := httptest.NewRecorder()
	apiErr := apierr.MustPreset(apierr.ProviderAnthropic, "billing")
	wire.WriteError(recorder, wire.Options{
		Provider:   apierr.ProviderAnthropic,
		RequestID:  "req_2",
		APIVersion: "2023-06-01",
	}, apiErr)

	if recorder.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d", recorder.Code)
	}
	if recorder.Header().Get("request-id") != "req_2" {
		t.Fatalf("request id = %q", recorder.Header().Get("request-id"))
	}
	if recorder.Header().Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("version = %q", recorder.Header().Get("anthropic-version"))
	}
	if !strings.Contains(recorder.Body.String(), `"type":"error"`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestRateLimitHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	wire.SetRateLimitHeaders(recorder, apierr.ProviderOpenAI,
		ratelimit.Decision{Limit: 10, Remaining: 7, ResetAt: time.Now().Add(time.Second)},
		ratelimit.Decision{Limit: 100, Remaining: 42, ResetAt: time.Now().Add(time.Second)})
	if recorder.Header().Get("x-ratelimit-limit-requests") != "10" {
		t.Fatalf("headers = %v", recorder.Header())
	}
	if recorder.Header().Get("x-ratelimit-remaining-tokens") != "42" {
		t.Fatalf("headers = %v", recorder.Header())
	}

	recorder = httptest.NewRecorder()
	wire.SetRateLimitHeaders(recorder, apierr.ProviderAnthropic,
		ratelimit.Decision{Limit: 10, Remaining: 7, ResetAt: time.Now().Add(time.Second)},
		ratelimit.Decision{})
	if recorder.Header().Get("anthropic-ratelimit-requests-limit") != "10" {
		t.Fatalf("headers = %v", recorder.Header())
	}
}

func TestDetectProvider(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := wire.DetectProvider(request); got != apierr.ProviderOpenAI {
		t.Fatalf("provider = %q", got)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if got := wire.DetectProvider(request); got != apierr.ProviderAnthropic {
		t.Fatalf("provider = %q", got)
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("x-api-key", "x")
	if got := wire.DetectProvider(request); got != apierr.ProviderAnthropic {
		t.Fatalf("provider = %q", got)
	}
}
