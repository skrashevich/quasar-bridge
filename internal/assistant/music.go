package assistant

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"unicode/utf8"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/sound"
)

func musicDefinition() llm.Tool {
	return definition("play_music", "Включить музыку на текущей колонке с публичного сайта или по прямой ссылке на аудио. source — точный URL страницы, сайта или аудиофайла из запроса пользователя. Если пользователь назвал конкретный трек, передай его точное название в title: будет выбран только один совпавший трек, без подбора замены. Без title сам находит аудиоссылки и ставит найденные MP3 в очередь. Не придумывай адреса файлов.", map[string]any{
		"source": map[string]string{"type": "string"},
		"title":  map[string]any{"type": "string", "minLength": 1, "maxLength": maxMusicTitleRunes},
	}, []string{"source"})
}

func (t *Tools) playMusic(ctx context.Context, args string) (any, error) {
	var p struct {
		Source string         `json:"source"`
		Title  jsontext.Value `json:"title"`
	}
	if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil || p.Source == "" || len(p.Source) > 2048 {
		return nil, errors.New("invalid play_music source: provide the page or audio URL")
	}
	var title string
	if len(p.Title) > 0 && (json.Unmarshal(p.Title, &title) != nil || utf8.RuneCountInString(title) > maxMusicTitleRunes || normalizeMusicTitle(title) == "") {
		return nil, errors.New("invalid play_music title: provide a nonempty track title of at most 256 characters")
	}
	raw, err := normalizeMusicSource(p.Source)
	if err != nil {
		return nil, err
	}
	if !sound.HasPlayer(ctx) {
		return nil, errors.New("play_music requires a current speaker request")
	}
	resolution, err := t.WebReader.ResolveMusic(ctx, raw)
	if err != nil {
		return nil, err
	}
	tracks := resolution.Tracks
	if title != "" {
		tracks, err = selectMusicTitle(tracks, title)
		if err != nil {
			return nil, err
		}
	}
	if err := t.Playback.Start(ctx, tracks); err != nil {
		return nil, err
	}
	return map[string]any{"status": "queued", "track_count": len(tracks), "first_track": tracks[0].Title}, nil
}

func musicNavigation(link WebLink) bool {
	text := strings.ToLower(link.Title + " " + link.URL)
	for _, marker := range []string{"music", "audio", "songs", "playlist", "музык", "песн", "аудио"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
