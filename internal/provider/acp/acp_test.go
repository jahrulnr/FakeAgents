package acp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jahrulnr/fakeagents/internal/clock"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/envelope"
	"github.com/jahrulnr/fakeagents/internal/id"
	"github.com/jahrulnr/fakeagents/internal/provider/acp"
	"github.com/jahrulnr/fakeagents/internal/scenario"
	"github.com/jahrulnr/fakeagents/internal/session"
	"github.com/jahrulnr/fakeagents/internal/template"
	"github.com/jahrulnr/fakeagents/internal/token"
)

// rpc is a decoded JSON-RPC frame exchanged with the agent.
type rpc struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

func (r *rpc) isRequest() bool      { return len(r.ID) > 0 && r.Method != "" }
func (r *rpc) isNotification() bool { return len(r.ID) == 0 && r.Method != "" }

var eofSentinel = &rpc{Error: &struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}{Code: -1, Message: "connection closed"}}

// client is a scripted ACP client. A single read goroutine demultiplexes
// frames: responses resolve per-id channels, notifications buffer globally,
// and agent->client requests are answered through onInvoke.
type client struct {
	toAgent  io.Writer
	nextID   atomic.Int64
	onInvoke func(c *client, msg *rpc)

	mu        sync.Mutex
	pending   map[string]chan *rpc
	notes     []*rpc
	unmatched chan *rpc
	readErr   error
}

func newClient(toAgent io.Writer, fromAgent io.Reader) *client {
	c := &client{
		toAgent:   toAgent,
		pending:   make(map[string]chan *rpc),
		unmatched: make(chan *rpc, 64),
		onInvoke:  func(*client, *rpc) {},
	}
	go c.readLoop(fromAgent)
	return c
}

func (c *client) readLoop(from io.Reader) {
	reader := bufio.NewReaderSize(from, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			for key, ch := range c.pending {
				delete(c.pending, key)
				ch <- eofSentinel
			}
			c.mu.Unlock()
			return
		}
		var msg rpc
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		switch {
		case msg.isRequest():
			c.onInvoke(c, &msg)
		case msg.isNotification():
			c.mu.Lock()
			c.notes = append(c.notes, &msg)
			c.mu.Unlock()
		default:
			key := string(msg.ID)
			c.mu.Lock()
			ch, ok := c.pending[key]
			c.mu.Unlock()
			if ok {
				ch <- &msg
			} else {
				select {
				case c.unmatched <- &msg:
				default:
				}
			}
		}
	}
}

func (c *client) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.toAgent.Write(append(data, '\n'))
	return err
}

func (c *client) sendRaw(s string) {
	_, _ = io.WriteString(c.toAgent, s+"\n")
}

// respond answers an agent->client request.
func (c *client) respond(id json.RawMessage, result any) {
	_ = c.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

// call sends a request and waits for its response, returning the notification
// frames received since the call started. Send failures and timeouts surface
// as a synthetic error frame so the helper is safe inside goroutines.
func (c *client) call(t *testing.T, method string, params any) (*rpc, []*rpc) {
	t.Helper()
	return c.callID(t, fmt.Sprintf("client-%d", c.nextID.Add(1)), method, params)
}

// callID is call with an explicit request id, needed when a test must
// reference the id later (e.g. $/cancel_request).
func (c *client) callID(t *testing.T, id string, method string, params any) (*rpc, []*rpc) {
	t.Helper()
	c.mu.Lock()
	base := len(c.notes)
	key, _ := json.Marshal(id) // response ids arrive as raw JSON
	ch := make(chan *rpc, 1)
	c.pending[string(key)] = ch
	c.mu.Unlock()

	fail := func(format string, args ...any) *rpc {
		return &rpc{Error: &struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}{Code: -2, Message: fmt.Sprintf(format, args...)}}
	}

	var msg *rpc
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		msg = fail("send %s: %v", method, err)
	} else {
		select {
		case msg = <-ch:
		case <-time.After(10 * time.Second):
			msg = fail("timeout waiting for %s response", method)
		}
	}
	c.mu.Lock()
	delete(c.pending, string(key))
	notes := append([]*rpc(nil), c.notes[base:]...)
	c.mu.Unlock()
	return msg, notes
}

