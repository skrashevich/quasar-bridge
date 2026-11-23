package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"strings"
	"unicode"

	"github.com/skrashevich/quasar-bridge/internal/llm"
	"github.com/skrashevich/quasar-bridge/internal/sound"
)

func soundDefinition() llm.Tool {
	return definition("play_sound", "По явной просьбе пользователя воспроизвести звук на текущей колонке. Обязательный source — строка: notification, success, error или timer для короткого встроенного сигнала; либо точный HTTP/HTTPS URL аудио, предоставленный пользователем или полученный инструментом. Не выдумывай URL. Колонка сама загружает аудио; адрес должен быть ей доступен. status=queued означает постановку в очередь ответа, а не подтверждение слышимого воспроизведения.", map[string]any{
		"source": map[string]any{"type": "string", "maxLength": 2048},
	}, []string{"source"})
}

func playSound(ctx context.Context, args string) (any, error) {
	var p struct {
		Source string `json:"source"`
	}
	invalid := errors.New("invalid play_sound arguments: source must be a built-in signal or HTTP/HTTPS audio URL")
	if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil || p.Source == "" {
		return nil, invalid
	}
	var request sound.Request
	switch p.Source {
	case "notification", "success", "error", "timer":
		request.Sound = p.Source
	default:
		if !validSoundURL(p.Source) {
			return nil, invalid
		}
		request.URL = p.Source
	}
	if err := sound.Play(ctx, request); err != nil {
		return nil, err
	}
	return map[string]any{"status": "queued", "request": request}, nil
}

func validSoundURL(raw string) bool {
	if raw == "" || len(raw) > 2048 || strings.Contains(raw, "#") || strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.Opaque == "" && !strings.ContainsFunc(u.Path, unicode.IsControl)
}
