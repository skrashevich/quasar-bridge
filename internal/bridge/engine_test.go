package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

type completionFunc func(context.Context, []llm.Message, bool) (string, error)

func (f completionFunc) Complete(ctx context.Context, m []llm.Message, b bool) (string, error) {
	return f(ctx, m, b)
}

type homeFunc func(context.Context, string) (string, error)

func (f homeFunc) Handle(ctx context.Context, s string) (string, error) { return f(ctx, s) }

type listenHome struct{ listen *bool }

func (h listenHome) Handle(context.Context, string) (string, error) { return "Включено", nil }
func (h listenHome) Turn(context.Context, string) (string, *bool, error) {
	return "Включено", h.listen, nil
}

func TestHomeTurnSuppliesListenOverride(t *testing.T) {
	off := false
	engine := Engine{Home: listenHome{listen: &off}}
	got, err := engine.Resolve(t.Context(), "Включи свет в кабинете")
	if err != nil || got.Route != "home" || got.Listen == nil || *got.Listen {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestFlashlightPhraseRoutesToHome(t *testing.T) {
	e := &Engine{Home: homeFunc(func(context.Context, string) (string, error) { return "", nil })}
	got := e.Decide(t.Context(), "включи фонарики")
	if got.Route != "home" || got.Rule != "home_devices" {
		t.Fatalf("flashlight route=%+v", got)
	}
}

func TestEngineRoutesHomeComplexAndYandexWithoutDuplicateBackend(t *testing.T) {
	for _, tc := range []struct{ text, route string }{{"Включи свет в кабинете", "home"}, {"Напиши код сортировки на Go", "llm"}, {"Какая завтра погода", "yandex"}} {
		t.Run(tc.route, func(t *testing.T) {
			homeCalls, answers := 0, 0
			engine := Engine{Home: homeFunc(func(ctx context.Context, s string) (string, error) { homeCalls++; return "Включено", nil }), LLM: completionFunc(func(ctx context.Context, m []llm.Message, structured bool) (string, error) {
				if structured {
					return `{"route":"yandex"}`, nil
				}
				answers++
				return "Ответ модели", nil
			})}
			got, err := engine.Resolve(t.Context(), tc.text)
			if err != nil || got.Route != tc.route {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			if (homeCalls > 0) != (tc.route == "home") || (answers > 0) != (tc.route == "llm") {
				t.Fatalf("wrong backends: home=%d llm=%d", homeCalls, answers)
			}
		})
	}
}

func TestHomeFailureIsNotRetriedInYandex(t *testing.T) {
	e := Engine{Home: homeFunc(func(context.Context, string) (string, error) { return "", errors.New("readback failed") }), LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("home failure fell through to LLM")
		return "", nil
	})}
	r, err := e.Resolve(t.Context(), "Выключи свет в кабинете")
	if err == nil || r.Route != "home" {
		t.Fatalf("unsafe fallback=%+v %v", r, err)
	}
}

func TestUnconfiguredHomeRoutesToYandex(t *testing.T) {
	for _, source := range []string{"rule", "default", "classifier", "classifier_error"} {
		t.Run(source, func(t *testing.T) {
			cfg := DefaultRoutingConfig()
			e := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
				if source == "classifier_error" {
					return "", errors.New("offline")
				}
				return `{"route":"home"}`, nil
			})}
			if source != "rule" {
				cfg.Rules = nil
			}
			if source == "default" {
				cfg.DefaultRoute = "home"
			}
			cfg.Classifier.OnError = "home"
			if err := e.SetRouting(cfg); err != nil {
				t.Fatal(err)
			}
			d := e.Decide(t.Context(), "Выключи свет")
			if d.Route != "yandex" || d.Source != "home_disabled" {
				t.Fatalf("unavailable home selected: %+v", d)
			}
			if source == "rule" && d.Rule != "home_devices" {
				t.Fatal("matching rule identity lost", d)
			}
			r, err := e.ResolveDecision(t.Context(), "Выключи свет", d)
			if err != nil || r.Route != "yandex" || r.Answer != "" {
				t.Fatalf("cloud handoff failed: %+v %v", r, err)
			}
		})
	}
	// Callers supplying a decision directly must get the same handoff.
	r, err := (&Engine{}).ResolveDecision(t.Context(), "Выключи свет", RouteDecision{Route: "home"})
	if err != nil || r.Route != "yandex" {
		t.Fatalf("direct decision failed: %+v %v", r, err)
	}
}

func TestRefusalDetectionAvoidsBenignMention(t *testing.T) {
	for _, text := range []string{"Я не могу помочь с этим запросом.", "Извините, я не могу обсуждать эту тему.", "Я не поняла ваш вопрос."} {
		if !IsRefusal(text) {
			t.Fatalf("missed refusal %q", text)
		}
	}
	if IsRefusal("Автор написал: я не могу помочь с этим запросом. Разберём эту цитату.") {
		t.Fatal("quoted refusal classified as own refusal")
	}
	if IsRefusal(strings.Repeat(" ", 3) + "Сегодня хорошая погода.") {
		t.Fatal("benign answer rejected")
	}
}

