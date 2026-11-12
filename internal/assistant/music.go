package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/sound"
)

func musicDefinition() llm.Tool {
	return definition("play_music", "Включить музыку на текущей колонке с публичного сайта или по прямой ссылке на аудио. source — точный URL страницы, сайта или аудиофайла из запроса пользователя. Сам находит аудиоссылки в HTML и данных плеера, при необходимости открывает музыкальный раздел сайта и ставит найденные MP3 в очередь. Не придумывай адреса файлов.", map[string]any{"source": map[string]string{"type": "string"}}, []string{"source"})
}

func (t *Tools) playMusic(ctx context.Context, args string) (any, error) {
	var p struct {
		Source string `json:"source"`
	}
	if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil || p.Source == "" || len(p.Source) > 2048 {
		return nil, errors.New("invalid play_music source: provide the page or audio URL")
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
