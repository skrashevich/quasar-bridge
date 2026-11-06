package bridge

import (
	"context"
	"encoding/json/v2"
	"errors"
	"quasarbridge/internal/configvoice"
	"quasarbridge/internal/dialogue"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/playback"
	"quasarbridge/internal/reply"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/webaddress"
)

type Completer interface {
	Complete(context.Context, []llm.Message, bool) (string, error)
}
type HomeHandler interface {
	Handle(context.Context, string) (string, error)
}
type PlaybackController interface {
	Active(context.Context) bool
	Control(context.Context, string) (string, error)
}
type MusicStarter interface {
	CanStartMusic(string) bool
	StartMusic(context.Context, string) (string, error)
}
type SoundCloudProvider interface {
	CanHandle(context.Context, string) bool
	Handle(context.Context, string) (string, error)
	HandleEvent(context.Context, string, map[string]any) (bool, error)
	CanDeliver(context.Context, sound.Request) bool
}
type MusicPreferenceController interface {
	MusicDefault(context.Context, string) (string, error)
}

type Engine struct {
	SoundCloud       SoundCloudProvider
	MusicPreferences MusicPreferenceController
	Configuration    bool
	Recovery         func(context.Context, string) (string, error)
	AirPlay          AirPlayController
	LLM              Completer
	Assistant        Completer
	Home             HomeHandler
	Playback         PlaybackController
	Music            MusicStarter
	Dialogue         *dialogue.Store
	// StripPrefixes are model addresses removed only from outbound requests.
	// An empty list disables stripping.
	StripPrefixes    []string
	dispatchMu       sync.Mutex
	dispatches       map[string]*soundCloudDispatchGate
	requestSequence  atomic.Uint64
	dialogueOnce     sync.Once
	routing          atomic.Pointer[RoutingPolicy]
	comparisonMu     sync.Mutex
	comparisonInputs map[string]*comparisonTicket
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
	ctx = e.sequenceRequest(ctx)
	ctx = e.prepareComparisonInput(ctx)
	ctx, _, _ = e.dialogue().Start(ctx)
	return ctx
}

func (e *Engine) sequenceRequest(ctx context.Context) context.Context {
	if sound.RequestSequence(ctx) == 0 {
		return sound.WithRequestSequence(ctx, e.requestSequence.Add(1))
	}
	return ctx
}

func (e *Engine) Resolve(ctx context.Context, text string) (Resolution, error) {
	ctx = e.Prepare(ctx)
	return e.ResolveDecision(ctx, text, e.Decide(ctx, text))
}
func (e *Engine) ResolveDecision(ctx context.Context, text string, decision RouteDecision) (Resolution, error) {
	route := decision.Route
	if route == "music_preference" {
		if err := ctx.Err(); err != nil {
			return Resolution{Route: route}, err
		}
		if decision.prepared && !e.dialogue().Current(ctx, decision.ticket) {
			return Resolution{Route: route}, context.Canceled
		}
		if e.MusicPreferences == nil {
			return Resolution{Route: route, Answer: "Настройка музыкального сервиса сейчас недоступна."}, nil
		}
		answer, err := e.MusicPreferences.MusicDefault(e.sequenceRequest(ctx), configvoice.MusicDefault(text))
		return Resolution{Route: route, Answer: answer}, err
	}
	if route == "soundcloud" {
		result := Resolution{Route: route}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if e.SoundCloud == nil {
			return result, errors.New("SoundCloud is unavailable")
		}
		// Music has its own mutation generations. An information turn must not
		// invalidate audio already queued by an independent playback request.
		answer, err := e.SoundCloud.Handle(e.sequenceRequest(ctx), text)
		result.Answer = answer
		return result, err
	}
	if decision.prepared {
		ctx = e.dialogue().Use(ctx, decision.ticket)
	} else {
		ctx = e.Prepare(ctx)
	}
	_, _, ticket := e.dialogue().Start(ctx)
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
	if route != "llm" && !e.dialogue().Clear(ctx, ticket) {
		return Resolution{Route: route}, context.Canceled
	}
	result := Resolution{Route: route}
	switch route {
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
		answer, err := e.Home.Handle(ctx, e.stripModelPrefix(text))
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
	messages := []llm.Message{{Role: "system", Content: "Ты голосовой помощник пользователя. Отвечай по-русски, кратко и по существу, обычно до трёх предложений. Ответ будет прочитан вслух: без Markdown и служебных рассуждений. Выполняй внешние действия только доступными инструментами по явному запросу пользователя. Не заявляй о выполнении действий без успешного результата инструмента; если инструмента нет, прямо скажи об этом. Для Home Assistant есть отдельный обработчик умного дома."}}
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
	if err == nil {
		if !e.dialogue().Commit(ctx, ticket, text, answer, private) {
			return r, context.Canceled
		}
	}
	r.Answer = answer
	return r, err
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
func (e *Engine) Decide(ctx context.Context, text string) (decision RouteDecision) {
	ctx, snapshot, ticket := e.dialogue().Start(ctx)
	defer func() {
		// Choose the cloud path before Native can start local progress/TTS.
		if decision.Route == "home" && e.Home == nil {
			decision.Route = "yandex"
			decision.Source = "home_disabled"
		}
		decision.ticket = ticket
		decision.prepared = true
	}()
	p := e.policy()
	if configvoice.RecoveryAction(text) != "" {
		return RouteDecision{Route: "recovery", Source: "bridge_recovery"}
	}
	if e.MusicPreferences != nil && configvoice.MusicDefault(text) != "" {
		return RouteDecision{Route: "music_preference", Source: "music_preference"}
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
	if provider, ok := e.SoundCloud.(MusicComparisonProvider); ok && provider.ComparisonEnabled(ctx, text) {
		return RouteDecision{Route: "music_compare", Source: "music_compare"}
	}
	if e.SoundCloud != nil && e.SoundCloud.CanHandle(ctx, text) {
		return RouteDecision{Route: "soundcloud", Source: "soundcloud"}
	}
	if e.Playback != nil && playback.Command(text) != "" && e.Playback.Active(ctx) {
		return RouteDecision{Route: "playback", Source: "playback"}
	}
	if d, ok := p.match(text); ok {
		if d.Route == "llm" && e.Music != nil && e.Music.CanStartMusic(text) {
			d.Route = "music"
		}
		return d
	}
	if len(snapshot.Messages) > 0 && dialogue.IsFollowup(text) {
		return RouteDecision{Route: "llm", Source: "dialogue"}
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
	messages = append(messages, snapshot.Messages...)
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

// HandleEvent bypasses dialogue generations: playback lifecycle is not a conversation turn.
func (e *Engine) HandleEvent(ctx context.Context, name string, payload map[string]any) (bool, error) {
	if !strings.HasPrefix(name, nativeMusicCallbackPrefix) {
		return false, nil
	}
	if e.SoundCloud == nil {
		return true, nil
	}
	return e.SoundCloud.HandleEvent(ctx, name, payload)
}

// CanDeliver checks a queued action at final delivery, after any focus-release
// wait. The provider owns music generations; dialogue turns do not invalidate it.
func (e *Engine) CanDeliver(ctx context.Context, request sound.Request) bool {
	if request.Provider != "SoundCloud" {
		return true
	}
	if control, _ := ctx.Value(soundCloudControlContextKey{}).(bool); control && !e.soundCloudControlAvailable(ctx) {
		return false
	}
	return e.SoundCloud != nil && e.SoundCloud.CanDeliver(ctx, request)
}
