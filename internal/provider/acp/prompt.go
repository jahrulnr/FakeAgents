package acp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/chunk"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/scenario"
)

// onPrompt runs a full prompt turn. Unlike the HTTP surfaces, ACP makes the
// agent own the tool loop inside one session/prompt: each engine iteration
// that produces tool calls is executed (simulated) and the fabricated results
// are appended to the session history before the next Generate. The loop is
// bounded by providers.acp.max_turns and ends with max_turn_requests.
func (a *Agent) onPrompt(ctx context.Context, msg *rpcMessage) (any, *rpcError) {
	var params promptParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, rpcErr(codeInvalidParams, "invalid session/prompt params: "+err.Error(), nil)
	}
	sess, notFound := a.lookup(params.SessionID)
	if notFound != nil {
		return nil, notFound
	}

	// Register under the request id before the turn gate so $/cancel_request
	// can kill a prompt that is still queued behind an in-flight turn.
	// session/cancel only reaches the running turn (registered below).
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reqKey := string(msg.ID)
	a.mu.Lock()
	a.requests[reqKey] = cancel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.requests, reqKey)
		a.mu.Unlock()
	}()

	// Serialise turns within the session. The gate is one slot deep; a queued
	// prompt still answers promptly when cancelled while waiting.
	select {
	case sess.turn <- struct{}{}:
		defer func() { <-sess.turn }()
	case <-turnCtx.Done():
		return cancelResult(), nil
	}

	a.mu.Lock()
	a.cancels[sess.id] = cancel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.cancels, sess.id)
		a.mu.Unlock()
	}()

	text := flattenPrompt(params.Prompt)
	sess.history = append(sess.history, engine.Message{Role: engine.RoleUser, Content: text})
	headers, turn, turnSet := metaOverrides(params.Meta)
	pinned := ""
	var last *engine.Plan

	maxTurns := a.cfg.Providers.ACP.MaxIterations()
	for range maxTurns {
		req := &engine.Request{
			Provider:   apierr.ProviderACP,
			Endpoint:   scenario.EndpointACPPrompt,
			Model:      a.cfg.Defaults.Model,
			Stream:     true,
			Messages:   append([]engine.Message(nil), sess.history...),
			Tools:      defaultACPTools(),
			SessionKey: "acp:" + sess.id,
			Headers:    headers.Clone(),
			Raw:        rawMap(msg.Params),
		}
		if turnSet {
			req.Turn = turn
			req.TurnSet = true
		}
		// Pin the scenario resolved on the first iteration so matchers that
		// depend on turn or probability cannot flip the script mid-turn.
		if pinned != "" {
			req.Headers.Set("X-FakeAgents-Scenario", pinned)
		}

		plan, err := a.engine.Generate(turnCtx, req)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// ACP requires the cancelled stop reason whenever the client
				// cancelled the turn, even mid-generation.
				return cancelResult(), nil
			}
			if apiErr, ok := err.(*apierr.Error); ok {
				return nil, toRPCError(apiErr)
			}
			return nil, rpcErrInternal(err.Error())
		}
		last = plan
		if pinned == "" && plan.Meta.Scenario != "" && plan.Meta.Scenario != "default" {
			pinned = plan.Meta.Scenario
		}

		emit := &emitter{agent: a, sess: sess, plan: plan, ctx: turnCtx}
		if !emit.sleep(plan.Latency.FirstToken) {
			return emit.endResult()
		}
		if emit.aborted() {
			return nil, nil
		}

		for _, block := range plan.Reasoning {
			if block.Redacted || block.Text == "" {
				continue
			}
			if done := emit.chunks(updateThoughtChunk, block.Text); done {
				return emit.endResult()
			}
		}
		if plan.Text != "" {
			if done := emit.chunks(updateMessageChunk, plan.Text); done {
				return emit.endResult()
			}
		}

		if len(plan.ToolCalls) > 0 {
			sess.history = append(sess.history, engine.Message{
				Role: engine.RoleAssistant, Content: plan.Text, ToolCalls: plan.ToolCalls,
			})
			for _, call := range plan.ToolCalls {
				resultText, outcome := a.execToolCall(turnCtx, emit, call)
				if emit.aborted() {
					return nil, nil
				}
				if emit.failed != nil {
					return emit.endResult()
				}
				if outcome == toolCancelled {
					return cancelResult(), nil
				}
				sess.history = append(sess.history, engine.Message{
					Role:       engine.RoleTool,
					ToolResult: true,
					ToolCallID: call.ID,
					ToolName:   call.Name,
					Content:    resultText,
				})
			}
			continue
		}

		if plan.Refusal != "" {
			if done := emit.chunks(updateMessageChunk, plan.Refusal); done {
				return emit.endResult()
			}
			sess.history = append(sess.history, engine.Message{Role: engine.RoleAssistant, Content: plan.Refusal})
		} else {
			sess.history = append(sess.history, engine.Message{Role: engine.RoleAssistant, Content: plan.Text})
		}
		if !plan.Stream.OmitUsage {
			a.emitUsage(sess, plan)
		}
		return a.promptResult(plan, finishStop(plan)), nil
	}
	return a.promptResult(last, stopMaxTurnRequests), nil
}

