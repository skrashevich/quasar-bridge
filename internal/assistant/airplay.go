package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
	"strconv"
	"strings"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/reply"
)

// AirPlayStatus distinguishes receiver availability from sender playback.
type AirPlayStatus struct {
	Enabled   bool    `json:"enabled"`
	State     string  `json:"state"`
	Name      string  `json:"name"`
	Connected bool    `json:"connected"`
	Playing   bool    `json:"playing"`
	VolumeDB  float64 `json:"volume_db"`
	Error     string  `json:"error,omitempty"`
}

type AirPlayController interface {
	Status(context.Context) (AirPlayStatus, error)
	Control(context.Context, string) (string, error)
	ControlMedia(context.Context, string) (string, error)
}

var airPlayPlaybackActions = []string{"pause", "resume", "stop", "next", "previous", "restart", "volume_up", "volume_down"}

func airPlayDefinitions() []llm.Tool {
	return []llm.Tool{
		definition("get_airplay_status", "Узнать состояние AirPlay-приёмника этой колонки: state — stopped/starting/running/stopping, name — имя для выбора на устройстве, connected — подключён ли отправитель, playing — идёт ли воспроизведение, volume_db — громкость AirPlay. enabled означает доступность функции, а не включённый приёмник. running не означает, что отправитель подключён или музыка играет. Не изменяет состояние.", map[string]any{}, []string{}),
		definition("control_airplay", "Включить (start) или выключить (stop) AirPlay-приёмник этой колонки по просьбе пользователя. После start пользователь должен выбрать колонку в списке AirPlay на своём устройстве. Инструмент не выбирает музыку на телефоне. stop отключает приёмник; для остановки только воспроизведения используй control_airplay_playback. Результат содержит фактический status после команды.", map[string]any{"action": map[string]any{"type": "string", "enum": []string{"start", "stop"}}}, []string{"action"}),
		definition("control_airplay_playback", "Управлять воспроизведением подключённого AirPlay-отправителя: pause — пауза, resume — продолжить, stop — остановить воспроизведение, next/previous — следующий/предыдущий трек, restart — трек сначала, volume_up/volume_down — громче/тише. Требуется connected=true; переключение треков и воспроизведение зависят от поддержки удалённого управления отправителем. stop сохраняет AirPlay-приёмник включённым. Не выбирает песни или приложения на телефоне. Результат содержит фактический status после команды.", map[string]any{"action": map[string]any{"type": "string", "enum": slices.Clone(airPlayPlaybackActions)}}, []string{"action"}),
		definition("set_airplay_volume", "Установить громкость AirPlay-аудио этой колонки: volume — целое число процентов от 0 (без звука) до 100. Требуется подключённый AirPlay-отправитель. Системную громкость колонки не меняет. Результат содержит фактический status после команды.", map[string]any{"volume": map[string]any{"type": "integer", "minimum": 0, "maximum": 100}}, []string{"volume"}),
	}
}

func (t *Tools) callAirPlay(ctx context.Context, name, args string) (any, error) {
	if !strings.HasPrefix(strings.TrimSpace(args), "{") {
		return nil, errors.New("AirPlay arguments must be a JSON object")
	}
	var message string
	var err error
	switch name {
	case "get_airplay_status":
		var p struct{}
		if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil {
			return nil, errors.New("get_airplay_status takes an empty argument object")
		}
		status, err := t.AirPlay.Status(ctx)
		if err != nil {
			return nil, err
		}
		return status, nil
	case "control_airplay", "control_airplay_playback":
		var p struct {
			Action string `json:"action"`
		}
		if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil {
			return nil, errors.New("invalid AirPlay action arguments")
		}
		if name == "control_airplay" {
			if p.Action != "start" && p.Action != "stop" {
				return nil, errors.New("control_airplay action must be start or stop")
			}
			message, err = t.AirPlay.Control(ctx, p.Action)
			if err == nil && p.Action == "start" {
				reply.SuppressListening(ctx)
			}
		} else {
			if !slices.Contains(airPlayPlaybackActions, p.Action) {
				return nil, errors.New("unknown AirPlay playback action")
			}
			message, err = t.AirPlay.ControlMedia(ctx, p.Action)
		}
	case "set_airplay_volume":
		var p struct {
			Volume *int `json:"volume"`
		}
		if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil || p.Volume == nil || *p.Volume < 0 || *p.Volume > 100 {
			return nil, errors.New("set_airplay_volume requires an integer volume from 0 to 100")
		}
		message, err = t.AirPlay.ControlMedia(ctx, "volume:"+strconv.Itoa(*p.Volume))
	}
	if err != nil {
		return nil, err
	}
	status, err := t.AirPlay.Status(ctx)
	if err != nil {
		return nil, err
	}
	return struct {
		Message string        `json:"message"`
		Status  AirPlayStatus `json:"status"`
	}{Message: message, Status: status}, nil
}
