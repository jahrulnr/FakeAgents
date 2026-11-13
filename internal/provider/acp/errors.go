package acp

import (
	"net/http"

	"github.com/jahrulnr/fakeagents/internal/apierr"
)

// toRPCError maps a provider-neutral apierr.Error onto a JSON-RPC error. ACP
// reserves -32768..-32000 and -32899..-32800; provider-style failures carry
// their HTTP semantics in data so no information is lost at the boundary.
func toRPCError(err *apierr.Error) *rpcError {
	if err == nil {
		return nil
	}
	code := codeInternalError
	switch {
	case err.Status == http.StatusUnauthorized || err.Status == http.StatusForbidden:
		code = codeAuthRequired
	case err.Status == http.StatusNotFound:
		code = codeNotFound
	}
	data := map[string]any{
		"status": err.Status,
		"type":   err.Type,
		"code":   err.Code,
	}
	if err.Param != "" {
		data["param"] = err.Param
	}
	if err.RetryAfter > 0 {
		data["retryAfterMs"] = err.RetryAfter.Milliseconds()
	}
	return rpcErr(code, err.Message, data)
}

// notFound builds the standard ACP resource-not-found error for sessions.
func sessionNotFound(sessionID string) *rpcError {
	return rpcErr(codeNotFound, "session not found", map[string]any{"sessionId": sessionID})
}
