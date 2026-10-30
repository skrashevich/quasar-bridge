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

type followupHome struct {
	calls     []string
	followups [][3]string
}

func (h *followupHome) Handle(_ context.Context, s string) (string, error) {
	h.calls = append(h.calls, s)
	return "Выключить телевизор или и приставку тоже?", nil
}
func (h *followupHome) HandleFollowup(_ context.Context, text, request, question string) (string, error) {
	h.followups = append(h.followups, [3]string{text, request, question})
	return "Готово: Телевизор.", nil
}

func TestHomeClarifyingQuestionResumes(t *testing.T) {
	h := &followupHome{}
	e := &Engine{Home: h, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return `{"route":"yandex"}`, nil
	})}
	if _, err := e.Resolve(t.Context(), "Выключи телевизор"); err != nil {
		t.Fatal(err)
	}
	second, err := e.Resolve(t.Context(), "только телевизор")
	if err != nil || second.Route != "home" || len(h.followups) != 1 {
		t.Fatalf("follow-up not resumed: %+v %v %+v", second, err, h)
	}
	if f := h.followups[0]; f[1] != "Выключи телевизор" || !strings.Contains(f[2], "приставку") {
		t.Fatalf("lost context: %+v", f)
	}
	third, _ := e.Resolve(t.Context(), "Какая завтра погода")
	if third.Route != "yandex" {
		t.Fatalf("question must be answered once: %+v", third)
	}
}

func TestExplicitRuleAbandonsHomeQuestion(t *testing.T) {
	h := &followupHome{}
	e := &Engine{Home: h}
	if _, err := e.Resolve(t.Context(), "Выключи телевизор"); err != nil {
		t.Fatal(err)
	}
	r, _ := e.Resolve(t.Context(), "Поставь таймер на 5 минут")
	if r.Route == "home" || len(h.followups) != 0 {
		t.Fatalf("timer captured by pending question: %+v", r)
	}
}

func TestIsQuestion(t *testing.T) {
	for s, want := range map[string]bool{"Какой телевизор выключить?": true, "Уточните, пожалуйста, название.": true, "Готово: Телевизор.": false, "Команда отправлена: Телевизор.": false} {
		if isQuestion(s) != want {
			t.Errorf("%q", s)
		}
	}
}
