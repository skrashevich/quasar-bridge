package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"regexp"
	"strings"
	"time"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/update"
	"quasarbridge/internal/webaddress"
)

type BridgeUpdater interface {
	Check(context.Context) (update.Status, error)
	Install(context.Context) (update.InstallResult, error)
}

type Tools struct {
	Updater       BridgeUpdater
	Config        Config
	JournalConfig func() journal.Config
	WebReader     *WebReader
	Playback      *MusicPlayback
	Now           func() time.Time
}

func definition(name, description string, properties map[string]any, required []string) llm.Tool {
	return llm.Tool{Type: "function", Function: llm.Function{Name: name, Description: description, Parameters: map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}}
}
func (t *Tools) Definitions() []llm.Tool {
	var tools []llm.Tool
	if t.Config.Updates && t.Updater != nil {
		tools = append(tools,
			definition("check_quasar_bridge_update", "Проверить на GitHub последний стабильный выпуск Quasar Bridge, сравнить с текущей версией и узнать возможность автоматической установки. Не меняет программу.", map[string]any{}, []string{}),
			definition("install_quasar_bridge_update", "Установить последнюю стабильную версию Quasar Bridge с GitHub. Вызывать только по явной просьбе пользователя обновить Quasar Bridge. Проверяет SHA-256 и сохраняет настройки. Только для самостоятельной установки на колонке. scheduled означает, что установка запланирована и bridge скоро перезапустится; это ещё не подтверждение успешного обновления. Не принимает URL или номер версии.", map[string]any{}, []string{}))
	}
	if t.Config.Web {
		tools = append(tools, definition("read_webpage", "Прочитать публичную HTTP/HTTPS страницу по URL. Возвращает текст без JavaScript. Не является поиском в интернете.", map[string]any{"url": map[string]string{"type": "string"}}, []string{"url"}))
	}
	if t.Config.History {
		tools = append(tools, definition("search_history", "Поиск сохранённых запросов колонки и доставленных ответов. query — ключевые слова (все должны встретиться), пустая строка — последние запросы. since/until — необязательные RFC3339 (until не включительно), limit от 1 до 20. Результаты от новых к старым.", map[string]any{"query": map[string]string{"type": "string"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}, "since": map[string]string{"type": "string"}, "until": map[string]string{"type": "string"}}, []string{"query"}))
	}
	if t.Config.Time {
		tools = append(tools, definition("current_time", "Текущие дата, время и часовой пояс пользователя.", map[string]any{}, []string{}))
	}
	if t.Config.PlaySound {
		tools = append(tools, soundDefinition())
		if t.Config.Web && t.Playback != nil {
			tools = append(tools, musicDefinition())
		}
	}
	return tools
}
func (t *Tools) Call(ctx context.Context, name, args string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(args) > 16000 {
		return nil, errors.New("tool arguments exceed limit")
	}
	switch {
	case (name == "check_quasar_bridge_update" || name == "install_quasar_bridge_update") && t.Config.Updates && t.Updater != nil:
		var p struct{}
		if !strings.HasPrefix(strings.TrimSpace(args), "{") || json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil {
			return nil, errors.New("update tools take an empty argument object")
		}
		if name == "check_quasar_bridge_update" {
			return t.Updater.Check(ctx)
		}
		return t.Updater.Install(ctx)
	case name == "play_music" && t.Config.PlaySound && t.Config.Web && t.Playback != nil:
		return t.playMusic(ctx, args)
	case name == "play_sound" && t.Config.PlaySound:
		return playSound(ctx, args)
	case name == "read_webpage" && t.Config.Web:
		var p struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil || p.URL == "" {
			return nil, errors.New("read_webpage requires a URL")
		}
		return t.WebReader.Read(ctx, p.URL)
	case name == "search_history" && t.Config.History:
		var p HistoryQuery
		if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil {
			return nil, errors.New("invalid search_history arguments")
		}
		result, err := SearchHistory(ctx, t.JournalConfig(), p, t.Config.HistoryMaxScanMB)
		if err != nil {
			return nil, err
		}
		loc, err := time.LoadLocation(t.Config.Timezone)
		if err != nil {
			return nil, errors.New("configured timezone unavailable")
		}
		for i := range result.Items {
			result.Items[i].Time = result.Items[i].Time.In(loc)
		}
		result.Timezone = t.Config.Timezone
		return result, nil
	case name == "current_time" && t.Config.Time:
		var p struct{}
		if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil {
			return nil, errors.New("current_time takes no arguments")
		}
		loc, err := time.LoadLocation(t.Config.Timezone)
		if err != nil {
			return nil, errors.New("configured timezone unavailable")
		}
		now := time.Now()
		if t.Now != nil {
			now = t.Now()
		}
		now = now.In(loc)
		return map[string]string{"time": now.Format(time.RFC3339), "weekday": now.Weekday().String(), "timezone": t.Config.Timezone}, nil
	default:
		return nil, errors.New("unknown or disabled tool")
	}
}

