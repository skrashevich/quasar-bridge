package bridge

import (
	"strings"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/webaddress"
)

const musicSourcePrompt = "Не удалось разобрать адрес сайта. Произнеси его по буквам вместе с доменной зоной."

func (e *Engine) needsMusicSource(text string) bool {
	if _, _, _, explicit := websiteMusicSource(text); !explicit {
		return false
	}
	requester, ok := e.Music.(interface{ NeedsMusicSource(string) bool })
	return ok && requester.NeedsMusicSource(text)
}

func musicReplySource(text string) string {
	raw := strings.TrimSpace(text)
	for _, prefix := range []string{"адрес ", "сайт "} {
		if strings.HasPrefix(strings.ToLower(raw), prefix) {
			raw = strings.TrimSpace(raw[len(prefix):])
			break
		}
	}
	return strings.TrimRight(raw, ".,!?:;")
}

func musicSourceReply(text string) string {
	source, ok := webaddress.CanonicalSource(musicReplySource(text))
	if !ok {
		return ""
	}
	return "Включи музыку с сайта " + source
}

func (e *Engine) musicSourceDecision(text string) (RouteDecision, bool) {
	switch dialogue.Normalize(text) {
	case "нет", "отмена", "не надо", "стоп":
		return RouteDecision{Route: "music", Source: "music_source_cancel"}, true
	}
	if e.Music == nil {
		return RouteDecision{}, false
	}
	if _, _, _, explicit := websiteMusicSource(text); explicit {
		return RouteDecision{}, false
	}
	if command := musicSourceReply(text); command != "" && e.Music.CanStartMusic(command) {
		return RouteDecision{Route: "music", Source: "music_source_reply"}, true
	}
	// Retry only an address-shaped reply, never a new command or ordinary question.
	if _, addressShaped := webaddress.DomainStem(musicReplySource(text)); addressShaped {
		return RouteDecision{Route: "music", Source: "music_source_prompt"}, true
	}
	return RouteDecision{}, false
}
