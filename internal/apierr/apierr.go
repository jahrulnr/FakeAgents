// Package apierr models provider-native API errors and ships a catalogue of
// realistic presets used by scenario configuration.
package apierr

import (
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"
)

// Error is a neutral API error. Provider adapters render it using their native
// envelope (OpenAI or Anthropic) without losing status or retry metadata.
type Error struct {
	Status  int
	Type    string
	Code    string
	Param   string
	Message string
	Headers map[string]string

	// RetryAfter is emitted as a Retry-After header when greater than zero.
	RetryAfter time.Duration
}

// New builds an error with the given fields.
func New(status int, typ, code, message string) *Error {
	return &Error{Status: status, Type: typ, Code: code, Message: message}
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d %s: %s", e.Status, e.Type, e.Message)
}

// WithParam returns the error with a param set (OpenAI style).
func (e *Error) WithParam(param string) *Error {
	e.Param = param
	return e
}

// WithHeaders replaces the extra response headers.
func (e *Error) WithHeaders(headers map[string]string) *Error {
	e.Headers = headers
	return e
}

// WithRetryAfter sets the Retry-After duration.
func (e *Error) WithRetryAfter(d time.Duration) *Error {
	e.RetryAfter = d
	return e
}

// Clone returns a deep-enough copy so callers can safely mutate a preset.
func (e *Error) Clone() *Error {
	if e == nil {
		return nil
	}
	out := *e
	if e.Headers != nil {
		out.Headers = make(map[string]string, len(e.Headers))
		maps.Copy(out.Headers, e.Headers)
	}
	return &out
}

// Preset returns a realistic provider-specific error by name. The returned
// value is always a copy and may be modified by the caller.
func Preset(provider, name string) (*Error, bool) {
	name = normalizePreset(name)
	if provider == ProviderAnthropic {
		e, ok := anthropicPresets[name]
		if !ok {
			return nil, false
		}
		return e.Clone(), true
	}
	e, ok := openAIPresets[name]
	if !ok {
		return nil, false
	}
	return e.Clone(), true
}

// MustPreset is Preset with a fallback to a generic invalid request error when
// the name is unknown.
func MustPreset(provider, name string) *Error {
	if e, ok := Preset(provider, name); ok {
		return e
	}
	return PresetOrGeneric(provider, "invalid_request")
}

// PresetOrGeneric returns the named preset or a generic error.
func PresetOrGeneric(provider, name string) *Error {
	if e, ok := Preset(provider, name); ok {
		return e
	}
	return &Error{
		Status:  http.StatusBadRequest,
		Type:    invalidRequestType(provider),
		Code:    "invalid_request",
		Message: fmt.Sprintf("Unknown error preset %q", name),
	}
}

// KnownPreset reports whether a preset name exists for at least one provider.
func KnownPreset(name string) bool {
	name = normalizePreset(name)
	_, openAI := openAIPresets[name]
	_, anthropic := anthropicPresets[name]
	return openAI || anthropic
}

func normalizePreset(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "billing", "billing_error", "insufficient_quota", "quota", "quota_exceeded", "payment_required":
		return "billing"
	case "auth", "authentication", "invalid_api_key", "authentication_error":
		return "authentication"
	case "permission", "permission_error", "forbidden":
		return "permission"
	case "not_found", "not_found_error", "notfound":
		return "not_found"
	case "model_not_found", "modelnotfound":
		return "model_not_found"
	case "rate_limit", "rate_limit_error", "ratelimit", "rate_limited", "tpm", "rpm":
		return "rate_limit"
	case "overloaded", "overloaded_error", "capacity":
		return "overloaded"
	case "server", "server_error", "internal", "internal_error":
		return "server_error"
	case "timeout", "timeout_error", "gateway_timeout":
		return "timeout"
	case "invalid", "invalid_request", "invalid_request_error", "bad_request":
		return "invalid_request"
	case "context", "context_length", "context_length_exceeded":
		return "context_length"
	case "request_too_large", "payload_too_large", "too_large":
		return "request_too_large"
	case "content_policy", "content_filter", "content_policy_violation":
		return "content_policy"
	case "conflict", "conflict_error":
		return "conflict"
	default:
		return name
	}
}

// ProviderAnthropic is the canonical Anthropic provider id.
const ProviderAnthropic = "anthropic"

// ProviderOpenAI is the canonical OpenAI provider id.
const ProviderOpenAI = "openai"

// ProviderACP is the canonical Agent Client Protocol provider id.
const ProviderACP = "acp"

func invalidRequestType(provider string) string {
	if provider == ProviderAnthropic {
		return "invalid_request_error"
	}
	return "invalid_request_error"
}