// InitialCalls supplies cheap local context without an extra model round trip.
var explicitURL = regexp.MustCompile(`(?i)https?://[^\s<>"'«»]+`)

func historyQuestion(text string) bool {
	text = dialogue.Normalize(text)
	for _, marker := range []string{"в истории", "истории сообщени", "истории диалога", "истории запросов", "историю запросов", "что я спрашивал", "когда я спрашивал", "о чем мы", "что мы обсуждали", "что ты отвечал", "что ты ответил", "что я просил", "когда я просил"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
func (t *Tools) InitialCalls(text string) []llm.FunctionCall { return t.initialCalls(text, false) }

// Followups inherit the topic, but temporal filters always come from this turn.
func (t *Tools) InitialCallsForConversation(messages []llm.Message) []llm.FunctionCall {
	last := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			last = i
			break
		}
	}
	if last < 0 {
		return t.InitialCalls("")
	}
	text := messages[last].Content
	history := false
	if dialogue.IsFollowup(text) {
		for i := last - 1; i >= 0; i-- {
			if messages[i].Role == "user" && !dialogue.IsFollowup(messages[i].Content) {
				history = historyQuestion(messages[i].Content)
				break
			}
		}
	}
	return t.initialCalls(text, history)
}
func (t *Tools) initialCalls(text string, history bool) []llm.FunctionCall {
	if strings.Contains(strings.ToLower(text), "сайт") || webaddress.MusicRequest(text) {
		text = webaddress.Normalize(text)
	}
	original := text
	var calls []llm.FunctionCall
	if t.Config.Time {
		calls = append(calls, llm.FunctionCall{Name: "current_time", Arguments: "{}"})
	}
	history = history || historyQuestion(text)
	text = dialogue.Normalize(text)
	// A request that explicitly combines a page with history must read the page first.
	web := strings.Contains(text, "https://") || strings.Contains(text, "http://") || strings.Contains(text, "страниц") || strings.Contains(text, "сайт")
	playback := t.Config.PlaySound && (webaddress.MusicRequest(original) || strings.Contains(text, "воспроизведи") || strings.Contains(text, "проиграй") || strings.Contains(text, "включи аудио"))
	if t.Config.Web && !playback {
		if raw := explicitURL.FindString(original); raw != "" {
			args, _ := json.Marshal(map[string]string{"url": strings.TrimRight(raw, ".,;")})
			calls = append(calls, llm.FunctionCall{Name: "read_webpage", Arguments: string(args)})
			return calls
		}
	}
	if t.Config.History && history && !web {
		query := HistoryQuery{Limit: 8}
		if strings.Contains(text, "сегодня") || strings.Contains(text, "вчера") {
			loc, err := time.LoadLocation(t.Config.Timezone)
			if err == nil {
				now := time.Now()
				if t.Now != nil {
					now = t.Now()
				}
				now = now.In(loc)
				start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
				if strings.Contains(text, "вчера") {
					query.Until = start.Format(time.RFC3339)
					days := -1
					if strings.Contains(text, "позавчера") {
						days = -2
						query.Until = start.AddDate(0, 0, -1).Format(time.RFC3339)
					}
					start = start.AddDate(0, 0, days)
				}
				query.Since = start.Format(time.RFC3339)
			}
		}
		args, _ := json.Marshal(query)
		calls = append(calls, llm.FunctionCall{Name: "search_history", Arguments: string(args)})
	}
	return calls
}
