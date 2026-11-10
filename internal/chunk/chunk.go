// Package chunk splits generated text into streaming deltas while preserving
// UTF-8 rune boundaries and, for word mode, whitespace.
package chunk

import (
	"strings"
	"unicode"
)

// Split divides text into chunks of size runes or words. size <= 0 is treated
// as 1. The concatenation of all chunks equals the input.
func Split(text string, size int, unit string) []string {
	if text == "" {
		return nil
	}
	if size <= 0 {
		size = 1
	}
	if strings.EqualFold(unit, "words") {
		return splitWords(text, size)
	}
	return splitRunes(text, size)
}

func splitRunes(text string, size int) []string {
	runes := []rune(text)
	out := make([]string, 0, (len(runes)+size-1)/size)
	for start := 0; start < len(runes); start += size {
		end := min(start+size, len(runes))
		out = append(out, string(runes[start:end]))
	}
	return out
}

func splitWords(text string, size int) []string {
	runes := []rune(text)
	var out []string
	start := 0
	words := 0
	inWord := false
	for i, r := range runes {
		if unicode.IsSpace(r) {
			if inWord {
				words++
				inWord = false
			}
			if words >= size {
				out = append(out, string(runes[start:i+1]))
				start = i + 1
				words = 0
			}
			continue
		}
		inWord = true
	}
	if start < len(runes) {
		out = append(out, string(runes[start:]))
	}
	return out
}
