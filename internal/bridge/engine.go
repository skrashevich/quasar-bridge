package bridge

import (
	"context"
	"encoding/json/v2"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/skrashevich/quasar-bridge/internal/configvoice"
	"github.com/skrashevich/quasar-bridge/internal/dialogue"
	"github.com/skrashevich/quasar-bridge/internal/home"
	"github.com/skrashevich/quasar-bridge/internal/llm"
	"github.com/skrashevich/quasar-bridge/internal/playback"
	"github.com/skrashevich/quasar-bridge/internal/reply"
	"github.com/skrashevich/quasar-bridge/internal/webaddress"
)

const defaultSystemPrompt = "Ты голосовой помощник пользователя. Отвечай по-русски, кратко и по существу, обычно до трёх предложений. Ответ будет прочитан вслух: без Markdown и служебных рассуждений. Выполняй внешние действия только доступными инструментами по явному запросу пользователя. Не заявляй о выполнении действий без успешного результата инструмента; если инструмента нет, прямо скажи об этом. Для Home Assistant есть отдельный обработчик умного дома."

type Completer interface {
	Complete(context.Context, []llm.Message, bool) (string, error)
}
type HomeHandler interface {
	Handle(context.Context, string) (string, error)
}

// HomeDialogueHandler is optional. It receives history only for an active clarification.
type HomeDialogueHandler interface {
	HandleDialogue(context.Context, string, []llm.Message) (string, error)
}
type PlaybackController interface {
	Active(context.Context) bool
	Control(context.Context, string) (string, error)
}
type MusicStarter interface {
	CanStartMusic(string) bool
	StartMusic(context.Context, string) (string, error)
}
type Engine struct {
	Configuration bool
	Recovery      func(context.Context, string) (string, error)
	SystemPrompt  string
	Diagnostics   func(context.Context, bool) (string, error)
	AirPlay       AirPlayController
	LLM           Completer
	Assistant     Completer
	Home          HomeHandler
	Playback      PlaybackController
	Music         MusicStarter
	Dialogue      *dialogue.Store
	TTSMarkup     bool
	// StripPrefixes are model addresses removed only from outbound requests.
	// An empty list disables stripping.
	StripPrefixes []string
	dialogueOnce  sync.Once
	routing       atomic.Pointer[RoutingPolicy]
}

func (e *Engine) dialogue() *dialogue.Store {
	e.dialogueOnce.Do(func() {
		if e.Dialogue == nil {
			e.Dialogue = dialogue.New(dialogue.DefaultConfig())
		}
	})
	return e.Dialogue
}

// Prepare reserves this speaker's request before ASR/classification can finish late.
func (e *Engine) Prepare(ctx context.Context) context.Context {
	if ctx.Err() != nil {
		return ctx
	}
	ctx, _, _ = e.dialogue().Start(ctx)
	return ctx
}

