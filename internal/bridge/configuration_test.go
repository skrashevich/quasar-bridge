package bridge

import (
	"context"
	"testing"

	"quasarbridge/internal/llm"
)

func TestConfigurationRequestsReachAssistant(t *testing.T) {
	for _, text := range []string{"измени настройки квазар бридж", "какая модель в quasar bridge", "перезапусти квазар бридж", "включи журнал диалогов quasar-bridge", "Расскажи о конфигурации qwazar bridge", "Какие у тебя есть утилиты для управления qwazar bridge", "настройки квазар блич", "Включи сетевой доступ к веб-панели quasar bridge", "Отключи сетевой доступ к диагностике квазар бридж"} {
		e := &Engine{Configuration: true}
		if got := e.Decide(t.Context(), text); got.Route != "llm" {
			t.Errorf("%q: got %+v", text, got)
		}
	}
}

func TestVoiceRoutingAdministrationReachesAssistant(t *testing.T) {
	for _, text := range []string{"Отключи классификатор", "Включи классификатор", "Установи маршрут по умолчанию в нейросеть", "Покажи правила маршрутизации", "Запросы о погоде отправляй нейросети", "Отправляй вопросы про рецепты Яндексу", "Хочу чтобы вопросы о прогнозе отправлялись нейросети", "Отключи правило маршрутизации погоды", "Проверь маршрут для фразы привет"} {
		e := &Engine{Configuration: true}
		if got := e.Decide(t.Context(), text); got.Route != "llm" || got.Source != "bridge_configuration" {
			t.Errorf("routing administration went elsewhere: %q %+v", text, got)
		}
	}
}

func TestRoutingTopicDoesNotCaptureOtherRules(t *testing.T) {
	e := &Engine{Configuration: true, Assistant: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return "Правила прочитаны.", nil
	}), LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) { return `{"route":"yandex"}`, nil })}
	if _, err := e.Fallback(t.Context(), "Покажи правила маршрутизации"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Какие правила сейчас включены", "Отключи это правило"} {
		if got := e.Decide(t.Context(), text); got.Source != "bridge_configuration" {
			t.Fatalf("routing topic lost: %q %+v", text, got)
		}
	}
	for _, text := range []string{"Какие правила шахмат?", "Отправь запрос в Home Assistant включить свет"} {
		if got := e.Decide(t.Context(), text); got.Source == "bridge_configuration" {
			t.Fatalf("unrelated request treated as routing administration: %q %+v", text, got)
		}
	}
}

func TestBridgeRuleQuestionAnchorsRoutingFollowup(t *testing.T) {
	for _, anchor := range []string{"Покажи правила quasar bridge", "Какие правила у квазар бридж"} {
		e := &Engine{Configuration: true, Assistant: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
			return "Правила прочитаны.", nil
		})}
		if _, err := e.Fallback(t.Context(), anchor); err != nil {
			t.Fatal(err)
		}
		if got := e.Decide(t.Context(), "Отключи правило weather"); got.Source != "bridge_configuration" {
			t.Errorf("Bridge rule question lost routing topic: %q %+v", anchor, got)
		}
	}
}

func TestConfigurationFollowupsKeepLocalTopic(t *testing.T) {
	e := &Engine{Configuration: true, Assistant: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return "Настройки доступны.", nil
	}), LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) { return `{"route":"yandex"}`, nil })}
	if _, err := e.Fallback(t.Context(), "Расскажи о конфигурации quasar bridge"); err != nil {
		t.Fatal(err)
	}
	for _, question := range []string{"А какие у тебя есть утилиты", "Расскажи какие настройки есть", "А какие функции сейчас включены"} {
		decision := e.Decide(t.Context(), question)
		if decision.Route != "llm" || decision.Source != "bridge_configuration" {
			t.Fatalf("configuration followup lost: %q %+v", question, decision)
		}
		if _, err := e.ResolveDecision(t.Context(), question, decision); err != nil {
			t.Fatal(err)
		}
	}
	for _, question := range []string{"Покажи настройки телевизора", "Какая модель телевизора лучше?", "А выключи свет"} {
		if got := e.Decide(t.Context(), question); got.Source == "bridge_configuration" {
			t.Fatalf("unrelated subject inherited config topic: %q %+v", question, got)
		}
	}
	if got := e.Decide(t.Context(), "какая завтра погода"); got.Route != "yandex" {
		t.Fatalf("unrelated weather captured by config topic: %+v", got)
	}
}
