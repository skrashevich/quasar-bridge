// Package webaddress recognizes written and spoken website addresses in requests.
package webaddress

import (
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

const label = `[\p{L}\p{N}][\p{L}\p{N}-]{0,62}`
const spokenDot = `\s+(?:точка|dot)\s+`
const spokenSlash = `\s+(?:слэш|слеш|slash)\s+`
const pathToken = `[^\s<>"'«»]+`
const spokenPath = `(?:` + spokenSlash + pathToken + `)*`
const writtenDomain = `(?:` + label + `\.)+[\p{L}]{2,63}`
const addressTail = `(?::[0-9]{1,5})?(?:[/?#]` + pathToken + `)?` + spokenPath

var spokenDomain = spokenLabel + `(?:` + spokenDot + spokenLabel + `)*` + spokenDot + `(?:с\s+ю|эр\s+ю|` + spokenLabel + `)`
var addresses = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}@_])(` + `https?://` + pathToken + spokenPath + `|(?:` + spokenDomain + `|` + writtenDomain + `)` + addressTail + `)`)
var dots = regexp.MustCompile(`(?i)` + spokenDot)
var slashes = regexp.MustCompile(`(?i)` + spokenSlash)
var absoluteAddress = regexp.MustCompile(`(?i)https?://`)
var playbackVerb = regexp.MustCompile(`(?i)(?:^|[\s,;:])(?:включи(?:те)?|проиграй(?:те)?|воспроизведи(?:те)?|поставь(?:те)?|запусти(?:те)?|сыграй(?:те)?)(?:\s|$)`)
var musicTerm = regexp.MustCompile(`(?i)(?:^|[\s,;:])(?:музык|аудио|песн|трек|радио|звук)`)

var suffixAliases = map[string]string{
	"су": "su", "с ю": "su", "эс ю": "su", "зу": "su", "zu": "su",
	"ру": "ru", "эр ю": "ru", "ком": "com", "нет": "net", "орг": "org", "ио": "io", "дев": "dev",
}

// Normalize changes only address spans, preserving the rest of the request.
// Speech aliases identify Latin labels; actual internationalized TLDs keep IDNA.
func Normalize(text string) string {
	var out strings.Builder
	previous := 0
	for _, match := range addresses.FindAllStringSubmatchIndex(text, -1) {
		span := match[2:4]
		if span[1] < len(text) {
			after, _ := utf8.DecodeRuneInString(text[span[1]:])
			if unicode.IsLetter(after) || unicode.IsDigit(after) || strings.ContainsRune("_-", after) {
				continue
			}
		}
		if span[0] > 0 {
			before, _ := utf8.DecodeLastRuneInString(text[:span[0]])
			if unicode.IsLetter(before) || unicode.IsDigit(before) || strings.ContainsRune("@_", before) {
				continue
			}
		}
		raw := text[span[0]:span[1]]
		address := strings.TrimRight(raw, ".,!?;:")
		if strings.Count(address, ")") > strings.Count(address, "(") {
			address = strings.TrimRight(address, ")")
		}
		canonical, ok := normalizeAddress(address, false)
		if !ok {
			continue
		}
		out.WriteString(text[previous:span[0]])
		out.WriteString(canonical)
		out.WriteString(raw[len(address):])
		previous = span[1]
	}
	if previous == 0 {
		return text
	}
	out.WriteString(text[previous:])
	return out.String()
}

func normalizeAddress(raw string, unknownSuffix bool) (string, bool) {
	spoken := dots.MatchString(raw)
	raw = dots.ReplaceAllString(raw, ".")
	raw = slashes.ReplaceAllString(raw, "/")
	scheme, rest, explicit := strings.Cut(raw, "://")
	if !explicit {
		scheme, rest = "https", raw
	}
	head, tail := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		head, tail = rest[:i], rest[i:]
	}
	host, port := head, ""
	if !strings.HasPrefix(head, "[") {
		if name, suffix, ok := strings.Cut(head, ":"); ok {
			host, port = name, ":"+suffix
		}
	}
	canonicalHost, ok := normalizeHost(host, spoken, explicit, unknownSuffix)
	if !ok {
		return "", false
	}
	u, err := url.Parse(strings.ToLower(scheme) + "://" + canonicalHost + port + tail)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	return u.String(), true
}