func (e *Engine) Resolve(ctx context.Context, text string) (Resolution, error) {
	return e.ResolveDecision(ctx, text, e.Decide(ctx, text))
}
func (e *Engine) ResolveDecision(ctx context.Context, text string, decision RouteDecision) (Resolution, error) {
	route := decision.Route
	if decision.prepared {
		ctx = e.dialogue().Use(ctx, decision.ticket)
	} else {
		ctx = e.Prepare(ctx)
	}
	ctx, snapshot, ticket := e.dialogue().Start(ctx)
	if ctx.Err() != nil {
		return Resolution{Route: route}, ctx.Err()
	}

	if !e.dialogue().Current(ctx, ticket) {
		return Resolution{Route: route}, context.Canceled
	}
	if decision.Source == "dialogue_retry" {
		return e.retryQuestion(ctx, ticket)
	}
	if decision.Source == "dialogue_reset" {
		e.dialogue().ClearReplay(ctx, ticket)
		if !e.dialogue().Clear(ctx, ticket) {
			return Resolution{Route: route}, context.Canceled
		}
		return Resolution{Route: "llm", Answer: "Начинаем новый разговор."}, nil
	}
	if (route != "llm" && route != "home") || (snapshot.Route != "" && snapshot.Route != route) {
		if !e.dialogue().Clear(ctx, ticket) {
			return Resolution{Route: route}, context.Canceled
		}
		snapshot = dialogue.Snapshot{}
	}
	result := Resolution{Route: route}
	switch route {
	case "diagnostics":
		enabled, ok := configvoice.DiagnosticsCommand(text)
		if !ok || e.Diagnostics == nil {
			return result, errors.New("live diagnostics control is unavailable")
		}
		answer, err := e.Diagnostics(ctx, enabled)
		result.Answer = answer
		return result, err
	case "recovery":
		action := configvoice.RecoveryAction(text)
		if action == "" || e.Recovery == nil {
			return result, errors.New("emergency recovery is unavailable")
		}
		answer, err := e.Recovery(ctx, action)
		if err == nil {
			reply.SuppressListening(ctx)
		}
		result.Answer = answer
		return result, err
	case "airplay":
		if e.AirPlay == nil {
			return Resolution{Route: "yandex"}, nil
		}
		action := airPlayCommand(text)
		if decision.Source == "airplay_media" {
			controller, ok := e.AirPlay.(AirPlayMediaController)
			if !ok || !controller.Active(ctx) {
				return Resolution{Route: "yandex"}, nil
			}
			action = airPlayMediaCommand(text)
			if action == "" {
				return result, errors.New("unknown AirPlay media control")
			}
			answer, err := controller.ControlMedia(ctx, action)
			result.Answer = answer
			return result, err
		}
		if action == "" {
			return result, errors.New("unknown AirPlay control")
		}
		answer, err := e.AirPlay.Control(ctx, action)
		if err == nil && action == "start" {
			reply.SuppressListening(ctx)
		}
		result.Answer = answer
		return result, err
	case "music":
		if e.Music == nil {
			return result, errors.New("music playback is unavailable")
		}
		answer, err := e.Music.StartMusic(ctx, text)
		if !e.dialogue().Current(ctx, ticket) {
			return result, context.Canceled
		}
		result.Answer = answer
		return result, err
	case "playback":
		if e.Playback == nil {
			return Resolution{Route: "yandex"}, nil
		}
		action := playback.Command(text)
		if action == "" {
			return result, errors.New("unknown playback control")
		}
		answer, err := e.Playback.Control(ctx, action)
		if !e.dialogue().Current(ctx, ticket) {
			return result, context.Canceled
		}
		result.Answer = answer
		return result, err
	case "home":
		if e.Home == nil {
			return Resolution{Route: "yandex"}, nil
		}
		var history []llm.Message
		if decision.Source == "dialogue" {
			history = e.dialogue().TakeHomeDialogue(ctx, ticket)
			if len(history) == 0 {
				if !e.dialogue().CurrentTurn(ctx, ticket) {
					return result, context.Canceled
				}
				result.Answer = "Повтори команду целиком, чтобы я мог её выполнить."
				return result, nil
			}
		} else if decision.Source == "home_state_dialogue" {
			history = snapshot.Messages
		} else if !e.dialogue().Clear(ctx, ticket) {
			return result, context.Canceled
		}
		if !e.dialogue().CurrentTurn(ctx, ticket) {
			return result, context.Canceled
		}
		question := e.stripModelPrefix(text)
		var answer string
		var err error
		if contextual, ok := e.Home.(HomeDialogueHandler); ok {
			answer, err = contextual.HandleDialogue(ctx, question, history)
		} else {
			answer, err = e.Home.Handle(ctx, question)
		}
		if !e.dialogue().CurrentTurn(ctx, ticket) || (err == nil && !e.dialogue().CommitHome(ctx, ticket, question, answer, history)) {
			return result, context.Canceled
		}
		result.Answer = answer
		return result, err
	case "llm":
		return e.Fallback(ctx, text)
	default:
		return Resolution{Route: "yandex"}, nil
	}
}
func (e *Engine) Fallback(ctx context.Context, text string) (Resolution, error) {
	if strings.Contains(strings.ToLower(text), "сайт") || webaddress.MusicRequest(text) {
		text = webaddress.Normalize(text)
	}
	r := Resolution{Route: "llm"}
	if e.LLM == nil && e.Assistant == nil {
		return r, errors.New("LLM is not configured")
	}
	answerer := e.LLM
	if e.Assistant != nil {
		answerer = e.Assistant
	}
	ctx, snapshot, ticket := e.dialogue().Start(ctx)
	if !e.dialogue().Current(ctx, ticket) {
		return r, context.Canceled
	}
	system := e.SystemPrompt
	if strings.TrimSpace(system) == "" {
		system = defaultSystemPrompt
	}
	messages := []llm.Message{{Role: "system", Content: system + e.ttsInstructions()}}
	messages = append(messages, snapshot.Messages...)
	messages = append(messages, llm.Message{Role: "user", Content: text})
	messages = e.modelMessages(messages)
	private := snapshot.PrivateHistory
	var answer string
	var err error
	if contextual, ok := answerer.(interface {
		CompleteDialogue(context.Context, []llm.Message, bool) (string, bool, error)
	}); ok {
		answer, private, err = contextual.CompleteDialogue(ctx, messages, private)
	} else {
		answer, err = answerer.Complete(ctx, messages, false)
	}
	if !e.dialogue().Current(ctx, ticket) {
		return r, context.Canceled
	}
	if err != nil {
		r.Answer = answer
		return r, err
	}
	r.setModelAnswer(answer, e.TTSMarkup)
	if !e.dialogue().Commit(ctx, ticket, text, r.Answer, private) {
		return Resolution{Route: r.Route}, context.Canceled
	}
	return r, nil
}

