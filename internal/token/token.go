// Package token provides a deterministic token estimator. FakeAgents never
// calls a real tokenizer; the numbers are intentionally stable and only need to
// be plausible enough for client-side accounting and truncation tests.
package token

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Counter estimates token counts.
type Counter interface {
	Count(text string) int
	Truncate(text string, maxTokens int) string
	CountMessages(parts []MessagePart) int
	CountToolSchemas(schemas []string) int
}

// Heuristic is a dependency-free Counter that blends word count and character
// count. It is stable across runs and platforms.
type Heuristic struct{}

// New returns the default heuristic counter.
func New() *Heuristic { return &Heuristic{} }

// Count implements Counter.
func (*Heuristic) Count(text string) int {
	if text == "" {
		return 0
	}
	runes := utf8.RuneCountInString(text)
	words := 0
	inWord := false
	cjk := 0
	for _, r := range text {
		if isCJK(r) {
			cjk++
			inWord = false
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if !inWord {
				words++
				inWord = true
			}
			continue
		}
		inWord = false
	}
	byChars := int(math.Ceil(float64(runes) / 4.0))
	byWords := int(math.Ceil(float64(words) * 1.3))
	estimate := max(byWords, byChars)
	estimate += cjk
	if estimate < 1 {
		estimate = 1
	}
	return estimate
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// Truncate implements Counter. It returns the longest prefix whose estimated
// token count is at most maxTokens, preferring a word boundary when one is
// close to the cut point.
func (h *Heuristic) Truncate(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	if h.Count(text) <= maxTokens {
		return text
	}
	runes := []rune(text)
	low, high := 0, len(runes)
	best := 0
	for low <= high {
		mid := (low + high) / 2
		if h.Count(string(runes[:mid])) <= maxTokens {
			best = mid
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	prefix := string(runes[:best])
	if cut := strings.LastIndexAny(prefix, " \n\t"); cut > 0 && cut > len(prefix)*3/4 {
		prefix = prefix[:cut]
	}
	return strings.TrimRight(prefix, " \n\t")
}

// CountMessages estimates the prompt token cost of a conversation, including a
// small per-message overhead similar to real chat formats.
func (h *Heuristic) CountMessages(parts []MessagePart) int {
	total := 0
	for _, part := range parts {
		total += 4
		total += h.Count(part.Text)
		if part.ToolSchema {
			total += 8
		}
	}
	if total == 0 {
		total = 1
	}
	return total
}

// MessagePart is a single prompt fragment for prompt accounting.
type MessagePart struct {
	Text       string
	ToolSchema bool
}

// CountToolSchemas estimates the cost of JSON tool definitions.
func (h *Heuristic) CountToolSchemas(schemas []string) int {
	total := 0
	for _, schema := range schemas {
		total += 8 + h.Count(schema)
	}
	return total
}