func (c *client) notify(method string, params any) {
	_ = c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// nextUnmatched returns a response frame that matched no pending request.
func (c *client) nextUnmatched(t *testing.T) *rpc {
	t.Helper()
	select {
	case msg := <-c.unmatched:
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for unmatched frame")
		return nil
	}
}

type harness struct {
	client *client
	stdin  io.WriteCloser // client -> agent stream, closed to simulate EOF
	cancel context.CancelFunc
	done   chan error
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	cfg := config.Default()
	disabled := false
	cfg.Defaults.Latency.Enabled = &disabled
	cfg.Encryption.Key = "acp-test-key"
	cfg.Logging.Level = "error"
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	renderer, err := template.New(cfg.Templates, template.Builtins())
	if err != nil {
		t.Fatalf("templates: %v", err)
	}
	scenarios, err := scenario.Compile(cfg.Scenarios)
	if err != nil {
		t.Fatalf("scenarios: %v", err)
	}
	sealer, err := envelope.New(cfg.Encryption.Key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	// Concurrent prompt turns exercise the engine from multiple goroutines, so
	// the deterministic-but-single-goroutine id.Fake cannot be used here.
	ids := id.New()
	sim := engine.New(engine.Deps{
		Config:    cfg,
		Clock:     clk,
		IDs:       ids,
		Tokens:    token.New(),
		Templates: renderer,
		Scenarios: scenarios,
		Sessions:  session.New(clk),
		Sealer:    sealer,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	agent := acp.New(acp.Deps{
		Config:  cfg,
		Engine:  sim,
		Clock:   clk,
		IDs:     ids,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "test",
	})

	clientToAgentR, clientToAgentW := io.Pipe()
	agentToClientR, agentToClientW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx, clientToAgentR, agentToClientW) }()

	h := &harness{client: newClient(clientToAgentW, agentToClientR), stdin: clientToAgentW, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		_ = clientToAgentW.Close()
		agentToClientR.Close()
	})
	return h
}

func (h *harness) initialize(t *testing.T, caps map[string]any) {
	t.Helper()
	res, _ := h.client.call(t, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": caps,
		"clientInfo":         map[string]any{"name": "test-client", "version": "0"},
	})
	if res.Error != nil {
		t.Fatalf("initialize error: %+v", res.Error)
	}
}

func (h *harness) newSession(t *testing.T) string {
	t.Helper()
	res, _ := h.client.call(t, "session/new", map[string]any{"cwd": "/tmp/work", "mcpServers": []any{}})
	if res.Error != nil {
		t.Fatalf("session/new error: %+v", res.Error)
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res.Result, &out); err != nil || out.SessionID == "" {
		t.Fatalf("session/new result: %s", res.Result)
	}
	return out.SessionID
}

func (h *harness) prompt(t *testing.T, sessionID, text string, meta map[string]any) (*rpc, []*rpc) {
	t.Helper()
	params := map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": text}},
	}
	if meta != nil {
		params["_meta"] = meta
	}
	return h.client.call(t, "session/prompt", params)
}

func mustInit(t *testing.T, mutate func(*config.Config)) (*harness, string) {
	t.Helper()
	h := newHarness(t, mutate)
	h.initialize(t, map[string]any{})
	return h, h.newSession(t)
}

func updatesOf(notes []*rpc) []map[string]any {
	var out []map[string]any
	for _, n := range notes {
		if n.Method != "session/update" {
			continue
		}
		var p struct {
			Update map[string]any `json:"update"`
		}
		_ = json.Unmarshal(n.Params, &p)
		out = append(out, p.Update)
	}
	return out
}