func finishStop(plan *engine.Plan) string {
	switch plan.Finish {
	case engine.FinishLength:
		return stopMaxTokens
	case engine.FinishRefusal, engine.FinishContentFilter:
		return stopRefusal
	default:
		return stopEndTurn
	}
}

// promptResult maps the final plan to the session/prompt response, attaching
// simulation metadata under _meta for observability.
func (a *Agent) promptResult(plan *engine.Plan, stop string) map[string]any {
	result := map[string]any{"stopReason": stop}
	if plan != nil {
		result["_meta"] = map[string]any{"fakeagents": map[string]any{
			"scenario": plan.Meta.Scenario,
			"model":    plan.Model,
			"turn":     plan.Meta.Turn,
			"step":     plan.Meta.Step,
			"usage": map[string]any{
				"inputTokens":  plan.Usage.InputTokens,
				"outputTokens": plan.Usage.OutputTokens,
			},
		}}
	}
	return result
}

func cancelResult() map[string]any {
	return map[string]any{"stopReason": stopCancelled}
}

// emitter streams session/update notifications for one engine iteration,
// honouring chunking, inter-chunk latency, and injected stream faults. Each
// notification counts as one "chunk" against fail_after_chunks and
// abort_after_chunks.
type emitter struct {
	agent   *Agent
	sess    *acpSession
	plan    *engine.Plan
	ctx     context.Context
	emitted int
	failed  *rpcError
}

// update writes one session/update notification and applies fault counters.
func (e *emitter) update(update map[string]any) bool {
	if e.ctx.Err() != nil {
		return false
	}
	if e.plan.Stream.AbortAfterChunks > 0 && e.emitted >= e.plan.Stream.AbortAfterChunks {
		e.agent.conn.Close()
		return false
	}
	if e.plan.Stream.Error != nil && e.plan.Stream.ErrorAfterChunks > 0 && e.emitted >= e.plan.Stream.ErrorAfterChunks {
		e.failed = toRPCError(e.plan.Stream.Error)
		return false
	}
	ok := e.agent.conn.notify(methodSessionUpdate, sessionUpdate{
		SessionID: e.sess.id,
		Update:    update,
	})
	if !ok {
		return false
	}
	e.emitted++
	if e.plan.Latency.Chunk > 0 {
		return e.sleep(e.plan.Latency.Chunk)
	}
	return true
}

// chunks emits a text body as chunked content updates sharing one messageId.
// It reports true when the turn must stop (stream fault, abort, or cancel);
// callers return e.endResult().
func (e *emitter) chunks(kind, text string) bool {
	parts := chunk.Split(text, e.plan.Latency.ChunkSize, e.plan.Latency.ChunkUnit)
	if len(parts) == 0 {
		return false
	}
	messageID := e.agent.ids.New("msg-")
	for _, part := range parts {
		if !e.update(map[string]any{
			"sessionUpdate": kind,
			"messageId":     messageID,
			"content":       map[string]any{"type": "text", "text": part},
		}) {
			return true
		}
	}
	return false
}

