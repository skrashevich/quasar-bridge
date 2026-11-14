package assistant

import (
	"net/url"
	"strings"
)

type pageMusic struct {
	player  bool
	scripts []string
	base    *url.URL
}

func sameWebOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// script reads a narrow subset of JavaScript data, never executing code or
// combining expressions. Arbitrary strings and comments are not media sources.
func (c *mediaCollector) script(body string) {
	l := scriptLexer{source: body}
	for token := l.next(); token.kind != 0; token = l.next() {
		if token.kind != 'i' {
			continue
		}
		switch strings.ToLower(token.value) {
		case "audiourl", "audio_url", "streamurl", "stream_url", "hlsurl", "hls_url", "playlisturl", "playlist_url":
			if l.next().value != "=" {
				continue
			}
			literal, end := l.next(), l.next()
			if literal.kind == 's' && (end.kind == 0 || end.value == ";" || end.value == "," || end.value == "}") {
				c.addAudio(literal.value, "", false)
			}
		case "loadsource":
			if l.next().value != "(" {
				continue
			}
			literal, end := l.next(), l.next()
			if literal.kind == 's' && end.value == ")" {
				c.addAudio(literal.value, "", false)
			}
		}
	}
}

type scriptToken struct {
	kind  byte
	value string
}

type scriptLexer struct {
	source string
	pos    int
}

func (l *scriptLexer) next() scriptToken {
	for l.pos < len(l.source) {
		ch := l.source[l.pos]
		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' {
			l.pos++
			continue
		}
		if strings.HasPrefix(l.source[l.pos:], "//") {
			if end := strings.IndexByte(l.source[l.pos:], '\n'); end >= 0 {
				l.pos += end + 1
				continue
			}
			l.pos = len(l.source)
			return scriptToken{}
		}
		if strings.HasPrefix(l.source[l.pos:], "/*") {
			if end := strings.Index(l.source[l.pos+2:], "*/"); end >= 0 {
				l.pos += end + 4
				continue
			}
			l.pos = len(l.source)
			return scriptToken{}
		}
		if ch == '`' {
			// Templates can contain nested JavaScript. Stop rather than treating
			// their contents as independent static assignments.
			l.pos = len(l.source)
			return scriptToken{kind: 'x'}
		}
		if ch == '\'' || ch == '"' {
			return l.quoted(ch)
		}
		if scriptIdentifier(ch) {
			start := l.pos
			for l.pos < len(l.source) && (scriptIdentifier(l.source[l.pos]) || l.source[l.pos] >= '0' && l.source[l.pos] <= '9') {
				l.pos++
			}
			return scriptToken{kind: 'i', value: l.source[start:l.pos]}
		}
		if ch == '/' {
			// Skip regular-expression literals conservatively; their contents
			// must not be mistaken for player configuration.
			l.pos++
			inClass := false
			for l.pos < len(l.source) && l.source[l.pos] != '\n' {
				current := l.source[l.pos]
				l.pos++
				if current == '\\' && l.pos < len(l.source) {
					l.pos++
				} else if current == '[' {
					inClass = true
				} else if current == ']' {
					inClass = false
				} else if current == '/' && !inClass {
					break
				}
			}
			return scriptToken{kind: 'x'}
		}
		l.pos++
		return scriptToken{kind: 'p', value: string(ch)}
	}
	return scriptToken{}
}

func scriptIdentifier(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch == '_' || ch == '$'
}

func (l *scriptLexer) quoted(quote byte) scriptToken {
	l.pos++
	var value strings.Builder
	valid := true
	for l.pos < len(l.source) {
		ch := l.source[l.pos]
		l.pos++
		if ch == quote {
			if valid {
				return scriptToken{kind: 's', value: value.String()}
			}
			return scriptToken{kind: 'x'}
		}
		if ch == '\r' || ch == '\n' {
			valid = false
		}
		if ch == '\\' {
			if l.pos == len(l.source) {
				break
			}
			ch = l.source[l.pos]
			l.pos++
			if ch != '\\' && ch != '/' && ch != '\'' && ch != '"' {
				valid = false
			}
		}
		value.WriteByte(ch)
	}
	return scriptToken{kind: 'x'}
}