func kindsOf(updates []map[string]any) []string {
	var kinds []string
	for _, u := range updates {
		if k, ok := u["sessionUpdate"].(string); ok {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

func containsKind(kinds []string, want string) bool {
	return slices.Contains(kinds, want)
}

func TestInitializeCapabilities(t *testing.T) {
	h := newHarness(t, nil)
	h.initialize(t, map[string]any{"fs": map[string]any{"readTextFile": true, "writeTextFile": true}, "terminal": true})
}

func TestPreInitializeRejected(t *testing.T) {
	h := newHarness(t, nil)
	res, _ := h.client.call(t, "session/new", map[string]any{"cwd": "/tmp"})
	if res.Error == nil || res.Error.Code != -32600 {
		t.Fatalf("expected -32600, got %+v", res.Error)
	}
}

func TestUnknownMethodParseErrorBatch(t *testing.T) {
	h := newHarness(t, nil)
	h.initialize(t, map[string]any{})

	res, _ := h.client.call(t, "bogus/method", map[string]any{})
	if res.Error == nil || res.Error.Code != -32601 {
		t.Fatalf("expected -32601, got %+v", res.Error)
	}

	h.client.sendRaw("{not json")
	msg := h.client.nextUnmatched(t)
	if msg.Error == nil || msg.Error.Code != -32700 {
		t.Fatalf("expected -32700, got %+v", msg.Error)
	}

	h.client.sendRaw(`[{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}]`)
	msg = h.client.nextUnmatched(t)
	if msg.Error == nil || msg.Error.Code != -32600 {
		t.Fatalf("expected batch -32600, got %+v", msg.Error)
	}
}

func TestIDTypesEcho(t *testing.T) {
	h := newHarness(t, nil)
	h.client.sendRaw(`{"jsonrpc":"2.0","id":42,"method":"initialize","params":{"protocolVersion":1}}`)
	msg := h.client.nextUnmatched(t)
	if string(msg.ID) != "42" {
		t.Fatalf("numeric id echo: %s", msg.ID)
	}
	h.client.sendRaw(`{"jsonrpc":"2.0","id":"sess-1","method":"bogus","params":{}}`)
	msg = h.client.nextUnmatched(t)
	if string(msg.ID) != `"sess-1"` || msg.Error == nil || msg.Error.Code != -32601 {
		t.Fatalf("string id echo: %+v", msg)
	}
}

func TestPromptTextAndUsage(t *testing.T) {
	h, sid := mustInit(t, nil)
	res, notes := h.prompt(t, sid, "hello there", nil)
	if res.Error != nil {
		t.Fatalf("prompt error: %+v", res.Error)
	}
	var out struct {
		StopReason string         `json:"stopReason"`
		Meta       map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		t.Fatalf("result: %v", err)
	}
	if out.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q", out.StopReason)
	}
	kinds := kindsOf(updatesOf(notes))
	if !containsKind(kinds, "agent_message_chunk") {
		t.Fatalf("missing message chunk, kinds=%v", kinds)
	}
	if !containsKind(kinds, "usage_update") {
		t.Fatalf("missing usage_update, kinds=%v", kinds)
	}
	if out.Meta["fakeagents"] == nil {
		t.Fatalf("missing _meta.fakeagents: %s", res.Result)
	}
}

func TestPromptThinkingScenario(t *testing.T) {
	h, sid := mustInit(t, nil)
	_, notes := h.prompt(t, sid, "think [[fakeagents:thinking]]", nil)
	if !containsKind(kindsOf(updatesOf(notes)), "agent_thought_chunk") {
		t.Fatalf("missing thought chunk")
	}
}

func TestPromptMetaScenarioHeader(t *testing.T) {
	h, sid := mustInit(t, nil)
	res, notes := h.prompt(t, sid, "anything", map[string]any{"fakeagents": map[string]any{"scenario": "thinking"}})
	if !containsKind(kindsOf(updatesOf(notes)), "agent_thought_chunk") {
		t.Fatalf("meta scenario not applied")
	}
	var out struct {
		Meta map[string]any `json:"_meta"`
	}
	_ = json.Unmarshal(res.Result, &out)
	fa := out.Meta["fakeagents"].(map[string]any)
	if fa["scenario"] != "thinking" {
		t.Fatalf("scenario = %v", fa["scenario"])
	}
}

func TestToolCallLifecycleInOneTurn(t *testing.T) {
	h, sid := mustInit(t, func(c *config.Config) { c.Providers.ACP.Permission = "never" })
	res, notes := h.prompt(t, sid, "use a tool", map[string]any{"fakeagents": map[string]any{"scenario": "tool-call"}})
	var out struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(res.Result, &out)
	if out.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q", out.StopReason)
	}
	updates := updatesOf(notes)
	var sawCall bool
	statuses := []string{}
	for _, u := range updates {
		switch u["sessionUpdate"] {
		case "tool_call":
			sawCall = u["toolCallId"] != nil
		case "tool_call_update":
			if s, ok := u["status"].(string); ok {
				statuses = append(statuses, s)
			}
		}
	}
	if !sawCall {
		t.Fatalf("missing tool_call update: %v", updates)
	}
	if len(statuses) == 0 || statuses[len(statuses)-1] != "completed" {
		t.Fatalf("tool statuses = %v", statuses)
	}
	if !containsKind(kindsOf(updates), "agent_message_chunk") {
		t.Fatalf("missing final message")
	}
}

func TestToolErrorScript(t *testing.T) {
	h, sid := mustInit(t, func(c *config.Config) { c.Providers.ACP.Permission = "never" })
	_, notes := h.prompt(t, sid, "test tool error", nil)
	updates := updatesOf(notes)
	failed, calls := 0, 0
	for _, u := range updates {
		if u["sessionUpdate"] == "tool_call" {
			calls++
		}
		if u["sessionUpdate"] == "tool_call_update" && u["status"] == "failed" {
			failed++
		}
	}
	if calls != 2 || failed != 1 {
		t.Fatalf("expected 2 calls/1 failed, got %d/%d", calls, failed)
	}
}

func TestErrorScenarioMapsToRPCError(t *testing.T) {
	h, sid := mustInit(t, nil)
	res, _ := h.prompt(t, sid, "charge me", map[string]any{"fakeagents": map[string]any{"scenario": "billing"}})
	if res.Error == nil {
		t.Fatalf("expected error, got %s", res.Result)
	}
	if res.Error.Code != -32603 {
		t.Fatalf("code = %d", res.Error.Code)
	}
	var data map[string]any
	_ = json.Unmarshal(res.Error.Data, &data)
	if data["status"].(float64) != 429 {
		t.Fatalf("error data = %s", res.Error.Data)
	}
}

func TestFailAfterChunksReturnsError(t *testing.T) {
	h, sid := mustInit(t, nil)
	res, notes := h.prompt(t, sid, "go", map[string]any{"fakeagents": map[string]any{"scenario": "mid-stream-error"}})
	if res.Error == nil || res.Error.Code != -32603 {
		t.Fatalf("expected -32603 mid-turn error, got %+v", res.Error)
	}
	if got := len(updatesOf(notes)); got != 3 {
		t.Fatalf("expected 3 updates before failure, got %d", got)
	}
}

func TestAbortAfterChunksClosesConnection(t *testing.T) {
	h, sid := mustInit(t, nil)
	res, notes := h.prompt(t, sid, "go", map[string]any{"fakeagents": map[string]any{"scenario": "mid-stream-abort"}})
	if res != eofSentinel {
		t.Fatalf("expected connection close sentinel, got %+v", res)
	}
	if got := len(updatesOf(notes)); got != 2 {
		t.Fatalf("expected 2 updates before abort, got %d", got)
	}
}

func TestUnknownSession(t *testing.T) {
	h := newHarness(t, nil)
	h.initialize(t, map[string]any{})
	res, _ := h.client.call(t, "session/prompt", map[string]any{
		"sessionId": "nope",
		"prompt":    []any{map[string]any{"type": "text", "text": "hi"}},
	})
	if res.Error == nil || res.Error.Code != -32002 {
		t.Fatalf("expected -32002, got %+v", res.Error)
	}
}

func TestSessionCloseThenPrompt(t *testing.T) {
	h, sid := mustInit(t, nil)
	res, _ := h.client.call(t, "session/close", map[string]any{"sessionId": sid})
	if res.Error != nil {
		t.Fatalf("close: %+v", res.Error)
	}
	res2, _ := h.client.call(t, "session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "hi"}},
	})
	if res2.Error == nil || res2.Error.Code != -32002 {
		t.Fatalf("expected -32002 after close, got %+v", res2.Error)
	}
}

