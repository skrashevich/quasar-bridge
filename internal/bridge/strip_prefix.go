package bridge

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"quasarbridge/internal/llm"
)

// modelMessages keeps model addresses out of subsequent turns too, without
// changing the original dialogue or assistant/tool content.
func (e *Engine) modelMessages(messages []llm.Message) []llm.Message {
	out := slices.Clone(messages)
	for i := range out {
		if out[i].Role == "user" {
			out[i].Content = e.stripModelPrefix(out[i].Content)
		}
	}
	return out
}

// stripModelPrefix removes the longest configured address at the start of the
// request, optionally after "Алиса,". Unmatched and empty requests stay intact.
func (e *Engine) stripModelPrefix(text string) string {
	start := strings.TrimSpace(text)
	candidates := []string{start}
	if rest, ok := cutModelPrefix(start, "алиса"); ok {
		candidates = append(candidates, trimModelSeparator(rest))
	}
	matched := 0
	var rest string
	for _, configured := range e.StripPrefixes {
		prefix := strings.TrimSpace(configured)
		if prefix == "" {
			continue
		}
		for _, candidate := range candidates {
			if tail, ok := cutModelPrefix(candidate, prefix); ok {
				if consumed := len(candidate) - len(tail); consumed > matched {
					matched, rest = consumed, tail
				}
			}
		}
	}
	if matched > 0 {
		if question := trimModelSeparator(rest); question != "" {
			return question
		}
	}
	return text
}

// Match case-insensitively while slicing at the original UTF-8 boundaries.
// Folding can change byte length (for example K and k).
func cutModelPrefix(text, prefix string) (string, bool) {
	end := 0
	for range prefix {
		_, size := utf8.DecodeRuneInString(text[end:])
		if size == 0 {
			return text, false
		}
		end += size
	}
	if !strings.EqualFold(text[:end], prefix) {
		return text, false
	}
	rest := text[end:]
	last, _ := utf8.DecodeLastRuneInString(prefix)
	if rest != "" && (unicode.IsLetter(last) || unicode.IsNumber(last) || unicode.IsMark(last) || last == '_') {
		r, _ := utf8.DecodeRuneInString(rest)
		if !unicode.IsSpace(r) && !strings.ContainsRune(",.:;!—", r) {
			return text, false
		}
	}
	return rest, true
}

// Consume one spoken separator, then leave the question's own punctuation
// alone: ': .gitignore', ': !flag' and ': -2' must retain their meaning.
func trimModelSeparator(text string) string {
	text = strings.TrimSpace(text)
	r, size := utf8.DecodeRuneInString(text)
	if strings.ContainsRune(",:;—", r) {
		return strings.TrimSpace(text[size:])
	}
	if r == '.' || r == '!' {
		next, _ := utf8.DecodeRuneInString(text[size:])
		if size == len(text) || unicode.IsSpace(next) {
			return strings.TrimSpace(text[size:])
		}
	}
	return text
}
