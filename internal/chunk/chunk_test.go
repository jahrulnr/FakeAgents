package chunk_test

import (
	"strings"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/chunk"
)

func TestSplitRunesPreservesText(t *testing.T) {
	text := "Hello, 世界 🌏 — emoji 😀😃 and more."
	parts := chunk.Split(text, 4, "runes")
	if strings.Join(parts, "") != text {
		t.Fatalf("chunks do not reconstruct text: %q", parts)
	}
	for _, part := range parts {
		if !validUTF8(part) {
			t.Fatalf("invalid UTF-8 chunk: %q", part)
		}
	}
}

func TestSplitWordsPreservesWhitespace(t *testing.T) {
	text := "one two  three\nfour five"
	parts := chunk.Split(text, 2, "words")
	if strings.Join(parts, "") != text {
		t.Fatalf("chunks do not reconstruct text: %q", parts)
	}
	if len(parts) != 3 {
		t.Fatalf("expected 3 chunks, got %d: %q", len(parts), parts)
	}
}

func TestSplitEmpty(t *testing.T) {
	if parts := chunk.Split("", 10, "runes"); len(parts) != 0 {
		t.Fatalf("expected no chunks, got %q", parts)
	}
}

func validUTF8(value string) bool {
	for _, r := range value {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}
