// Package httpx contains small HTTP helpers shared by provider adapters.
package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/apierr"
)

// WriteJSON writes v as JSON with the given status. HTML escaping is disabled
// to match provider behavior for strings containing angle brackets.
func WriteJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Marshal is a helper that encodes without HTML escaping or a trailing newline.
func Marshal(v any) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(buf.String(), "\n")), nil
}

func decodeError(err error) *apierr.Error {
	if maxErr, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return apierr.New(http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large",
			fmt.Sprintf("Request body exceeds the %d byte limit.", maxErr.Limit))
	}
	if syntax, ok := errors.AsType[*json.SyntaxError](err); ok {
		return apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_json",
			fmt.Sprintf("Invalid JSON in request body at byte %d: %s", syntax.Offset, syntax.Error()))
	}
	if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		return apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_type",
			fmt.Sprintf("Invalid type for %s: expected %s but received %s.", field, typeErr.Type, typeErr.Value)).WithParam(field)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_json", "Request body is empty or truncated.")
	}
	return apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_json", "Unable to parse request body: "+err.Error())
}

// ClientIP extracts a stable client address for session fallback.
func ClientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first := strings.TrimSpace(strings.Split(forwarded, ",")[0])
		if first != "" {
			return first
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ReadBody reads a request body with an optional size limit.
func ReadBody(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, *apierr.Error) {
	body := r.Body
	if maxBytes > 0 {
		body = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		if maxErr, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, apierr.New(http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large",
				fmt.Sprintf("Request body exceeds the %d byte limit.", maxErr.Limit))
		}
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_body", "Unable to read request body: "+err.Error())
	}
	if len(raw) == 0 {
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_json", "Request body is empty.")
	}
	return raw, nil
}

// DecodeBytes decodes a JSON object from raw bytes and rejects trailing data.
func DecodeBytes(raw []byte, dst any) *apierr.Error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_json", "Request body must contain a single JSON object.")
		}
		return decodeError(err)
	}
	return nil
}

// DecodeMap decodes raw bytes into a generic object for scenario matching.
func DecodeMap(raw []byte) map[string]any {
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// BearerToken extracts a bearer token from the Authorization header.
func BearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if value == "" {
		return ""
	}
	parts := strings.SplitN(value, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