func TestPermissionAllowAndReject(t *testing.T) {
	for _, tc := range []struct {
		name       string
		option     string
		wantStatus string
	}{
		{"allow once", "allow-once", "completed"},
		{"reject once", "reject-once", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(c *config.Config) { c.Providers.ACP.Permission = "always" })
			h.initialize(t, map[string]any{})
			sid := h.newSession(t)

			permissionSeen := make(chan json.RawMessage, 1)
			h.client.onInvoke = func(c *client, msg *rpc) {
				if msg.Method == "session/request_permission" {
					permissionSeen <- msg.ID
				}
			}

			done := make(chan *rpc, 1)
			go func() {
				res, _ := h.client.call(t, "session/prompt", map[string]any{
					"sessionId": sid,
					"prompt":    []any{map[string]any{"type": "text", "text": "tool please"}},
					"_meta":     map[string]any{"fakeagents": map[string]any{"scenario": "tool-call"}},
				})
				done <- res
			}()

			var permID json.RawMessage
			select {
			case permID = <-permissionSeen:
			case <-time.After(5 * time.Second):
				t.Fatal("no permission request received")
			}
			h.client.respond(permID, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": tc.option}})

			out := <-done
			h.client.mu.Lock()
			var statuses []string
			for _, u := range updatesOf(h.client.notes) {
				if u["sessionUpdate"] == "tool_call_update" {
					if s, ok := u["status"].(string); ok {
						statuses = append(statuses, s)
					}
				}
			}
			h.client.mu.Unlock()
			if len(statuses) == 0 || statuses[len(statuses)-1] != tc.wantStatus {
				t.Fatalf("statuses = %v", statuses)
			}
			if out.Error != nil {
				t.Fatalf("prompt error: %+v", out.Error)
			}
		})
	}
}

