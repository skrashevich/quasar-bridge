package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"quasarbridge/internal/webaddress"
)

var musicSourceSeparator = regexp.MustCompile(`(?i)(?:^|\s+)(?:со|с|на)(?:\s+(?:сайта|сайте|страницы|странице))?`)

// CanStartMusic recognizes a current explicit command without consulting history.
func (t *Tools) CanStartMusic(text string) bool {
	return t != nil && t.Config.PlaySound && t.Config.Web && t.Playback != nil && musicStartSource(text) != ""
}

func musicStartSource(text string) string {
	source, _, _ := musicStartRequest(text)
	return source
}

// Separate a requested title before normalizing addresses: titles such as
// Love.com are metadata, not another website to fetch.
func musicStartRequest(text string) (source, title string, named bool) {
	rest, ok := webaddress.MusicCommandTail(text)
	if !ok || !webaddress.MusicRequest(text) {
		return "", "", false
	}
	rest, ok = webaddress.MusicTitleTail(rest)
	if !ok {
		return musicRequestSource(text), "", false
	}
	rest = strings.TrimSpace(rest)
	if quoted, tail, ok := musicQuotedTitle(rest); ok {
		if source := musicRequestSource(tail); source != "" {
			return source, quoted, true
		}
		// A quoted URL alone remains an unnamed source.
		if strings.Trim(tail, " .,!?:;«»\"") == "" {
			return musicRequestSource(quoted), "", false
		}
		return "", "", false
	}
	separators := musicSourceSeparator.FindAllStringIndex(rest, -1)
	for i := len(separators) - 1; i >= 0; i-- {
		separator := separators[i]
		if separator[1] < len(rest) {
			next, _ := utf8.DecodeRuneInString(rest[separator[1]:])
			if !unicode.IsSpace(next) {
				continue
			}
		}
		if source := musicRequestSource(rest[separator[1]:]); source != "" {
			title := strings.TrimSpace(rest[:separator[0]])
			return source, title, title != ""
		}
	}
	if address := explicitURL.FindStringIndex(rest); address != nil {
		title := strings.TrimSpace(rest[:address[0]])
		return musicRequestSource(rest[address[0]:]), title, title != ""
	}
	// Bare written or spoken domains are generic sources only when no title
	// text surrounds the normalized address.
	normalized := webaddress.Normalize(rest)
	if source := musicRequestSource(rest); source != "" && strings.Trim(normalized, " .,!?:;«»\"'") == source {
		return source, "", false
	}
	return "", "", false
}

func musicRequestSource(text string) string {
	source := strings.TrimRight(explicitURL.FindString(webaddress.Normalize(text)), ".,!?;:)]}")
	u, err := url.Parse(source)
	if err != nil || len(source) > 2048 || validateWebURL(u) != nil || u.Fragment != "" {
		return ""
	}
	return source
}

func musicQuotedTitle(text string) (title, tail string, ok bool) {
	quote, size := utf8.DecodeRuneInString(text)
	var closing rune
	switch quote {
	case '\'', '"':
		closing = quote
	case '«':
		closing = '»'
	case '“':
		closing = '”'
	case '„':
		closing = '“'
	case '‘':
		closing = '’'
	default:
		return "", "", false
	}
	end := strings.IndexRune(text[size:], closing)
	if end < 0 {
		return "", "", false
	}
	end += size
	return text[size:end], text[end+utf8.RuneLen(closing):], true
}

func (t *Tools) StartMusic(ctx context.Context, text string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !t.CanStartMusic(text) {
		return "", errors.New("music playback requires an explicit current command and website URL")
	}
	source, title, named := musicStartRequest(text)
	params := map[string]string{"source": source}
	if named {
		params["title"] = title
	}
	args, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	if _, err := t.Call(ctx, "play_music", string(args)); err != nil {
		switch {
		case errors.Is(err, errMusicTitleNotFound):
			return "Не нашёл на сайте трек с таким названием.", nil
		case errors.Is(err, errMusicTitleAmbiguous):
			return "На сайте несколько треков с таким названием. Укажите прямую ссылку на нужный трек.", nil
		}
		return "", err
	}
	if named {
		return "Включаю трек.", nil
	}
	return "Включаю музыку.", nil
}
