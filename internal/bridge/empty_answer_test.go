package bridge

import (
	"context"
	"quasarbridge/internal/llm"
	"testing"
)

type emptyCompleter struct{}

func (emptyCompleter) Complete(context.Context, []llm.Message, bool) (string, error) {
	return "   ", nil
}

func TestFallbackRejectsEmptyAnswer(t *testing.T) {
	e := &Engine{Assistant: emptyCompleter{}}
	resolution, err := e.Fallback(t.Context(), "который час")
	if err == nil {
		t.Fatalf("expected error, got answer %q", resolution.Answer)
	}
	if resolution.Answer != "" {
		t.Fatalf("empty answer leaked: %q", resolution.Answer)
	}
}