var openAIPresets = map[string]*Error{
	"billing": {
		Status:  http.StatusTooManyRequests,
		Type:    "insufficient_quota",
		Code:    "insufficient_quota",
		Message: "You exceeded your current quota, please check your plan and billing details. For more information on this error, read the docs: https://platform.openai.com/docs/guides/error-codes/api-errors.",
	},
	"authentication": {
		Status:  http.StatusUnauthorized,
		Type:    "invalid_request_error",
		Code:    "invalid_api_key",
		Message: "Incorrect API key provided. You can find your API key at https://platform.openai.com/account/api-keys.",
	},
	"permission": {
		Status:  http.StatusForbidden,
		Type:    "invalid_request_error",
		Code:    "permission_denied",
		Message: "You do not have permission to access this resource.",
	},
	"not_found": {
		Status:  http.StatusNotFound,
		Type:    "invalid_request_error",
		Code:    "not_found",
		Message: "The requested resource was not found.",
	},
	"model_not_found": {
		Status:  http.StatusNotFound,
		Type:    "invalid_request_error",
		Code:    "model_not_found",
		Message: "The model does not exist or you do not have access to it.",
	},
	"rate_limit": {
		Status:  http.StatusTooManyRequests,
		Type:    "rate_limit_exceeded",
		Code:    "rate_limit_exceeded",
		Message: "Rate limit reached for requests. Please try again later.",
	},
	"overloaded": {
		Status:  http.StatusServiceUnavailable,
		Type:    "server_error",
		Code:    "server_error",
		Message: "The server is currently overloaded. Please try again later.",
	},
	"server_error": {
		Status:  http.StatusInternalServerError,
		Type:    "server_error",
		Code:    "server_error",
		Message: "The server had an error while processing your request.",
	},
	"timeout": {
		Status:  http.StatusGatewayTimeout,
		Type:    "server_error",
		Code:    "timeout",
		Message: "The request timed out while processing.",
	},
	"invalid_request": {
		Status:  http.StatusBadRequest,
		Type:    "invalid_request_error",
		Code:    "invalid_request",
		Message: "Invalid request.",
	},
	"context_length": {
		Status:  http.StatusBadRequest,
		Type:    "invalid_request_error",
		Code:    "context_length_exceeded",
		Message: "Your input exceeds the context window of this model. Please adjust your input and try again.",
	},
	"request_too_large": {
		Status:  http.StatusRequestEntityTooLarge,
		Type:    "invalid_request_error",
		Code:    "request_too_large",
		Message: "The request body is too large.",
	},
	"content_policy": {
		Status:  http.StatusBadRequest,
		Type:    "invalid_request_error",
		Code:    "content_policy_violation",
		Message: "Your request was rejected as a result of our safety system.",
	},
	"conflict": {
		Status:  http.StatusConflict,
		Type:    "invalid_request_error",
		Code:    "conflict",
		Message: "The resource is being modified by another request.",
	},
}

var anthropicPresets = map[string]*Error{
	"billing": {
		Status:  http.StatusPaymentRequired,
		Type:    "billing_error",
		Message: "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits.",
	},
	"authentication": {
		Status:  http.StatusUnauthorized,
		Type:    "authentication_error",
		Message: "invalid x-api-key",
	},
	"permission": {
		Status:  http.StatusForbidden,
		Type:    "permission_error",
		Message: "Your API key does not have permission to use the specified resource.",
	},
	"not_found": {
		Status:  http.StatusNotFound,
		Type:    "not_found_error",
		Message: "The requested resource was not found.",
	},
	"model_not_found": {
		Status:  http.StatusNotFound,
		Type:    "not_found_error",
		Message: "model: The requested model was not found.",
	},
	"rate_limit": {
		Status:  http.StatusTooManyRequests,
		Type:    "rate_limit_error",
		Message: "This request would exceed your rate limit. Please retry after a short delay.",
	},
	"overloaded": {
		Status:  529,
		Type:    "overloaded_error",
		Message: "Overloaded",
	},
	"server_error": {
		Status:  http.StatusInternalServerError,
		Type:    "api_error",
		Message: "Internal server error",
	},
	"timeout": {
		Status:  http.StatusGatewayTimeout,
		Type:    "timeout_error",
		Message: "The request timed out while processing.",
	},
	"invalid_request": {
		Status:  http.StatusBadRequest,
		Type:    "invalid_request_error",
		Message: "Invalid request.",
	},
	"context_length": {
		Status:  http.StatusBadRequest,
		Type:    "invalid_request_error",
		Message: "prompt is too long: the input exceeds the model context window.",
	},
	"request_too_large": {
		Status:  http.StatusRequestEntityTooLarge,
		Type:    "request_too_large",
		Message: "Request size exceeds the maximum allowed size.",
	},
	"content_policy": {
		Status:  http.StatusBadRequest,
		Type:    "invalid_request_error",
		Message: "The request was rejected by the safety system.",
	},
	"conflict": {
		Status:  http.StatusConflict,
		Type:    "conflict_error",
		Message: "The resource is being modified by another request.",
	},
}
