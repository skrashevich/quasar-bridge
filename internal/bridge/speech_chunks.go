package bridge

import (
	"strings"
	"unicode"
)

// speechChunks collects text deltas until a sentence boundary. The caller
// sends completed chunks in order and flushes the remainder at completion.
type speechChunks struct {
	pending string
}

func (s *speechChunks) add(delta string) []string {
	s.pending += delta
	var chunks []string
	start := 0
	runes := 0
	for i, r := range s.pending {
		runes++
		// Long answers with no sentence punctuation should still start talking.
		// Break only at whitespace and retain the remainder for the next chunk.
		if runes >= 120 && unicode.IsSpace(r) {
			if part := strings.TrimSpace(s.pending[start:i]); part != "" {
				chunks = append(chunks, part)
			}
			start = i + len(string(r))
			runes = 0
			continue
		}
		if r != '.' && r != '!' && r != '?' && r != '\n' {
			continue
		}
		end := i + 1
		if end == len(s.pending) {
			break // punctuation may be inside a URL, abbreviation or decimal
		}
		next, _ := utf8First(s.pending[end:])
		if !unicode.IsSpace(next) {
			continue
		}
		if part := strings.TrimSpace(s.pending[start:end]); part != "" {
			chunks = append(chunks, part)
		}
		start = end
		runes = 0
	}
	s.pending = s.pending[start:]
	return chunks
}

func utf8First(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 0
}

func (s *speechChunks) flush() string {
	result := strings.TrimSpace(s.pending)
	s.pending = ""
	return result
}
