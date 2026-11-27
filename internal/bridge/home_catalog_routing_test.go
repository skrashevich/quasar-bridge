package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/llm"
)

type stubHomeCatalog struct {
	known   bool
	matches map[string]bool
	summary string
}

func (s stubHomeCatalog) Handle(context.Context, string) (string, error) { return "Готово.", nil }
func (s stubHomeCatalog) HomeHint(text string) (bool, bool, string) {
	return s.known, s.matches[text], s.summary
}

func TestBareMediaRequestsSkipClassifierWhenNoHomeDeviceMatches(t *testing.T) {
	var classifierCalls int
	var lastSystemPrompt string
	e := &Engine{
		Home: stubHomeCatalog{
			known: true,
			matches: map[string]bool{
				"включи приставку": true,
				"включи аквариум":  true,
			},
			summary: "Комнаты: Гостиная, Кухня. Устройства: Подсветка, Приставка, Аквариум.",
		},
		LLM: completionFunc(func(_ context.Context, msgs []llm.Message, _ bool) (string, error) {
			classifierCalls++
			if len(msgs) > 0 {
				lastSystemPrompt = msgs[0].Content
			}
			return `{"route":"home"}`, nil
		}),
	}

	for _, phrase := range []string{
		"включи Батарейку",
		"включи Солнце Монако",
		"поставь Группа крови",
		"Алиса, включи Белая ночь",
	} {
		d := e.Decide(t.Context(), phrase)
		if d.Route != "yandex" || d.Source != "non_home_media" {
			t.Errorf("%q: got %+v, want yandex/non_home_media", phrase, d)
		}
	}
	if classifierCalls != 0 {
		t.Fatalf("expected 0 classifier calls for bare media titles, got %d", classifierCalls)
	}

	// Real device requests still go through the classifier with the live HA catalog in context.
	d := e.Decide(t.Context(), "включи приставку")
	if d.Route != "home" || d.Source != "classifier" {
		t.Fatalf("real device got %+v, want home/classifier", d)
	}
	if classifierCalls != 1 || !strings.Contains(lastSystemPrompt, "Устройства: Подсветка, Приставка, Аквариум.") {
		t.Fatalf("expected catalog summary in classifier system prompt, calls=%d prompt=%q", classifierCalls, lastSystemPrompt)
	}

	// Questions and bridge/assistant settings phrases must not bypass the classifier.
	d = e.Decide(t.Context(), "включи режим отладки")
	if d.Source == "non_home_media" {
		t.Fatalf("settings phrase must not match non_home_media: %+v", d)
	}
}

func TestUnpopulatedHomeCatalogFallsBackToClassifier(t *testing.T) {
	var classifierCalls int
	e := &Engine{
		Home: stubHomeCatalog{known: false},
		LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
			classifierCalls++
			return `{"route":"yandex"}`, nil
		}),
	}
	d := e.Decide(t.Context(), "включи Батарейку")
	if d.Route != "yandex" || d.Source != "classifier" || classifierCalls != 1 {
		t.Fatalf("unpopulated catalog should fall back to classifier, got %+v calls=%d", d, classifierCalls)
	}
}