var homeActionWords = regexp.MustCompile(`(?:^|[\s\p{P}])(?:` +
	`включи(?:ть)?|выключи(?:ть)?|включай|выключай|погаси|погасить|зажги|зажечь|открой|открыть|закрой|закрыть|` +
	`установи(?:ть)?|выставь|выставить|поставь|поставить|сделай|сделать|подними|поднять|опусти(?:ть)?|` +
	`прибавь|убавь|запусти(?:ть)?|останови(?:ть)?|проиграй|проиграть|воспроизведи|воспроизвести|` +
	`сыграй|проверь|проверить|покажи|найди|расскажи|объясни|напомни|закажи)(?:те)?(?:$|[\s\p{P}])`)
var homeReplySelection = regexp.MustCompile(`^(?:(?:да|нет)[\s,]+)?(?:только|кроме|вместе с|в|во|на|у|без)\s+\p{L}`)
var homeRoomReply = regexp.MustCompile(`^(?:в|во|на|у)\s+\p{L}`)
var homeReplyNumber = regexp.MustCompile(`^[+-]?[0-9]+(?:[.,][0-9]+)?(?:\s*(?:градус[а-я]*|процент[а-я]*|%))?$`)

// Only a selection, location, numeric value or an option in the question
// may inherit an earlier command and its target.
func isHomeClarificationReply(text, question string) bool {
	answer := strings.TrimPrefix(dialogue.Normalize(text), "а ")
	if answer == "" || homeActionWords.MatchString(answer) {
		return false
	}
	switch answer {
	case "да", "нет", "оба", "обе", "все", "все сразу", "везде", "первый", "второй", "первая", "вторая":
		return true
	}
	const maxAnswerWords = 6
	if len(strings.Fields(answer)) > maxAnswerWords {
		return false
	}
	if homeReplyNumber.MatchString(answer) || homeReplySelection.MatchString(answer) {
		return true
	}
	// Compare named choices across inflections such as «приставку» and «приставка».
	words := func(text string) []string {
		return strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	}
	stem := func(word string) string {
		if utf8.RuneCountInString(word) > 4 {
			return strings.TrimRight(word, "аоуыеиюя")
		}
		return word
	}
	options := map[string]bool{}
	for _, word := range words(dialogue.Normalize(question)) {
		options[stem(word)] = true
	}
	answerWords := words(answer)
	for _, word := range answerWords {
		if !options[stem(word)] {
			return false
		}
	}
	return len(answerWords) > 0
}