func normalizeHost(host string, spoken, explicit, unknownSuffix bool) (string, bool) {
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return host, explicit
	}
	labels := strings.Split(strings.ToLower(host), ".")
	last := len(labels) - 1
	suffix := strings.Join(strings.Fields(labels[last]), " ")
	if spoken {
		if spelled, ok := spellLabel(suffix); ok {
			suffix = spelled
			labels[last] = spelled
		}
	}
	mapped, alias := suffixAliases[suffix]
	if alias {
		labels[last] = mapped
	}
	international := strings.ContainsFunc(suffix, func(r rune) bool { return r > unicode.MaxASCII }) && !alias
	if unknownSuffix && spoken && international {
		asciiSuffix, err := idna.Lookup.ToASCII(suffix)
		if _, known := publicsuffix.PublicSuffix(asciiSuffix); err == nil && !known {
			international = false
		}
	}
	if spoken {
		for i := range last {
			if spelled, ok := spellLabel(labels[i]); ok {
				labels[i] = spelled
			}
		}
	}
	if (spoken || alias) && !international {
		for i := range last {
			labels[i] = transliterate(labels[i])
		}
	}
	ascii, err := idna.Lookup.ToASCII(strings.Join(labels, "."))
	if err != nil {
		return "", false
	}
	for part := range strings.SplitSeq(ascii, ".") {
		if len(part) == 0 || len(part) > 63 || strings.HasPrefix(part, "-") || strings.HasSuffix(part, "-") {
			return "", false
		}
	}
	if len(ascii) > 253 {
		return "", false
	}
	if !unknownSuffix && (spoken || (international && !explicit)) {
		_, suffix, _ := strings.CutLast(ascii, ".")
		if _, known := publicsuffix.PublicSuffix(suffix); !known {
			return "", false
		}
	}
	return ascii, true
}

// CanonicalSource accepts one complete website address with a known public TLD.
// It does not guess corrections for unknown suffixes or discard extra words.
func CanonicalSource(raw string) (string, bool) {
	canonical, ok := completeSource(raw, false)
	if !ok {
		return "", false
	}
	u, _ := url.Parse(canonical)
	_, suffix, hasDot := strings.CutLast(u.Hostname(), ".")
	if _, known := publicsuffix.PublicSuffix(suffix); !hasDot || !known {
		return "", false
	}
	return canonical, true
}

// DomainStem returns the canonical hostname without its final suffix. An
// unknown suffix is retained only for parsing; it is never corrected or fetched.
func DomainStem(raw string) (string, bool) {
	canonical, ok := completeSource(raw, true)
	if !ok {
		return "", false
	}
	u, _ := url.Parse(canonical)
	stem, _, hasDot := strings.CutLast(u.Hostname(), ".")
	return stem, hasDot && stem != ""
}

func completeSource(raw string, unknownSuffix bool) (string, bool) {
	raw = strings.TrimSpace(raw)
	match := addresses.FindStringSubmatchIndex(raw)
	if match == nil || match[2] != 0 || match[3] != len(raw) || strings.Contains(raw, "#") {
		return "", false
	}
	canonical, ok := normalizeAddress(raw, unknownSuffix)
	if !ok || strings.ContainsFunc(canonical, unicode.IsSpace) {
		return "", false
	}
	u, err := url.Parse(canonical)
	if err != nil || u.User != nil || u.Opaque != "" {
		return "", false
	}
	if _, err := netip.ParseAddr(u.Hostname()); err == nil {
		return "", false
	}
	return canonical, true
}

func transliterate(label string) string {
	const letters = "абвгдеёжзийклмнопрстуфхцчшщъыьэюя"
	latin := [...]string{"a", "b", "v", "g", "d", "e", "e", "zh", "z", "i", "y", "k", "l", "m", "n", "o", "p", "r", "s", "t", "u", "f", "kh", "ts", "ch", "sh", "shch", "", "y", "", "e", "yu", "ya"}
	var out strings.Builder
	for _, r := range label {
		if i := strings.IndexRune(letters, r); i >= 0 {
			out.WriteString(latin[utf8.RuneCountInString(letters[:i])])
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// MusicRequest excludes ordinary Yandex music and negated playback commands.
func MusicRequest(text string) bool {
	lower := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))
	for _, negative := range []string{"не включ", "не проигр", "не воспроиз", "не став", "не постав", "не запус", "не сыгр", "не надо", "не нужно", "не хочу"} {
		if strings.Contains(lower, negative) {
			return false
		}
	}
	if !playbackVerb.MatchString(lower) || !musicTerm.MatchString(lower) {
		return false
	}
	return strings.Contains(lower, "сайт") || strings.Contains(lower, "страниц") || absoluteAddress.MatchString(Normalize(text))
}
