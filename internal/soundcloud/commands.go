package soundcloud

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type providerCommand struct {
	action, query  string
	kind           Kind
	explicit, find bool
	number         int
	offset         int64
	relative       bool
}

var providerName = regexp.MustCompile(`(?i)\b(?:sound\s*cloud)\b|саунд\s*клауд(?:е|e|а)?|саунд\s*клоуд(?:е|а)?`)
var providerWebsite = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*\.)+(?:(?:com|ru|net|org|io|me|fm|tv)(?:[/:?#].*)?|[a-z]{2,}/.*)$`)
var seekCommand = regexp.MustCompile(`^(?:перемотай|перемотка|seek)(?:\s+(вперед|вперёд|назад|forward|back))?(?:\s+на)?\s+(\d+)(?:\s*(секунд[уы]?|сек|seconds?|минут[уы]?|мин|minutes?))?$`)
var selectionCommand = regexp.MustCompile(`^(?:(?:включи|поставь|играй|play)\s+)?(?:(?:трек|песню|номер|результат|track)\s+)?(\d+)$`)

func parseProviderCommand(text string) providerCommand {
	original := strings.TrimSpace(text)
	text = strings.ToLower(original)
	text = strings.Trim(text, " .,!?")
	text = strings.TrimPrefix(text, "алиса, ")
	c := providerCommand{kind: TrackKind}
	if text == "" {
		return c
	}
	// Reject the request intent, not words inside a song or artist title.
	for _, prefix := range []string{"не включай", "не играй", "не ставь", "не надо", "не хочу", "don't ", "do not ", "что такое", "как работает", "расскажи", "почему", "зачем", "можно ли", "умеешь ли", "how does", "what is soundcloud"} {
		if text == prefix || strings.HasPrefix(text, prefix+" ") || strings.HasSuffix(prefix, " ") && strings.HasPrefix(text, prefix) {
			return c
		}
	}
	for _, other := range []string{" на яндекс", " в яндекс", " on yandex", " на airplay", " на эйрплей", " на spotify", " on spotify", " на ютуб", " на youtube"} {
		if strings.Contains(text, other) {
			return c
		}
	}
	// Parse links before removing the provider name: it is also the domain name.
	for word := range strings.FieldsSeq(original) {
		normalizedWord := strings.ToLower(word)
		if strings.Contains(normalizedWord, "://") || strings.HasPrefix(normalizedWord, "www.") || providerWebsite.MatchString(normalizedWord) {
			raw := word
			if !strings.Contains(raw, "://") {
				raw = "https://" + raw
			}
			u, err := url.Parse(raw)
			if err != nil {
				return providerCommand{}
			}
			u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
			if u.Scheme != "https" || u.User != nil || u.Port() != "" || (u.Hostname() != "soundcloud.com" && u.Hostname() != "www.soundcloud.com" && u.Hostname() != "on.soundcloud.com") {
				return providerCommand{}
			}
			c.action, c.query, c.explicit = "resolve", u.String(), true
			return c
		}
	}
	c.explicit = providerName.MatchString(text)
	text = providerName.ReplaceAllString(text, "")
	text = strings.Join(strings.Fields(text), " ")
	for _, suffix := range []string{" на", " в", " on", " from"} {
		text = strings.TrimSuffix(text, suffix)
	}
	text = strings.TrimSpace(text)
	switch text {
	case "что играет", "что сейчас играет", "какой трек играет", "какая песня играет", "название песни", "название трека", "что за песня", "что за трек", "что это за песня", "текущий трек", "current track", "what is playing", "what's playing":
		c.action = "info"
	case "кто поет", "кто поёт", "кто исполняет", "кто исполнитель", "назови исполнителя", "who sings this", "artist":
		c.action = "artist_info"
	case "следующий", "следующая", "следующий трек", "следующая песня", "переключи трек", "дальше", "next", "next track", "skip":
		c.action = "next"
	case "предыдущий", "предыдущая", "предыдущий трек", "предыдущая песня", "назад", "previous", "previous track", "prev":
		c.action = "previous"
	case "пауза", "на паузу", "поставь на паузу", "приостанови музыку", "pause":
		c.action = "pause"
	case "продолжи", "продолжай", "продолжи музыку", "продолжи воспроизведение", "сними с паузы", "resume", "continue":
		c.action = "resume"
	case "стоп", "останови", "останови музыку", "выключи музыку", "stop", "stop music":
		c.action = "stop"
	case "сначала", "с начала", "начни сначала", "включи сначала", "перезапусти трек", "restart":
		c.action = "restart"
	case "повтор", "повтори трек", "повторяй трек", "повторяй песню", "включи повтор", "repeat", "repeat one":
		c.action = "repeat_one"
	case "повторяй все", "повторяй всё", "повторяй очередь", "repeat all":
		c.action = "repeat_all"
	case "выключи повтор", "отключи повтор", "без повтора", "repeat off":
		c.action = "repeat_off"
	case "перемешай", "перемешай очередь", "в случайном порядке", "включи случайный порядок", "shuffle", "shuffle on":
		c.action = "shuffle_on"
	case "не перемешивай", "выключи случайный порядок", "отключи случайный порядок", "shuffle off":
		c.action = "shuffle_off"
	case "лайк", "мне нравится", "лайкни", "добавь в избранное", "like", "like this":
		c.action = "like"
	case "убери лайк", "удали из избранного", "убери из избранного", "мне не нравится", "unlike":
		c.action = "unlike"
	case "похожее", "похожие треки", "включи похожее", "включи похожую музыку", "similar", "related", "play similar":
		c.action = "related"
	}
	if c.action != "" {
		return c
	}
	if m := seekCommand.FindStringSubmatch(text); m != nil {
		n, err := strconv.ParseInt(m[2], 10, 32)
		if err != nil {
			return c
		}
		if strings.HasPrefix(m[3], "мин") || strings.HasPrefix(m[3], "min") {
			n *= 60
		}
		c.action, c.offset, c.relative = "seek", n*1000, m[1] != ""
		if m[1] == "назад" || m[1] == "back" {
			c.offset = -c.offset
		}
		return c
	}
	if m := selectionCommand.FindStringSubmatch(text); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil && n > 0 {
			c.action, c.number = "select", n
		}
		return c
	}
	hasVerb, musicIntent := false, false
	for _, prefix := range []string{"найди ", "найти ", "поищи ", "ищи ", "search for ", "search ", "find "} {
		if q, ok := strings.CutPrefix(text, prefix); ok {
			c.find = true
			text = q
			hasVerb = true
			break
		}
	}
	if !hasVerb {
		for _, prefix := range []string{"включи ", "поставь ", "сыграй ", "играй ", "запусти ", "play "} {
			if q, ok := strings.CutPrefix(text, prefix); ok {
				text = q
				hasVerb = true
				musicIntent = prefix == "сыграй " || prefix == "играй " || prefix == "play "
				break
			}
		}
	}
	if c.explicit && (text == "включи" || text == "поставь" || text == "play" || text == "") {
		text = "музыку"
		hasVerb = true
	}
	if !hasVerb {
		return providerCommand{}
	}
	for _, prefix := range []string{"на ", "в ", "on ", "from "} {
		text = strings.TrimPrefix(text, prefix)
	}
	if !c.explicit {
		for _, source := range []string{"airplay", "эйрплей", "яндекс", "yandex", "spotify", "youtube", "ютуб"} {
			if text == source || strings.HasPrefix(text, source+" ") {
				return providerCommand{}
			}
		}
	}
	c.action = "search"
	switch text {
	case "мои лайки", "избранное", "любимые треки", "любимую музыку", "liked music", "my likes":
		c.action = "likes"
		return c
	}
	for _, group := range []struct {
		kind     Kind
		prefixes []string
	}{
		{ArtistKind, []string{"исполнителя ", "артиста ", "песни исполнителя ", "музыку исполнителя ", "artist "}},
		{AlbumKind, []string{"альбом ", "album "}}, {PlaylistKind, []string{"плейлист ", "playlist "}},
		{TrackKind, []string{"песню ", "песни ", "трек ", "композицию ", "музыку ", "song ", "track ", "music "}},
	} {
		for _, prefix := range group.prefixes {
			if q, ok := strings.CutPrefix(text, prefix); ok {
				c.kind, text, musicIntent = group.kind, q, true
				goto query
			}
		}
	}
query:
	switch text {
	case "музыку", "музыка", "песню", "трек", "music":
		text, musicIntent = "", true
	}
	// Default music routing needs a musical kind/phrase or an unambiguous
	// music verb. Ordinary home imperatives must keep their existing route.
	if !c.explicit && !musicIntent {
		return providerCommand{}
	}
	c.query = strings.TrimSpace(text)
	return c
}
