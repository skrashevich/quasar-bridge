package bridge

import (
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"
)

// streamedSentenceEnd finds a complete sentence, including punctuation
// arriving at the end of a streamed token. Decimal points are held until
// the following token disambiguates them.
func streamedSentenceEnd(text string) int {
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r != '.' && r != '!' && r != '?' && r != '…' {
			i += size
			continue
		}
		end := i + size
		for end < len(text) {
			r, size = utf8.DecodeRuneInString(text[end:])
			if r != '.' && r != '!' && r != '?' && r != '…' &&
				r != '"' && r != '»' && r != '”' && r != '’' {
				break
			}
			end += size
		}
		// Do not synthesize a fragment while an opening quote is still
		// unmatched. A closing » or ” can arrive in the next LLM token;
		// sending the punctuation prematurely would orphan that symbol in
		// the following acoustic segment.
		prefix := text[:end]
		if strings.Count(prefix, "«") > strings.Count(prefix, "»") ||
			strings.Count(prefix, "“") > strings.Count(prefix, "”") {
			i = end
			continue
		}
		if end == len(text) {
			// A decimal like "3." may still be followed by "14".
			if r == '.' && i > 0 {
				prev, _ := utf8.DecodeLastRuneInString(text[:i])
				if unicode.IsDigit(prev) {
					return -1
				}
			}
			return end
		}
		next, _ := utf8.DecodeRuneInString(text[end:])
		if unicode.IsSpace(next) {
			return end
		}
		i = end
	}
	return -1
}

// A new TtsGenerate request resets utterance prosody. Never create an
// artificial full stop or split solely at a word count: use a grammatical
// boundary that is already in the streamed text.
//
// For the first fragment, a short sentence is preferred; if the opening
// sentence is long, the first natural comma/semicolon/colon after 32 runes
// starts speech earlier. Later fragments prefer sentence endings and only
// fall back to clause punctuation only for unusually long (800+ rune) sentences.
const (
	firstClauseMinRunes  = 32
	laterClauseMinRunes  = 250
	laterClauseThreshold = 800
)

func streamedClausePauseEnd(text string, minRunes int) int {
	runes := 0
	for i, r := range text {
		runes++
		if r != ',' && r != ';' && r != ':' {
			continue
		}
		if runes < minRunes {
			continue
		}
		end := i + utf8.RuneLen(r)
		// A decimal or thousands separator is not a breathing pause.
		if r == ',' && i > 0 {
			prev, _ := utf8.DecodeLastRuneInString(text[:i])
			if unicode.IsDigit(prev) {
				if end == len(text) {
					return -1 // wait for next token to disambiguate 3,14
				}
				next, _ := utf8.DecodeRuneInString(text[end:])
				if unicode.IsDigit(next) {
					continue
				}
			}
		}
		if end < len(text) {
			next, _ := utf8.DecodeRuneInString(text[end:])
			if !unicode.IsSpace(next) {
				continue
			}
		}
		return end
	}
	return -1
}

func (b *streamedSentenceBuffer) nextBoundary() int {
	endOfSentence := streamedSentenceEnd(b.pending)
	if b.emitted == "" {
		// The first sentence is the preferred boundary when it is short.
		// If a long opening clause has a comma first, start playback there.
		clauseEnd := streamedClausePauseEnd(b.pending, firstClauseMinRunes)
		if clauseEnd > 0 && (endOfSentence < 0 || clauseEnd < endOfSentence) {
			return clauseEnd
		}
		return endOfSentence
	}
	if endOfSentence > 0 && utf8.RuneCountInString(b.pending[:endOfSentence]) < laterClauseThreshold {
		return endOfSentence
	}
	// For subsequent very long sentences, prefer a real pause. This is
	// never a mid-word or arbitrary character-limit split.
	if utf8.RuneCountInString(b.pending) >= laterClauseThreshold {
		clauseEnd := streamedClausePauseEnd(b.pending, laterClauseMinRunes)
		if clauseEnd > 0 && (endOfSentence < 0 || clauseEnd < endOfSentence) {
			return clauseEnd
		}
	}
	return endOfSentence
}

type streamedSentenceBuffer struct {
	pending, emitted string
}

func (b *streamedSentenceBuffer) feed(delta string, emit func(string) error) error {
	b.pending += delta
	for {
		end := b.nextBoundary()
		if end <= 0 {
			return nil
		}
		raw := b.pending[:end]
		text := strings.TrimSpace(raw)
		if text != "" {
			if err := emit(text); err != nil {
				return err
			}
		}
		b.emitted += raw
		b.pending = b.pending[end:]
	}
}

// feedNative sends exactly the original characters of each completed phrase
// to Yandex's existing DeferredTtsGenerate session. Unlike the display-oriented
// feed method, it MUST preserve boundary whitespace verbatim. A leading space
// at the beginning of the next chunk is preferable to silently concatenating
// two spoken words when the LLM streamed "Первое. Второе.".
func (b *streamedSentenceBuffer) feedNative(delta string, emit func(string) error) error {
	b.pending += delta
	for {
		end := b.nextBoundary()
		if end <= 0 {
			return nil
		}
		raw := b.pending[:end]
		if strings.TrimSpace(raw) != "" {
			if err := emit(raw); err != nil {
				return err
			}
		}
		b.emitted += raw
		b.pending = b.pending[end:]
	}
}

func (b *streamedSentenceBuffer) remaining(full string) string {
	if b.emitted == "" {
		return full
	}
	if strings.HasPrefix(full, b.emitted) {
		return strings.TrimSpace(full[len(b.emitted):])
	}
	// Never replay already-synthesized audio if the provider normalizes its
	// final answer differently from its streamed deltas.
	slog.Warn("native_stream_prefix_mismatch", "emitted_bytes", len(b.emitted), "answer_bytes", len(full))
	return strings.TrimSpace(b.pending)
}
