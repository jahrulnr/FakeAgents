package session_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/jahrulnr/fakeagents/internal/clock"
	"github.com/jahrulnr/fakeagents/internal/session"
)

func TestNextPeekReset(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	tracker := session.New(clk)
	if turn := tracker.Next("s"); turn != 0 {
		t.Fatalf("turn = %d", turn)
	}
	if turn := tracker.Next("s"); turn != 1 {
		t.Fatalf("turn = %d", turn)
	}
	info := tracker.Peek("s")
	if info.Turn != 2 || info.Requests != 2 {
		t.Fatalf("info = %+v", info)
	}
	if len(tracker.Snapshot()) != 1 {
		t.Fatalf("snapshot = %+v", tracker.Snapshot())
	}
	tracker.Reset()
	if len(tracker.Snapshot()) != 0 {
		t.Fatal("reset failed")
	}
}

func TestResolvePrecedence(t *testing.T) {
	headers := http.Header{}
	headers.Set("x-opencode-session", "open")
	headers.Set("x-session-id", "secondary")
	key := session.Resolve(headers, []string{"x-fakeagents-session", "x-opencode-session"}, "127.0.0.1")
	if key != "x-opencode-session:open" {
		t.Fatalf("key = %q", key)
	}
	if got := session.Resolve(http.Header{}, nil, "10.0.0.1", "hint"); got != "hint:hint" {
		t.Fatalf("hint key = %q", got)
	}
	if got := session.Resolve(http.Header{}, nil, "10.0.0.1"); got != "ip:10.0.0.1" {
		t.Fatalf("fallback key = %q", got)
	}
	if got := session.KeyFromHeader(headers, "x-session-id"); got != "x-session-id:secondary" {
		t.Fatalf("header key = %q", got)
	}
}
