package bridge

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var speechSourceLabel = regexp.MustCompile(`(?i)^(?:(?:https?://|www\.)\S+|(?:[\p{L}\p{N}-]+\.)+[\p{L}]{2,63}(?::[0-9]+)?(?:[/#?]\S*)?)$`)
var speechAudio = regexp.MustCompile(`(?i)<speaker\s+[^<>]*\baudio\s*=\s*(?:"[^"]+"|'[^']+')`)

// speechText leaves the displayed answer and dialogue history to the caller.
func speechText(text string) string {
	stripped, changed := speechLinks(text, 0)
	if !changed {
		return text
	}
	stripped = speechSpacing(stripped)
	if !speechReadable(stripped) {
		return strings.Join(ttsMarkup.FindAllString(stripped, -1), "") + "Ссылка доступна в текстовом ответе."
	}
	return stripped
}

func speechLinks(text string, depth int) (string, bool) {
	var out strings.Builder
	changed := false
	// Exhausted markup parsing falls through to flat URL removal. Bound both
	// nesting and failed delimiter scans, including malformed bracket runs.
	budget := 2 * len(text)
	for i := 0; i < len(text); {
		if text[i] == '<' {
			if end := speechAngleEnd(text, i, &budget); end > i {
				inside := text[i+1 : end-1]
				if speechURLPrefix(inside) && !strings.ContainsFunc(inside, unicode.IsSpace) {
					out.WriteByte(' ')
					changed = true
				} else {
					// Speaker attributes, pauses and phonemes are opaque to URL removal.
					out.WriteString(text[i:end])
				}
				i = end
				continue
			}
		}
		if speechURLPrefix(text[i:]) && speechURLBoundary(text, i) {
			out.WriteByte(' ')
			changed = true
			i = speechURLEnd(text, i)
			continue
		}
		if depth < 16 && budget > 0 && (text[i] == '[' || text[i] == '(') {
			open, close := text[i], byte(')')
			if open == '[' {
				close = ']'
			}
			if end := speechPairEnd(text, i, open, close, &budget); end > i {
				destinationStart := end + 1
				for destinationStart < len(text) {
					r, size := utf8.DecodeRuneInString(text[destinationStart:])
					if !unicode.IsSpace(r) {
						break
					}
					destinationStart += size
				}
				if open == '[' && destinationStart < len(text) && text[destinationStart] == '(' {
					if destination := speechPairEnd(text, destinationStart, '(', ')', &budget); destination > end {
						label := text[i+1 : end]
						if speechSourceLabel.MatchString(strings.TrimSpace(label)) {
							out.WriteByte(' ')
						} else {
							label, _ = speechLinks(label, depth+1)
							out.WriteString(label)
						}
						changed, i = true, destination+1
						continue
					}
				}
				inner, removed := speechLinks(text[i+1:end], depth+1)
				if removed {
					changed = true
					if speechReadable(inner) {
						out.WriteByte(open)
						out.WriteString(inner)
						out.WriteByte(close)
					} else {
						out.WriteString(strings.Join(ttsMarkup.FindAllString(inner, -1), ""))
						out.WriteByte(' ')
					}
				} else {
					out.WriteString(text[i : end+1])
				}
				i = end + 1
				continue
			}
		}
		out.WriteByte(text[i])
		i++
	}
	return out.String(), changed
}

func speechURLPrefix(text string) bool {
	return len(text) >= 8 && strings.EqualFold(text[:8], "https://") ||
		len(text) >= 7 && strings.EqualFold(text[:7], "http://") ||
		len(text) >= 4 && strings.EqualFold(text[:4], "www.")
}

func speechURLBoundary(text string, at int) bool {
	if at == 0 {
		return true
	}
	previous, _ := utf8.DecodeLastRuneInString(text[:at])
	return !unicode.IsLetter(previous) && !unicode.IsDigit(previous) && previous != '_' && previous != '@'
}

func speechURLEnd(text string, start int) int {
	depth, end := 0, start
	for end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		if unicode.IsSpace(r) || strings.ContainsRune("<>[]\"«»", r) || r == ')' && depth == 0 {
			break
		}
		if r == '(' {
			depth++
		} else if r == ')' {
			depth--
		}
		end += size
	}
	return start + len(strings.TrimRight(text[start:end], ".,;:!?"))
}

func speechPairEnd(text string, start int, open, close byte, budget *int) int {
	depth := 0
	for i := start; i < len(text); i++ {
		if *budget <= 0 {
			return -1
		}
		*budget--
		if text[i] == '\\' {
			i++
			continue
		}
		if text[i] == open {
			depth++
		} else if text[i] == close {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func speechAngleEnd(text string, start int, budget *int) int {
	quote := byte(0)
	autolink := speechURLPrefix(text[start+1:])
	for i := start + 1; i < len(text); i++ {
		if *budget <= 0 {
			return -1
		}
		*budget--
		if quote != 0 {
			if text[i] == quote {
				quote = 0
			}
		} else if !autolink && (text[i] == '\'' || text[i] == '"') {
			quote = text[i]
		} else if text[i] == '>' {
			return i + 1
		} else if text[i] == '\n' {
			break
		}
	}
	return -1
}

func speechReadable(text string) bool {
	if speechAudio.MatchString(text) {
		return true
	}
	plain := ttsMarkup.ReplaceAllString(text, "$1")
	return strings.ContainsFunc(plain, func(r rune) bool {
		return !unicode.IsSpace(r) && !unicode.IsPunct(r) && !unicode.IsControl(r)
	})
}

func speechSpacing(text string) string {
	var out strings.Builder
	var previous rune
	space := false
	budget := 2 * len(text)
	for i := 0; i < len(text); {
		if text[i] == '<' {
			if end := speechAngleEnd(text, i, &budget); end > i {
				if space && out.Len() > 0 {
					out.WriteByte(' ')
				}
				out.WriteString(text[i:end])
				i, previous, space = end, '>', false
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && previous == r && strings.ContainsRune(".!?;,", r) {
			space = false
			continue
		}
		if space && out.Len() > 0 && !strings.ContainsRune(",.;:!?)]}", r) && !strings.ContainsRune("([{", previous) {
			out.WriteByte(' ')
		}
		out.WriteRune(r)
		previous, space = r, false
	}
	return out.String()
}