func TestObservedAliceRefusalVariants(t *testing.T) {
	for _, s := range []string{"Увы, но я не могу понять, где это на планете Кракозябра.", "Извините, я не знаю, где это.", "Я не поняла вопрос."} {
		if !IsRefusal(s) {
			t.Errorf("missed refusal: %q", s)
		}
	}
}

func TestAssistantToolsAreIsolatedFromRoutingAndHomePlanning(t *testing.T) {
	plain, agentCalls := 0, 0
	e := &Engine{LLM: completionFunc(func(ctx context.Context, m []llm.Message, structured bool) (string, error) {
		if !structured {
			t.Fatal("dialogue bypassed assistant")
		}
		plain++
		return `{"route":"llm"}`, nil
	}), Assistant: completionFunc(func(ctx context.Context, m []llm.Message, structured bool) (string, error) {
		if structured {
			t.Fatal("tools used for classification")
		}
		agentCalls++
		return "tool answer", nil
	})}
	cfg := DefaultRoutingConfig()
	cfg.Rules = nil
	cfg.DefaultRoute = "classify"
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := e.Resolve(t.Context(), "прочитай веб-страницу")
	if err != nil || got.Answer != "tool answer" || plain != 1 || agentCalls != 1 {
		t.Fatalf("wrong calls: %+v %v %d %d", got, err, plain, agentCalls)
	}
}

func TestSystemPromptIsAppendedOnlyToDialogue(t *testing.T) {
	note := "Ты находишься в комнате «Зал» в городе «Москва». Если пользователь не указал комнату, считай, что речь идёт о Зале."
	var dialogue, classifier string
	e := &Engine{SystemPrompt: note, LLM: completionFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
		if structured {
			if len(m) == 0 || m[0].Role != "system" {
				t.Fatal("classifier has no system message")
			}
			classifier = m[0].Content
			return `{"route":"llm"}`, nil
		}
		systems := 0
		for _, message := range m {
			if message.Role == "system" {
				systems++
				dialogue = message.Content
			}
		}
		if systems != 1 {
			t.Fatalf("system messages=%d", systems)
		}
		return "Свет в Зале.", nil
	})}
	cfg := DefaultRoutingConfig()
	cfg.Rules = nil
	cfg.DefaultRoute = "classify"
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := e.Resolve(t.Context(), "включи свет")
	if err != nil || got.Route != "llm" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	if !strings.Contains(dialogue, "голосовой помощник") || !strings.Contains(dialogue, "комнате «Зал»") {
		t.Fatalf("dialogue prompt=%q", dialogue)
	}
	if strings.Contains(classifier, "Зал") || strings.Contains(classifier, "Дополнительные указания") {
		t.Fatalf("classifier saw operator note: %q", classifier)
	}
}

type sessionHome struct {
	active bool
	resets int
	calls  int
}

func (s *sessionHome) Handle(context.Context, string) (string, error) {
	s.calls++
	return "Свет в зале включён.", nil
}
func (s *sessionHome) Active(context.Context) bool { return s.active }
func (s *sessionHome) Reset(context.Context)       { s.resets++; s.active = false }

func TestHomeAssistantFollowupAndReset(t *testing.T) {
	home := &sessionHome{}
	e := &Engine{Home: home}
	if got := e.Decide(t.Context(), "а ярче?"); got.Route == "home" {
		t.Fatal("inactive assistant captured follow-up", got)
	}
	home.active = true
	got := e.Decide(t.Context(), "а ярче?")
	if got.Route != "home" || got.Source != "home_assistant" {
		t.Fatalf("follow-up=%+v", got)
	}
	if weather := e.Decide(t.Context(), "какая погода"); weather.Route != "yandex" || weather.Source == "home_assistant" {
		t.Fatal("rule lost to assistant follow-up", weather)
	}
	result, err := e.Resolve(t.Context(), "а ярче?")
	if err != nil || result.Answer != "Свет в зале включён." || home.calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, home.calls)
	}
	reset, err := e.Resolve(t.Context(), "начни новый разговор")
	if err != nil || reset.Answer != "Начинаем новый разговор." || home.resets != 1 || home.calls != 1 || home.active {
		t.Fatalf("reset=%+v err=%v home=%+v", reset, err, home)
	}
	if again := e.Decide(t.Context(), "а ярче?"); again.Route == "home" {
		t.Fatal("reset kept the assistant conversation", again)
	}
}

func TestHistoryQuestionNeverExecutesQuotedHomeCommand(t *testing.T) {
	e := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return "сохранённый ответ", nil
	}), Home: homeFunc(func(context.Context, string) (string, error) {
		t.Fatal("history question executed a physical command")
		return "", nil
	})}
	for _, query := range []string{"что я спрашивал, когда просил выключить свет", "найди в истории запрос включи свет", "прочитай страницу https://example.org/ про температуру"} {
		result, err := e.Resolve(t.Context(), query)
		if err != nil || result.Route != "llm" {
			t.Fatalf("query %q: %+v %v", query, result, err)
		}
	}
}
