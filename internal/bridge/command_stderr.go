package bridge

import (
	"bytes"
	"regexp"
	"strings"
	"unicode"
)

const maxCommandStderr = 2048

// Always drain stderr, including after the excerpt fills. A verbose program
// must not block on a full pipe or change the result of the command.
type commandStderr struct {
	data  []byte
	total int64
}

func (s *commandStderr) Write(p []byte) (int, error) {
	s.total += int64(len(p))
	s.data = append(s.data, p[:min(len(p), maxCommandStderr-len(s.data))]...)
	return len(p), nil
}

var stderrCredential = regexp.MustCompile(`(?i)authorization|cookie|password|passwd|secret|api[-_ ]?key|(?:access|refresh|auth|id)[_-]?token|--token\b|\btoken["']?\s*[:=]|bearer\s|https?://[^\s]*[?@]|\b(?:sk-|gh[pousr]_)[a-z0-9_-]{12,}|"text"\s*:`)

func (s *commandStderr) excerpt(text string, args []string, identifiers ...string) string {
	const hidden = "[скрыто]"
	if bytes.Contains(s.data, []byte("-----BEGIN ")) {
		return hidden
	}
	raw := s.data
	if int64(len(raw)) < s.total {
		// A partial line may cut a credential or a multibyte character in half.
		if end := bytes.LastIndexByte(raw, '\n'); end >= 0 {
			raw = raw[:end]
		} else {
			return "[длинная строка пропущена]"
		}
	}
	lines := strings.Split(strings.ToValidUTF8(string(raw), "�"), "\n")
	for i, line := range lines {
		if stderrCredential.MatchString(line) {
			lines[i] = hidden
		}
	}
	message := strings.Join(lines, "\n")
	// Some programs echo their input on parse errors. Keep the transcript,
	// device identifiers and configured argument values out of such messages.
	var replacements []string
	for _, values := range [][]string{{text}, identifiers, args} {
		for _, value := range values {
			if value != "" && strings.Contains(message, value) {
				replacements = append(replacements, value, "*")
			}
		}
	}
	if len(replacements) > 0 {
		message = strings.NewReplacer(replacements...).Replace(message)
	}
	// Render controls as spaces so stderr cannot forge a second log record.
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, message))
}
