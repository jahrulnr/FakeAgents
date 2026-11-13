package acp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"

	"github.com/jahrulnr/fakeagents/internal/clock"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/id"
)

// Deps are the agent's explicit dependencies, mirroring the provider adapters.
type Deps struct {
	Config  *config.Config
	Engine  *engine.Engine
	Clock   clock.Clock
	IDs     id.Generator
	Logger  *slog.Logger
	Version string
}

// Agent is a simulated ACP agent. It speaks JSON-RPC over NDJSON and drives
// the simulation engine inside prompt turns.
type Agent struct {
	cfg     *config.Config
	engine  *engine.Engine
	clock   clock.Clock
	ids     id.Generator
	logger  *slog.Logger
	version string

	conn *conn

	mu       sync.Mutex
	inited   bool
	client   clientCaps
	rooms    map[string]*acpSession
	cancels  map[string]context.CancelFunc // sessionID -> in-flight turn cancel
	requests map[string]context.CancelFunc // JSON-RPC request id -> cancel ($/cancel_request)
}

// acpSession is one ACP conversation. The turn gate serialises prompt turns
// inside the session; parallel prompts in different sessions stay concurrent.
type acpSession struct {
	id      string
	cwd     string
	history []engine.Message
	// dirs accepts additionalDirectories per the advertised capability; the
	// simulator does not interpret them.
	dirs       []string
	allowRules map[string]bool // permission kind -> remembered always decisions
	turn       chan struct{}   // one-slot turn gate
}

// New builds an ACP agent.
func New(deps Deps) *Agent {
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
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Agent{
		cfg:      deps.Config,
		engine:   deps.Engine,
		clock:    clk,
		ids:      ids,
		logger:   logger,
		version:  deps.Version,
		rooms:    make(map[string]*acpSession),
		cancels:  make(map[string]context.CancelFunc),
		requests: make(map[string]context.CancelFunc),
	}
}

// Run serves the protocol over in/out until the client disconnects, ctx ends,
// or a simulated transport abort closes the connection. Closable streams are
// closed on return so the peer observes EOF.
func (a *Agent) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	a.conn = newConn(ctx, out, a.handle, a.logger)
	defer func() {
		a.conn.Close()
		if closer, ok := out.(io.Closer); ok {
			_ = closer.Close()
		}
	}()
	return a.conn.run(in)
}

// handle is the conn dispatch callback. Requests get responses; notifications
// never do.
func (a *Agent) handle(ctx context.Context, msg *rpcMessage) {
	if msg.isRequest() {
		result, rpcErr := a.dispatch(ctx, msg)
		a.conn.write(response(msg.ID, result, rpcErr))
		return
	}
	a.notify(ctx, msg)
}

// dispatch routes inbound requests. Anything the agent does not implement —
// including capability-gated methods it never advertised — gets -32601.
func (a *Agent) dispatch(ctx context.Context, msg *rpcMessage) (any, *rpcError) {
	if !a.initialized() && msg.Method != methodInitialize {
		return nil, rpcErr(codeInvalidRequest, "initialize must be the first request", nil)
	}
	switch msg.Method {
	case methodInitialize:
		return a.onInitialize(msg.Params)
	case methodAuthenticate, methodLogout:
		// No authentication is configured; probing clients get a no-op success.
		return nil, nil
	case methodSessionNew:
		return a.onSessionNew(msg.Params)
	case methodSessionPrompt:
		return a.onPrompt(ctx, msg)
	case methodSessionClose:
		return a.onSessionClose(msg.Params)
	case methodSessionDelete:
		return a.onSessionDelete(msg.Params)
	default:
		return nil, rpcErr(codeMethodNotFound, "method not implemented: "+msg.Method, nil)
	}
}

