// Package shared contains helpers used by both provider adapters.
package shared

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/httpx"
	"github.com/jahrulnr/fakeagents/internal/observe"
	"github.com/jahrulnr/fakeagents/internal/session"
)

// Session resolves a session key and advances the turn counter. A
// X-FakeAgents-Turn header forces a specific turn without incrementing.
func Session(cfg *config.Config, r *http.Request, tracker session.Tracker, hints ...string) (string, int, bool) {
	key := session.Resolve(r.Header, cfg.Defaults.SessionHeaders, httpx.ClientIP(r), hints...)
	if raw := strings.TrimSpace(r.Header.Get("X-FakeAgents-Turn")); raw != "" {
		if turn, err := strconv.Atoi(raw); err == nil && turn >= 0 {
			return key, turn, true
		}
	}
	if tracker != nil {
		return key, tracker.Next(key), true
	}
	return key, 0, false
}

// RequestID returns the middleware-generated request id, or an empty string.
func RequestID(r *http.Request) string {
	if trace := observe.From(r.Context()); trace != nil {
		return trace.RequestID
	}
	return ""
}

// Record updates the request trace with provider metadata.
func Record(r *http.Request, provider, endpoint, model, scenario, sessionKey string, turn int, stream bool) {
	trace := observe.From(r.Context())
	if trace == nil {
		return
	}
	trace.SetProvider(provider, endpoint)
	trace.SetSession(sessionKey, turn)
	trace.SetStream(stream)
	if model != "" {
		trace.SetModel(model)
	}
	if scenario != "" {
		trace.SetScenario(scenario)
	}
}

// RecordTools captures the harness tool catalog when debug.record_tools is on.
func RecordTools(r *http.Request, enabled bool, tools []engine.Tool) {
	if !enabled || len(tools) == 0 {
		return
	}
	trace := observe.From(r.Context())
	if trace == nil {
		return
	}
	infos := make([]observe.ToolInfo, 0, len(tools))
	for _, tool := range tools {
		var schema json.RawMessage
		if tool.Schema != nil {
			if encoded, err := json.Marshal(tool.Schema); err == nil {
				schema = encoded
			}
		}
		infos = append(infos, observe.ToolInfo{
			Name:        tool.Name,
			Description: tool.Description,
			Schema:      schema,
		})
	}
	trace.SetTools(infos)
}

// RecordStep updates the trace with the active script/agent step.
func RecordStep(r *http.Request, step int) {
	if trace := observe.From(r.Context()); trace != nil {
		trace.SetStep(step)
	}
}

// RecordUsage updates the trace with token counts.
func RecordUsage(r *http.Request, input, output int) {
	if trace := observe.From(r.Context()); trace != nil {
		trace.SetUsage(input, output)
	}
}
