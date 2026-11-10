package token_test

import (
	"testing"

	"github.com/jahrulnr/fakeagents/internal/token"
)

func TestCount(t *testing.T) {
	counter := token.New()
	if got := counter.Count(""); got != 0 {
		t.Fatalf("empty = %d", got)
	}
	short := counter.Count("hello world")
	long := counter.Count("hello world this is a much longer sentence")
	if short <= 0 || long <= short {
		t.Fatalf("unexpected counts: %d, %d", short, long)
	}
}

func TestTruncate(t *testing.T) {
	counter := token.New()
	text := "The quick brown fox jumps over the lazy dog. " + "Extra content follows here."
	truncated := counter.Truncate(text, 5)
	if truncated == "" {
		t.Fatal("expected non-empty truncation")
	}
	if counter.Count(truncated) > 5 {
		t.Fatalf("truncated text has %d tokens, want <= 5", counter.Count(truncated))
	}
	if counter.Truncate(text, 0) != "" {
		t.Fatal("zero budget must return empty string")
	}
	if counter.Truncate("short", 100) != "short" {
		t.Fatal("text under budget must be unchanged")
	}
}

func TestCountMessages(t *testing.T) {
	counter := token.New()
	parts := []token.MessagePart{{Text: "hello"}, {Text: "world", ToolSchema: true}}
	if got := counter.CountMessages(parts); got < 10 {
		t.Fatalf("message overhead missing: %d", got)
	}
	if got := counter.CountToolSchemas([]string{`{"type":"object"}`}); got <= 0 {
		t.Fatalf("tool schema count = %d", got)
	}
}