func containsOne(text string, needles []string) bool {
	for _, s := range needles {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}
func (e *Engine) SetRouting(cfg RoutingConfig) error {
	p, err := CompileRouting(cfg)
	if err != nil {
		return err
	}
	e.routing.Store(p)
	return nil
}
func (e *Engine) policy() *RoutingPolicy {
	if p := e.routing.Load(); p != nil {
		return p
	}
	return builtInRouting
}

type RoutingStatus struct {
	Revision          string
	ActiveRules       int
	ClassifierEnabled bool
	DefaultRoute      string
}

func (e *Engine) RoutingStatus() RoutingStatus {
	p := e.policy()
	return RoutingStatus{Revision: p.revision, ActiveRules: len(p.rules), ClassifierEnabled: p.classifierEnabled, DefaultRoute: p.defaultRoute}
}
func (e *Engine) IsRefusal(text string) bool { return e.policy().isRefusal(text) }
func IsRefusal(text string) bool             { return builtInRouting.isRefusal(text) }

// DecideWithWakeWord supplies the same activation tag as a speaker session.
func (e *Engine) DecideWithWakeWord(ctx context.Context, text, word string) RouteDecision {
	if word != "" {
		ctx = context.WithValue(ctx, wakewordTagKey{}, stationWakewordTag(word))
	}
	return e.Decide(ctx, text)
}

func (e *Engine) Decide(ctx context.Context, text string) (decision RouteDecision) {
	ctx, snapshot, ticket := e.dialogue().Start(ctx)
	defer func() {
		// Choose the cloud path before Native can start local progress/TTS.
		if decision.Route == "home" && e.Home == nil {
			decision.Route = "yandex"
			decision.Source = "home_disabled"
		}
		if decision.Route != "home" || decision.Source != "dialogue" {
			e.dialogue().TakeHomeQuestion(ctx, ticket)
		}
		decision.ticket = ticket
		decision.prepared = true
	}()
	p := e.policy()
	if configvoice.RecoveryAction(text) != "" {
		return RouteDecision{Route: "recovery", Source: "bridge_recovery"}
	}
	if _, ok := configvoice.DiagnosticsCommand(text); ok {
		return RouteDecision{Route: "diagnostics", Source: "bridge_diagnostics"}
	}
	if e.Configuration && configvoice.InConversation(text, snapshot.Messages) {
		return RouteDecision{Route: "llm", Source: "bridge_configuration"}
	}
	if e.AirPlay != nil && airPlayCommand(text) != "" {
		return RouteDecision{Route: "airplay", Source: "airplay"}
	}
	if isRetryCommand(text) {
		return RouteDecision{Route: "llm", Source: "dialogue_retry"}
	}
	if dialogue.IsReset(text) {
		return RouteDecision{Route: "llm", Source: "dialogue_reset"}
	}
	if controller, ok := e.AirPlay.(AirPlayMediaController); ok && airPlayMediaCommand(text) != "" && controller.Active(ctx) {
		return RouteDecision{Route: "airplay", Source: "airplay_media"}
	}
	if e.Playback != nil && playback.Command(text) != "" && e.Playback.Active(ctx) {
		return RouteDecision{Route: "playback", Source: "playback"}
	}
	wakewordTag, _ := ctx.Value(wakewordTagKey{}).(string)
	d, matched := p.match(text, wakewordTag)
	if !matched || d.Route == "home" {
		if question := e.dialogue().PeekHomeQuestion(ctx, ticket); question != "" && isHomeClarificationReply(text, question) {
			return RouteDecision{Route: "home", Source: "dialogue"}
		}
	}
	if matched {
		if d.Route == "llm" && e.Music != nil && e.Music.CanStartMusic(text) {
			d.Route = "music"
		}
		return d
	}
	if snapshot.Route == "llm" && len(snapshot.Messages) > 0 && dialogue.IsFollowup(text) {
		return RouteDecision{Route: "llm", Source: "dialogue"}
	}
	if snapshot.Route == "home" && len(snapshot.Messages) > 0 &&
		home.IsStateQuestion(snapshot.Messages[0].Content) && !dialogue.IsHomeQuestion(snapshot.Messages[len(snapshot.Messages)-1].Content) &&
		homeRoomReply.MatchString(strings.TrimPrefix(dialogue.Normalize(text), "а ")) && isHomeClarificationReply(text, "") {
		return RouteDecision{Route: "home", Source: "home_state_dialogue"}
	}
	if !p.classifierEnabled {
		return RouteDecision{Route: p.defaultRoute, Source: "default"}
	}
	failed := RouteDecision{Route: p.classifier.OnError, Source: "classifier_error"}
	if e.LLM == nil {
		return failed
	}
	classCtx, cancel := context.WithTimeout(ctx, time.Duration(p.classifier.TimeoutMS)*time.Millisecond)
	defer cancel()
	messages := []llm.Message{{Role: "system", Content: p.classifier.Prompt}}
	if snapshot.Route != "home" {
		messages = append(messages, snapshot.Messages...)
	}
	messages = append(messages, llm.Message{Role: "user", Content: text})
	answer, err := e.LLM.Complete(classCtx, messages, true)
	if err != nil {
		return failed
	}
	var choice struct {
		Route string `json:"route"`
	}
	if json.Unmarshal([]byte(answer), &choice) != nil || !directRoute(choice.Route) {
		return failed
	}
	return RouteDecision{Route: choice.Route, Source: "classifier"}
}
