// Package session tracks per-client turn counters used by templates, scripts,
// and cache simulation.
package session

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jahrulnr/fakeagents/internal/clock"
)

// Info is a snapshot of one session.
type Info struct {
	Key      string    `json:"key"`
	Turn     int       `json:"turn"`
	Requests int       `json:"requests"`
	LastSeen time.Time `json:"last_seen"`
}

// Tracker is the session store contract used by the engine.
type Tracker interface {
	Next(key string) int
	Peek(key string) Info
	Reset()
	Snapshot() []Info
}

// Memory is an in-memory Tracker.
type Memory struct {
	mu       sync.Mutex
	clock    clock.Clock
	sessions map[string]*state
}

type state struct {
	turn     int
	requests int
	lastSeen time.Time
}

// New returns a session tracker.
func New(clk clock.Clock) *Memory {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Memory{clock: clk, sessions: make(map[string]*state)}
}

// Next returns the current turn and increments the counter.
func (m *Memory) Next(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.get(key)
	turn := s.turn
	s.turn++
	s.requests++
	s.lastSeen = m.clock.Now()
	return turn
}

// Peek returns a snapshot without incrementing.
func (m *Memory) Peek(key string) Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[key]
	if !ok {
		return Info{Key: key}
	}
	return Info{Key: key, Turn: s.turn, Requests: s.requests, LastSeen: s.lastSeen}
}

// Reset clears all sessions.
func (m *Memory) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = make(map[string]*state)
}

// Snapshot returns all sessions for debug endpoints.
func (m *Memory) Snapshot() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.sessions))
	for key, s := range m.sessions {
		out = append(out, Info{Key: key, Turn: s.turn, Requests: s.requests, LastSeen: s.lastSeen})
	}
	return out
}

func (m *Memory) get(key string) *state {
	s, ok := m.sessions[key]
	if !ok {
		s = &state{lastSeen: m.clock.Now()}
		m.sessions[key] = s
	}
	return s
}

// Resolve determines a session key from configured headers, then optional
// provider-specific hints, then a fallback (usually the client IP).
func Resolve(headers http.Header, configured []string, fallback string, hints ...string) string {
	for _, name := range configured {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return name + ":" + value
		}
	}
	for _, hint := range hints {
		if hint = strings.TrimSpace(hint); hint != "" {
			return "hint:" + hint
		}
	}
	if fallback == "" {
		return "anonymous"
	}
	return "ip:" + fallback
}

// KeyFromHeader returns a deterministic key for a specific header name.
func KeyFromHeader(headers http.Header, name string) string {
	value := strings.TrimSpace(headers.Get(name))
	if value == "" {
		return ""
	}
	return name + ":" + value
}
