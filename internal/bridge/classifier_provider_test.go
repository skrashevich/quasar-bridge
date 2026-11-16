package bridge

import (
	"context"
	"errors"
	"testing"

	"quasarbridge/internal/llm"
)

func TestIndependentClassifierProvider(t *testing.T) {
	e := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("classifier used answer provider")
		return "", nil
	})}
	cfg := DefaultRoutingConfig()
	cfg.Rules = nil
	cfg.Classifier.OnError = "llm"
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	e.Classifier = completionFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
		if !structured || m[len(m)-1].Content != "Полный вопрос" {
			t.Error("lost classifier input")
		}
		return `{"route":"yandex"}`, nil
	})
	if d := e.Decide(t.Context(), "Полный вопрос"); d.Route != "yandex" || d.Source != "classifier" {
		t.Fatalf("decision=%+v", d)
	}
	e.Classifier = completionFunc(func(context.Context, []llm.Message, bool) (string, error) { return "", errors.New("unavailable") })
	if d := e.Decide(t.Context(), "Другой вопрос"); d.Route != "llm" || d.Source != "classifier_error" {
		t.Fatalf("decision=%+v", d)
	}
}
