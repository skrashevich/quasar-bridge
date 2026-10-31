package bridge

import (
	"context"
	"quasarbridge/internal/playback"
	"strconv"
	"strings"
)

// AirPlayController controls the process-wide receiver, independently of the LLM.
type AirPlayController interface {
	Control(context.Context, string) (string, error)
}

// AirPlayMediaController retains sender ownership while playback is paused.
type AirPlayMediaController interface {
	Active(context.Context) bool
	ControlMedia(context.Context, string) (string, error)
}

// airPlayCommand intentionally accepts whole requests only. Explanations, quoted
// commands, negations and compound requests must never start or stop a receiver.
func airPlayCommand(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	text = strings.Trim(text, " ,.!?;:")
	text = strings.Join(strings.Fields(strings.ReplaceAll(text, ",", " ")), " ")
	text = strings.TrimPrefix(text, "алиса ")
	text = strings.TrimPrefix(text, "alice ")
	text = strings.TrimPrefix(text, "пожалуйста ")
	text = strings.TrimPrefix(text, "please ")
	text = strings.TrimSuffix(text, " пожалуйста")
	text = strings.TrimSuffix(text, " please")
	for _, alias := range []string{"эйр плей", "эир плей", "аир плей", "эйрплей", "эирплей", "аирплей", "air play"} {
		text = strings.ReplaceAll(text, alias, "airplay")
	}
	switch text {
	case "включи airplay", "запусти airplay", "активируй airplay", "enable airplay", "start airplay", "turn on airplay":
		return "start"
	case "выключи airplay", "останови airplay", "отключи airplay", "disable airplay", "stop airplay", "turn off airplay":
		return "stop"
	case "статус airplay", "состояние airplay", "проверь airplay", "airplay работает", "airplay включен", "airplay включён", "airplay status", "is airplay on", "is airplay running":
		return "status"
	default:
		return ""
	}
}

func airPlayMediaCommand(text string) string {
	if command := playback.Command(text); command != "" {
		return command
	}
	text = strings.ToLower(strings.Trim(text, " ,.!?"))
	text = strings.Join(strings.Fields(strings.ReplaceAll(text, ",", " ")), " ")
	text = strings.TrimPrefix(text, "алиса ")
	text = strings.TrimPrefix(text, "пожалуйста ")
	text = strings.TrimSuffix(text, " пожалуйста")
	switch text {
	case "громче", "погромче", "сделай громче", "прибавь громкость", "увеличь громкость":
		return "volume_up"
	case "тише", "потише", "сделай тише", "убавь громкость", "уменьши громкость":
		return "volume_down"
	default:
		return airPlayVolumeCommand(text)
	}
}

func airPlayVolumeCommand(text string) string {
	for _, prefix := range []string{"установи ", "поставь ", "сделай "} {
		text = strings.TrimPrefix(text, prefix)
	}
	value, ok := strings.CutPrefix(text, "громкость ")
	if !ok {
		return ""
	}
	value = strings.TrimPrefix(value, "на ")
	percent := false
	for _, suffix := range []string{" процентов", " процента", " процент", "%"} {
		if number, ok := strings.CutSuffix(value, suffix); ok {
			value = strings.TrimSpace(number)
			percent = true
			break
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		words := map[string]int{"ноль": 0, "нуль": 0, "один": 1, "два": 2, "три": 3, "четыре": 4, "пять": 5, "шесть": 6, "семь": 7, "восемь": 8, "девять": 9, "десять": 10}
		var ok bool
		n, ok = words[value]
		if !ok {
			return ""
		}
	}
	if percent {
		if n < 0 || n > 100 {
			return ""
		}
	} else {
		if n < 0 || n > 10 {
			return ""
		}
		n *= 10
	}
	return "volume:" + strconv.Itoa(n)
}