// notify routes inbound notifications.
func (a *Agent) notify(_ context.Context, msg *rpcMessage) {
	switch msg.Method {
	case methodSessionCancel:
		var params sessionIDParams
		if err := json.Unmarshal(msg.Params, &params); err == nil {
			a.cancelSession(params.SessionID)
		}
	case methodCancelRequest:
		var params cancelRequestParams
		if err := json.Unmarshal(msg.Params, &params); err == nil && len(params.ID) > 0 {
			a.mu.Lock()
			cancel := a.requests[string(params.ID)]
			a.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	default:
		a.logger.Debug("acp: ignoring notification", "method", msg.Method)
	}
}

func (a *Agent) initialized() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inited
}

// capabilities returns the negotiated client capabilities.
func (a *Agent) capabilities() clientCaps {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

func (a *Agent) onInitialize(params json.RawMessage) (any, *rpcError) {
	var p initializeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, rpcErr(codeInvalidParams, "invalid initialize params: "+err.Error(), nil)
	}
	a.mu.Lock()
	a.inited = true
	a.client = clientCaps{
		fsRead:  p.ClientCapabilities.FS.ReadTextFile,
		fsWrite: p.ClientCapabilities.FS.WriteTextFile,
		term:    p.ClientCapabilities.Terminal,
	}
	a.mu.Unlock()
	title := a.cfg.Providers.ACP.Title
	if title == "" {
		title = "FakeAgents"
	}
	version := a.version
	if version == "" {
		version = "dev"
	}
	return map[string]any{
		"protocolVersion": protocolVersion,
		"agentInfo":       implementation{Name: "fakeagents", Title: title, Version: version},
		"agentCapabilities": map[string]any{
			"loadSession": false,
			"promptCapabilities": map[string]any{
				"image":           false,
				"audio":           false,
				"embeddedContext": true,
			},
			"mcpCapabilities": map[string]any{"http": false, "sse": false},
			"sessionCapabilities": map[string]any{
				"additionalDirectories": map[string]any{},
				"close":                 map[string]any{},
				"delete":                map[string]any{},
			},
			"auth": map[string]any{},
		},
		"authMethods": []any{},
	}, nil
}

func (a *Agent) onSessionNew(params json.RawMessage) (any, *rpcError) {
	var p sessionNewParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, rpcErr(codeInvalidParams, "invalid session/new params: "+err.Error(), nil)
	}
	if p.Cwd == "" {
		return nil, rpcErr(codeInvalidParams, "session/new requires cwd", nil)
	}
	sess := &acpSession{
		id:         a.ids.New("acp-"),
		cwd:        p.Cwd,
		dirs:       append([]string(nil), p.AdditionalDirectories...),
		allowRules: make(map[string]bool),
		turn:       make(chan struct{}, 1),
	}
	a.mu.Lock()
	a.rooms[sess.id] = sess
	a.mu.Unlock()
	a.logger.Info("acp: session created", "session", sess.id, "cwd", sess.cwd)
	return map[string]any{"sessionId": sess.id}, nil
}

// lookup returns an active session or the ACP not-found error.
func (a *Agent) lookup(sessionID string) (*acpSession, *rpcError) {
	a.mu.Lock()
	sess, ok := a.rooms[sessionID]
	a.mu.Unlock()
	if !ok {
		return nil, sessionNotFound(sessionID)
	}
	return sess, nil
}

// cancelSession cancels the in-flight prompt turn of a session, if any.
func (a *Agent) cancelSession(sessionID string) {
	a.mu.Lock()
	cancel := a.cancels[sessionID]
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *Agent) onSessionClose(params json.RawMessage) (any, *rpcError) {
	var p sessionIDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, rpcErr(codeInvalidParams, "invalid session/close params: "+err.Error(), nil)
	}
	if _, notFound := a.lookup(p.SessionID); notFound != nil {
		return nil, notFound
	}
	a.cancelSession(p.SessionID)
	a.mu.Lock()
	delete(a.rooms, p.SessionID)
	a.mu.Unlock()
	return nil, nil
}

func (a *Agent) onSessionDelete(params json.RawMessage) (any, *rpcError) {
	// FakeAgents keeps no on-disk history; delete is close by another name.
	return a.onSessionClose(params)
}