func TestCancelDuringPermission(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Providers.ACP.Permission = "always" })
	h.initialize(t, map[string]any{})
	sid := h.newSession(t)

	permissionSeen := make(chan json.RawMessage, 1)
	h.client.onInvoke = func(c *client, msg *rpc) {
		if msg.Method == "session/request_permission" {
			permissionSeen <- msg.ID
		}
	}

	done := make(chan *rpc, 1)
	go func() {
		res, _ := h.client.call(t, "session/prompt", map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "tool please"}},
			"_meta":     map[string]any{"fakeagents": map[string]any{"scenario": "tool-call"}},
		})
		done <- res
	}()

	var permID json.RawMessage
	select {
	case permID = <-permissionSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("no permission request received")
	}

	// Client cancels the turn, then answers the pending permission request
	// with the mandated cancelled outcome.
	h.client.notify("session/cancel", map[string]any{"sessionId": sid})
	h.client.respond(permID, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})

	out := <-done
	var parsed struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(out.Result, &parsed)
	if parsed.StopReason != "cancelled" {
		t.Fatalf("stopReason = %q (result=%s)", parsed.StopReason, out.Result)
	}
}

func TestFSAndTerminalDelegation(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Providers.ACP.Permission = "never" })
	h.initialize(t, map[string]any{
		"fs":       map[string]any{"readTextFile": true, "writeTextFile": true},
		"terminal": true,
	})
	sid := h.newSession(t)

	var mu sync.Mutex
	seen := map[string]int{}
	h.client.onInvoke = func(c *client, msg *rpc) {
		mu.Lock()
		seen[msg.Method]++
		mu.Unlock()
		switch msg.Method {
		case "fs/read_text_file":
			c.respond(msg.ID, map[string]any{"content": "file body from client"})
		case "terminal/create":
			c.respond(msg.ID, map[string]any{"terminalId": "term-1"})
		case "terminal/wait_for_exit":
			c.respond(msg.ID, map[string]any{"exitCode": 0})
		case "terminal/output":
			c.respond(msg.ID, map[string]any{"output": "command output", "truncated": false})
		default:
			c.respond(msg.ID, map[string]any{})
		}
	}

	res, _ := h.prompt(t, sid, "read something", map[string]any{"fakeagents": map[string]any{"scenario": "tool-call"}})
	if res.Error != nil {
		t.Fatalf("prompt: %+v", res.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["fs/read_text_file"] == 0 {
		t.Fatalf("expected fs/read_text_file, seen=%v", seen)
	}
}

func TestExecuteDelegation(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Providers.ACP.Permission = "never"
		c.Scenarios = append(c.Scenarios, scenario.Spec{
			Name: "run-cmd",
			Respond: scenario.Respond{Steps: []scenario.Respond{
				{ToolCalls: []scenario.ToolCallSpec{{Name: "run_command", Arguments: map[string]any{"command": "ls"}}}},
				{Text: "done running"},
			}},
		})
	})
	h.initialize(t, map[string]any{"terminal": true})
	sid := h.newSession(t)

	created := make(chan struct{}, 1)
	h.client.onInvoke = func(c *client, msg *rpc) {
		switch msg.Method {
		case "terminal/create":
			created <- struct{}{}
			c.respond(msg.ID, map[string]any{"terminalId": "term-9"})
		case "terminal/wait_for_exit":
			c.respond(msg.ID, map[string]any{"exitCode": 0})
		case "terminal/output":
			c.respond(msg.ID, map[string]any{"output": "ok", "truncated": false})
		default:
			c.respond(msg.ID, map[string]any{})
		}
	}

	res, _ := h.prompt(t, sid, "run", map[string]any{"fakeagents": map[string]any{"scenario": "run-cmd"}})
	if res.Error != nil {
		t.Fatalf("prompt: %+v", res.Error)
	}
	select {
	case <-created:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal/create not called")
	}
}

