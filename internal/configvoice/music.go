package configvoice

import (
	"regexp"
	"strings"
)

var defaultMusicCommand = regexp.MustCompile(`^(?:включи|используй|сделай|установи|поставь|выбери|верни)\s+(sound\s*cloud|саунд\s*клауд(?:е|а)?|саунд\s*клоуд(?:е|а)?|яндекс(?:\s+музык[ау])?|yandex(?:\s+music)?|spotify|спотифай|apple\s+music|эппл\s+музык[ау])(?:\s+(?:музыкальным\s+сервисом|музыкальным\s+провайдером|сервисом|провайдером))?\s+по\s+умолчанию$`)

// MusicDefault recognizes a complete preference command, never an arbitrary
// mention inside a song title, explanation or negated request.
func MusicDefault(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	text = strings.Trim(text, " .,!?")
	text = strings.TrimPrefix(text, "алиса, ")
	text = strings.TrimPrefix(text, "алиса ")
	text = strings.TrimSuffix(text, ", пожалуйста")
	text = strings.TrimSuffix(text, " пожалуйста")
	text = strings.Join(strings.Fields(text), " ")
	switch text {
	case "какой музыкальный сервис по умолчанию", "какой музыкальный провайдер по умолчанию", "какая музыка по умолчанию", "что используется для музыки по умолчанию":
		return "status"
	}
	m := defaultMusicCommand.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	name := strings.ReplaceAll(m[1], " ", "")
	switch {
	case strings.HasPrefix(name, "soundcloud"), strings.HasPrefix(name, "саундклауд"), strings.HasPrefix(name, "саундклоуд"):
		return "soundcloud"
	case strings.HasPrefix(name, "яндекс"), strings.HasPrefix(name, "yandex"):
		return "yandex"
	default:
		return "unsupported"
	}
}