// endResult decides the prompt response after emission stopped early: stream
// fault error, nothing (transport aborted), or cancelled.
func (e *emitter) endResult() (any, *rpcError) {
	if e.failed != nil {
		return nil, e.failed
	}
	if e.aborted() {
		return nil, nil
	}
	return cancelResult(), nil
}

// aborted reports whether the whole connection is gone.
func (e *emitter) aborted() bool {
	return e.agent.conn.ctx.Err() != nil
}

func (e *emitter) sleep(d time.Duration) bool {
	if d <= 0 {
		return e.ctx.Err() == nil
	}
	return e.agent.clock.Sleep(e.ctx, d) == nil
}

// emitUsage reports cumulative context accounting via usage_update.
func (a *Agent) emitUsage(sess *acpSession, plan *engine.Plan) {
	used := plan.Usage.InputTokens + plan.Usage.CachedTokens + plan.Usage.CacheWriteTokens + plan.Usage.OutputTokens
	size := 200000
	for _, model := range a.cfg.Models {
		if strings.EqualFold(model.ID, plan.Model) && model.ContextWindow > 0 {
			size = model.ContextWindow
			break
		}
	}
	a.conn.notify(methodSessionUpdate, sessionUpdate{
		SessionID: sess.id,
		Update: map[string]any{
			"sessionUpdate": updateUsage,
			"used":          used,
			"size":          size,
		},
	})
}

// flattenPrompt reduces ACP content blocks to a single user text. Non-text
// blocks keep visible markers so templates and matchers still see them.
func flattenPrompt(blocks []contentBlock) string {
	var parts []string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, block.Text)
		case "resource_link":
			parts = append(parts, "["+block.Name+"]("+block.URI+")")
		case "resource":
			if block.Resource != nil && block.Resource.Text != "" {
				parts = append(parts, block.Resource.Text)
			} else {
				parts = append(parts, "[resource "+block.URI+"]")
			}
		default:
			if block.Type != "" {
				parts = append(parts, "["+block.Type+"]")
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// metaOverrides translates _meta.fakeagents.{scenario,step,turn}: scenario
// and step become the synthetic X-FakeAgents-* headers the engine reads,
// while turn maps directly onto Request.Turn (the header form only exists in
// the HTTP session tracker).
func metaOverrides(meta map[string]any) (http.Header, int, bool) {
	headers := http.Header{}
	fa, _ := meta["fakeagents"].(map[string]any)
	if fa == nil {
		return headers, 0, false
	}
	if name, ok := fa["scenario"].(string); ok && name != "" {
		headers.Set("X-FakeAgents-Scenario", name)
	}
	if value, ok := numberOf(fa["step"]); ok {
		headers.Set("X-FakeAgents-Step", value)
	}
	turn, turnSet := 0, false
	if value, ok := numberOf(fa["turn"]); ok {
		if n, err := strconv.Atoi(value); err == nil {
			turn, turnSet = n, true
		}
	}
	return headers, turn, turnSet
}

func numberOf(v any) (string, bool) {
	switch n := v.(type) {
	case float64:
		return strconv.Itoa(int(n)), true
	case string:
		if _, err := strconv.Atoi(n); err == nil {
			return n, true
		}
	}
	return "", false
}

func rawMap(raw json.RawMessage) map[string]any {
	var m map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

// defaultACPTools is the fixed tool catalog the simulated agent owns. ACP
// clients do not declare tools, so these back tool_index, tool_plan, and
// agentic scenarios with meaningful read/write/execute semantics.
func defaultACPTools() []engine.Tool {
	return []engine.Tool{
		{
			Name:        "read_file",
			Description: "Read a file from the workspace",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []string{"path"},
			},
		},
		{
			Name:        "write_file",
			Description: "Write content to a workspace file",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			Name:        "run_command",
			Description: "Run a shell command in the workspace",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"command": map[string]any{"type": "string"}},
				"required":   []string{"command"},
			},
		},
	}
}
