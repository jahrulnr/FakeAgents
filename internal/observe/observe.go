// Package observe records request traces for logging and the debug endpoints.
package observe

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

type contextKey struct{}

// ToolInfo is a recorded harness tool declaration.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

// Trace is mutable metadata for one in-flight request. It is safe for
// concurrent use because handlers and middleware may run in different
// goroutines during streaming.
type Trace struct {
	mu sync.Mutex

	RequestID string
	Method    string
	Path      string
	Provider  string
	Endpoint  string
	Model     string
	Scenario  string
	Session   string
	Stream    bool
	Turn      int
	Step      int
	Tools     []ToolInfo
	Status    int
	TokensIn  int
	TokensOut int
	Start     time.Time
	TTFT      time.Duration
	Err       string
}

// NewTrace creates a trace positioned at start.
func NewTrace(requestID, method, path string, start time.Time) *Trace {
	return &Trace{RequestID: requestID, Method: method, Path: path, Start: start, Status: 200}
}

// WithTrace stores the trace in a context.
func WithTrace(ctx context.Context, trace *Trace) context.Context {
	return context.WithValue(ctx, contextKey{}, trace)
}

// From retrieves the trace from a context.
func From(ctx context.Context) *Trace {
	trace, _ := ctx.Value(contextKey{}).(*Trace)
	return trace
}

// SetProvider records provider and endpoint.
func (t *Trace) SetProvider(provider, endpoint string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Provider = provider
	t.Endpoint = endpoint
}

// SetModel records the resolved model.
func (t *Trace) SetModel(model string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Model = model
}

// SetScenario records the selected scenario.
func (t *Trace) SetScenario(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Scenario = name
}

// SetSession records session and turn.
func (t *Trace) SetSession(key string, turn int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Session = key
	t.Turn = turn
}

// SetStep records the active agent/script step.
func (t *Trace) SetStep(step int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Step = step
}

// SetTools records the harness tool catalog for debugging.
func (t *Trace) SetTools(tools []ToolInfo) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Tools = append([]ToolInfo(nil), tools...)
}

// SetStream records whether the request is streaming.
func (t *Trace) SetStream(stream bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Stream = stream
}

// SetUsage records token counts.
func (t *Trace) SetUsage(in, out int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.TokensIn = in
	t.TokensOut = out
}

// SetStatus records the HTTP status.
func (t *Trace) SetStatus(status int) {
	if t == nil || status == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Status = status
}

// SetError records a short error code.
func (t *Trace) SetError(err string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Err = err
}

// MarkTTFT records time to first byte.
func (t *Trace) MarkTTFT(d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.TTFT == 0 {
		t.TTFT = d
	}
}

// Snapshot is an immutable trace record.
type Snapshot struct {
	RequestID  string     `json:"request_id"`
	Time       time.Time  `json:"time"`
	Method     string     `json:"method"`
	Path       string     `json:"path"`
	Provider   string     `json:"provider"`
	Endpoint   string     `json:"endpoint"`
	Model      string     `json:"model"`
	Scenario   string     `json:"scenario"`
	Session    string     `json:"session"`
	Stream     bool       `json:"stream"`
	Turn       int        `json:"turn"`
	Step       int        `json:"step"`
	Tools      []ToolInfo `json:"tools,omitempty"`
	Status     int        `json:"status"`
	TokensIn   int        `json:"tokens_in"`
	TokensOut  int        `json:"tokens_out"`
	DurationMS int64      `json:"duration_ms"`
	TTFTMS     int64      `json:"ttft_ms"`
	Error      string     `json:"error,omitempty"`
}

// Snapshot returns an immutable copy.
func (t *Trace) Snapshot() Snapshot {
	if t == nil {
		return Snapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return Snapshot{
		RequestID:  t.RequestID,
		Time:       t.Start,
		Method:     t.Method,
		Path:       t.Path,
		Provider:   t.Provider,
		Endpoint:   t.Endpoint,
		Model:      t.Model,
		Scenario:   t.Scenario,
		Session:    t.Session,
		Stream:     t.Stream,
		Turn:       t.Turn,
		Step:       t.Step,
		Tools:      append([]ToolInfo(nil), t.Tools...),
		Status:     t.Status,
		TokensIn:   t.TokensIn,
		TokensOut:  t.TokensOut,
		DurationMS: time.Since(t.Start).Milliseconds(),
		TTFTMS:     t.TTFT.Milliseconds(),
		Error:      t.Err,
	}
}

// Recorder is a bounded ring buffer of recent requests.
type Recorder struct {
	mu      sync.Mutex
	entries []Snapshot
	max     int
}

// NewRecorder builds a recorder with the given capacity (default 200).
func NewRecorder(max int) *Recorder {
	if max <= 0 {
		max = 200
	}
	return &Recorder{max: max}
}

// Add appends a snapshot, dropping the oldest when full.
func (r *Recorder) Add(snapshot Snapshot) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, snapshot)
	if len(r.entries) > r.max {
		r.entries = r.entries[len(r.entries)-r.max:]
	}
}

// List returns up to limit recent snapshots, newest first.
func (r *Recorder) List(limit int) []Snapshot {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > len(r.entries) {
		limit = len(r.entries)
	}
	out := make([]Snapshot, 0, limit)
	for i := len(r.entries) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, r.entries[i])
	}
	return out
}

// Reset clears the buffer.
func (r *Recorder) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = nil
}