func TestAdvertisedCapabilities(t *testing.T) {
	h := newHarness(t, nil)
	res, _ := h.client.call(t, "initialize", map[string]any{"protocolVersion": 1})
	var out struct {
		AgentCapabilities struct {
			SessionCapabilities map[string]any `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		t.Fatalf("result: %v", err)
	}
	caps := out.AgentCapabilities.SessionCapabilities
	if _, ok := caps["close"]; !ok {
		t.Fatalf("sessionCapabilities missing close: %v", caps)
	}
	if _, ok := caps["delete"]; !ok {
		t.Fatalf("sessionCapabilities missing delete: %v", caps)
	}
}

func TestSessionCloseAndDeleteUnknown(t *testing.T) {
	h := newHarness(t, nil)
	h.initialize(t, map[string]any{})
	for _, method := range []string{"session/close", "session/delete"} {
		res, _ := h.client.call(t, method, map[string]any{"sessionId": "ghost"})
		if res.Error == nil || res.Error.Code != -32002 {
			t.Fatalf("%s: expected -32002, got %+v", method, res.Error)
		}
	}
}

// TestSameSessionSerialized proves the per-session turn gate queues a second
// prompt behind an in-flight turn instead of interleaving.
func TestSameSessionSerialized(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Providers.ACP.Permission = "always" })
	h.initialize(t, map[string]any{})
	sid := h.newSession(t)

	permissionSeen := make(chan json.RawMessage, 2)
	h.client.onInvoke = func(c *client, msg *rpc) {
		if msg.Method == "session/request_permission" {
			permissionSeen <- msg.ID
		}
	}
	promptParams := map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "tool please"}},
		"_meta":     map[string]any{"fakeagents": map[string]any{"scenario": "tool-call"}},
	}
	first := make(chan *rpc, 1)
	go func() { res, _ := h.client.call(t, "session/prompt", promptParams); first <- res }()

	var permID json.RawMessage
	select {
	case permID = <-permissionSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("first prompt did not reach permission")
	}

	second := make(chan *rpc, 1)
	go func() { res, _ := h.client.call(t, "session/prompt", promptParams); second <- res }()

	// The second prompt must stay queued while the first holds the gate.
	select {
	case res := <-second:
		t.Fatalf("queued prompt responded while gate held: %+v", res)
	case <-time.After(200 * time.Millisecond):
	}

	h.client.respond(permID, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow-once"}})

	// First turn completes, then the queued turn needs its own permission.
	if res := <-first; res.Error != nil {
		t.Fatalf("first prompt: %+v", res.Error)
	}
	select {
	case permID = <-permissionSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("queued prompt never ran")
	}
	h.client.respond(permID, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow-once"}})
	if res := <-second; res.Error != nil {
		t.Fatalf("queued prompt: %+v", res.Error)
	}
}

// TestCancelDuringDelegation cancels while the agent waits on a delegated
// fs/read_text_file; the next engine iteration must see the cancelled context
// and map it to the cancelled stop reason.
func TestCancelDuringDelegation(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Providers.ACP.Permission = "never" })
	h.initialize(t, map[string]any{"fs": map[string]any{"readTextFile": true}})
	sid := h.newSession(t)

	fsSeen := make(chan json.RawMessage, 1)
	h.client.onInvoke = func(c *client, msg *rpc) {
		if msg.Method == "fs/read_text_file" {
			fsSeen <- msg.ID
		}
	}
	done := make(chan *rpc, 1)
	go func() {
		res, _ := h.client.call(t, "session/prompt", map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "read"}},
			"_meta":     map[string]any{"fakeagents": map[string]any{"scenario": "tool-call"}},
		})
		done <- res
	}()

	var fsID json.RawMessage
	select {
	case fsID = <-fsSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("fs/read_text_file not requested")
	}
	h.client.notify("session/cancel", map[string]any{"sessionId": sid})
	h.client.respond(fsID, map[string]any{"content": "data"})

	res := <-done
	var out struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(res.Result, &out)
	if out.StopReason != "cancelled" {
		t.Fatalf("stopReason = %q (error=%v)", out.StopReason, res.Error)
	}
}

// TestSessionCloseDuringTurn cancels the running turn and removes the
// session; subsequent prompts hit resource-not-found.
func TestSessionCloseDuringTurn(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Providers.ACP.Permission = "never" })
	h.initialize(t, map[string]any{"fs": map[string]any{"readTextFile": true}})
	sid := h.newSession(t)

	fsSeen := make(chan json.RawMessage, 1)
	h.client.onInvoke = func(c *client, msg *rpc) {
		if msg.Method == "fs/read_text_file" {
			fsSeen <- msg.ID
		}
	}
	done := make(chan *rpc, 1)
	go func() {
		res, _ := h.client.call(t, "session/prompt", map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "read"}},
			"_meta":     map[string]any{"fakeagents": map[string]any{"scenario": "tool-call"}},
		})
		done <- res
	}()

	var fsID json.RawMessage
	select {
	case fsID = <-fsSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("fs/read_text_file not requested")
	}

	closeRes, _ := h.client.call(t, "session/close", map[string]any{"sessionId": sid})
	if closeRes.Error != nil {
		t.Fatalf("close: %+v", closeRes.Error)
	}
	h.client.respond(fsID, map[string]any{"content": "data"})

	res := <-done
	var out struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(res.Result, &out)
	if out.StopReason != "cancelled" {
		t.Fatalf("stopReason = %q", out.StopReason)
	}
	after, _ := h.client.call(t, "session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "hi"}},
	})
	if after.Error == nil || after.Error.Code != -32002 {
		t.Fatalf("expected -32002, got %+v", after.Error)
	}
}

// TestCancelRequestForQueuedPrompt verifies $/cancel_request reaches prompts
// still queued behind the per-session turn gate.
func TestCancelRequestForQueuedPrompt(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Providers.ACP.Permission = "always" })
	h.initialize(t, map[string]any{})
	sid := h.newSession(t)

	permissionSeen := make(chan json.RawMessage, 2)
	h.client.onInvoke = func(c *client, msg *rpc) {
		if msg.Method == "session/request_permission" {
			permissionSeen <- msg.ID
		}
	}
	params := map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "tool"}},
		"_meta":     map[string]any{"fakeagents": map[string]any{"scenario": "tool-call"}},
	}
	first := make(chan *rpc, 1)
	go func() { res, _ := h.client.call(t, "session/prompt", params); first <- res }()
	var permID json.RawMessage
	select {
	case permID = <-permissionSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("first prompt did not reach permission")
	}

	queued := make(chan *rpc, 1)
	go func() { res, _ := h.client.callID(t, "queued-1", "session/prompt", params); queued <- res }()
	time.Sleep(100 * time.Millisecond) // let the queued prompt register

	h.client.notify("$/cancel_request", map[string]any{"id": "queued-1"})
	res := <-queued
	var out struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(res.Result, &out)
	if out.StopReason != "cancelled" {
		t.Fatalf("queued cancel: stopReason = %q (err=%v)", out.StopReason, res.Error)
	}

	h.client.respond(permID, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow-once"}})
	if res := <-first; res.Error != nil {
		t.Fatalf("first prompt: %+v", res.Error)
	}
}

func TestOversizedFrameRejected(t *testing.T) {
	h := newHarness(t, nil)
	h.initialize(t, map[string]any{})
	h.client.sendRaw(`{"jsonrpc":"2.0","id":7,"method":"bogus","params":"` + strings.Repeat("x", 33<<20) + `"}`)
	msg := h.client.nextUnmatched(t)
	if msg.Error == nil || msg.Error.Code != -32600 {
		t.Fatalf("expected -32600, got %+v", msg.Error)
	}
	// Connection must survive and keep answering.
	res, _ := h.client.call(t, "session/new", map[string]any{"cwd": "/tmp"})
	if res.Error != nil {
		t.Fatalf("post-oversize call failed: %+v", res.Error)
	}
}

// TestUnterminatedFinalFrame: a request cut off by EOF still gets a response
// before the connection tears down.
func TestUnterminatedFinalFrame(t *testing.T) {
	h := newHarness(t, nil)
	// No newline at the end, then EOF.
	_, _ = io.WriteString(h.stdin, `{"jsonrpc":"2.0","id":99,"method":"initialize","params":{"protocolVersion":1}}`)
	_ = h.stdin.Close()
	msg := h.client.nextUnmatched(t)
	if msg.Error != nil || string(msg.ID) != "99" {
		t.Fatalf("expected result for id 99, got %+v", msg)
	}
}

// TestEditDelegation exercises the fs/write_text_file path including the
// old-content read used for diff output.
func TestEditDelegation(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Providers.ACP.Permission = "never"
		c.Scenarios = append(c.Scenarios, scenario.Spec{
			Name: "edit-file",
			Respond: scenario.Respond{Steps: []scenario.Respond{
				{ToolCalls: []scenario.ToolCallSpec{{Name: "write_file", Arguments: map[string]any{
					"path": "notes.txt", "content": "new body",
				}}}},
				{Text: "edited"},
			}},
		})
	})
	h.initialize(t, map[string]any{"fs": map[string]any{"readTextFile": true, "writeTextFile": true}})
	sid := h.newSession(t)

	var mu sync.Mutex
	var wrotePath string
	h.client.onInvoke = func(c *client, msg *rpc) {
		switch msg.Method {
		case "fs/read_text_file":
			c.respond(msg.ID, map[string]any{"content": "old body"})
		case "fs/write_text_file":
			var p struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			mu.Lock()
			wrotePath = p.Path
			mu.Unlock()
			c.respond(msg.ID, map[string]any{})
		default:
			c.respond(msg.ID, map[string]any{})
		}
	}

	res, notes := h.prompt(t, sid, "edit it", map[string]any{"fakeagents": map[string]any{"scenario": "edit-file"}})
	if res.Error != nil {
		t.Fatalf("prompt: %+v", res.Error)
	}
	mu.Lock()
	if !strings.HasSuffix(wrotePath, "/notes.txt") || !strings.HasPrefix(wrotePath, "/") {
		t.Fatalf("delegated path not absolute: %q", wrotePath)
	}
	mu.Unlock()
	var sawDiff bool
	for _, u := range updatesOf(notes) {
		if u["sessionUpdate"] != "tool_call_update" {
			continue
		}
		content, _ := u["content"].([]any)
		for _, item := range content {
			if m, ok := item.(map[string]any); ok && m["type"] == "diff" {
				sawDiff = m["oldText"] == "old body"
			}
		}
	}
	if !sawDiff {
		t.Fatal("no diff content with read-back oldText")
	}
}

// TestAllowAlwaysPersisted remembers an allow_always decision for the rest of
// the session: the next same-kind call runs without another prompt.
func TestAllowAlwaysPersisted(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Providers.ACP.Permission = "always" })
	h.initialize(t, map[string]any{})
	sid := h.newSession(t)

	var mu sync.Mutex
	permCount := 0
	h.client.onInvoke = func(c *client, msg *rpc) {
		if msg.Method == "session/request_permission" {
			mu.Lock()
			permCount++
			mu.Unlock()
			c.respond(msg.ID, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow-always"}})
			return
		}
		c.respond(msg.ID, map[string]any{})
	}
	params := map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": "tool"}},
		"_meta":     map[string]any{"fakeagents": map[string]any{"scenario": "tool-call"}},
	}
	if res, _ := h.client.call(t, "session/prompt", params); res.Error != nil {
		t.Fatalf("first prompt: %+v", res.Error)
	}
	if res, _ := h.client.call(t, "session/prompt", params); res.Error != nil {
		t.Fatalf("second prompt: %+v", res.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if permCount != 1 {
		t.Fatalf("permission requests = %d, want 1 (remembered)", permCount)
	}
}

// TestEOFDuringPendingClientCall reproduces the reviewed deadlock: the client
// closes stdin while the agent waits on session/request_permission. Run must
// fail the pending call, let the handler unwind, and return.
func TestEOFDuringPendingClientCall(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Providers.ACP.Permission = "always" })
	h.initialize(t, map[string]any{})
	sid := h.newSession(t)

	permissionSeen := make(chan struct{}, 1)
	h.client.onInvoke = func(c *client, msg *rpc) {
		if msg.Method == "session/request_permission" {
			permissionSeen <- struct{}{}
		}
	}
	go func() {
		h.client.callID(t, "turn-1", "session/prompt", map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "tool"}},
			"_meta":     map[string]any{"fakeagents": map[string]any{"scenario": "tool-call"}},
		})
	}()

	select {
	case <-permissionSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("permission request never arrived")
	}

	// Client vanishes mid-permission without answering.
	_ = h.stdin.Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent hung after client EOF during pending permission")
	}
}

// TestRunReturnsOnContextCancel covers the SIGTERM path: cancelling the Run
// context while the agent idles on stdin must unblock the read loop.
func TestRunReturnsOnContextCancel(t *testing.T) {
	h := newHarness(t, nil)
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestConcurrentPrompts exercises parallel turns in separate sessions.
func TestConcurrentPrompts(t *testing.T) {
	h, sid1 := mustInit(t, nil)
	sid2 := h.newSession(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, sid := range []string{sid1, sid2} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			res, _ := h.client.call(t, "session/prompt", map[string]any{
				"sessionId": id,
				"prompt":    []any{map[string]any{"type": "text", "text": "parallel"}},
			})
			if res.Error != nil {
				errs <- fmt.Errorf("prompt %s: %+v", id, res.Error)
			}
		}(sid)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
