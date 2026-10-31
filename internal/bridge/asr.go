package bridge

import (
	"net/url"
	"regexp"
	"strings"

	recognition "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/webaddress"
)

const asrHypothesisLimit = 8

var websiteMusicCommand = regexp.MustCompile(`(?i)^(\s*(?:(?:алиса|пожалуйста|спроси нейросеть|спроси свою модель)[\s,;:]+)*(?:включи(?:те)?|проиграй(?:те)?|воспроизведи(?:те)?|поставь(?:те)?|запусти(?:те)?|сыграй(?:те)?)\s+(?:музыку|аудио|песню|песни|трек|треки|радио|звук)\s+(?:с\s+сайта|со\s+страницы|с|со)\s+)(.+?)([.!?,;:]*\s*)$`)
var sourceNegation = regexp.MustCompile(`(?i)(?:^|\s)(?:не|без)(?:\s|$)`)
var sourceSlashes = regexp.MustCompile(`(?i)\s+(?:слэш|слеш|slash)\s+`)

func recognitionText(r *recognition.TAsrRecognition) string {
	if text := r.GetNormalized(); text != "" {
		return text
	}
	var words []string
	for _, word := range r.GetWords() {
		words = append(words, word.GetValue())
	}
	return strings.Join(words, " ")
}

// websiteMusicSource separates only a current, explicit playback command.
func websiteMusicSource(text string) (prefix, source, suffix string, ok bool) {
	match := websiteMusicCommand.FindStringSubmatch(text)
	if match == nil || sourceNegation.MatchString(match[2]) {
		return "", "", "", false
	}
	return match[1], match[2], match[3], true
}

// sourceTail keeps ASR correction from changing the requested path or query.
func sourceTail(source string) (scheme, tail string) {
	source = sourceSlashes.ReplaceAllString(source, "/")
	scheme = "https"
	if before, after, ok := strings.Cut(source, "://"); ok {
		scheme = strings.ToLower(before)
		source = after
	}
	head := source
	if i := strings.IndexAny(source, "/?#"); i >= 0 {
		head, tail = source[:i], source[i:]
	}
	if u, err := url.Parse(tail); err == nil {
		tail = u.String()
	}
	if _, port, ok := strings.Cut(head, ":"); ok {
		tail = ":" + port + tail
	}
	return scheme, tail
}

func selectASRText(hypotheses []*recognition.TAsrRecognition) string {
	if len(hypotheses) == 0 {
		return ""
	}
	top := recognitionText(hypotheses[0])
	if len(hypotheses) > asrHypothesisLimit {
		return top
	}
	prefix, source, suffix, ok := websiteMusicSource(top)
	if !ok {
		return top
	}
	if _, reliable := webaddress.CanonicalSource(source); reliable {
		return top
	}
	stem, ok := webaddress.DomainStem(source)
	if !ok {
		return top
	}
	scheme, tail := sourceTail(source)
	selected := ""
	for _, candidate := range hypotheses[1:min(len(hypotheses), asrHypothesisLimit)] {
		nextPrefix, nextSource, nextSuffix, ok := websiteMusicSource(recognitionText(candidate))
		if !ok || strings.Join(strings.Fields(dialogue.Normalize(nextPrefix)), " ") != strings.Join(strings.Fields(dialogue.Normalize(prefix)), " ") || strings.TrimSpace(nextSuffix) != strings.TrimSpace(suffix) {
			continue
		}
		nextScheme, nextTail := sourceTail(nextSource)
		if nextScheme != scheme || nextTail != tail {
			continue
		}
		nextStem, ok := webaddress.DomainStem(nextSource)
		if !ok || nextStem != stem {
			continue
		}
		canonical, ok := webaddress.CanonicalSource(nextSource)
		if !ok {
			continue
		}
		if selected != "" && selected != canonical {
			return top
		}
		selected = canonical
	}
	if selected == "" {
		return top
	}
	return prefix + selected + suffix
}
