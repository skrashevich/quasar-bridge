package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

func TestLoadPromptsKeepsBuiltInsWhenUnset(t *testing.T) {
	p, err := LoadPrompts(PromptFiles{})
	if err != nil || p != (Prompts{}) {
		t.Fatalf("empty files: %+v %v", p, err)
	}
}

func TestBuiltInPromptsAreEmbedded(t *testing.T) {
	for name, text := range map[string]string{"assistant": defaultAssistantPrompt, "retry": defaultRetryPrompt} {
		if text == "" || text != strings.TrimSpace(text) {
			t.Errorf("%s prompt: %q", name, text)
		}
	}
}

func TestLoadPromptsReadsTrimmedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assistant.txt")
	if err := os.WriteFile(path, []byte("\n  Свой промпт\nвторая строка \n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPrompts(PromptFiles{Assistant: path})
	if err != nil {
		t.Fatal(err)
	}
	if p.Assistant != "Свой промпт\nвторая строка" || p.Retry != "" {
		t.Fatalf("prompts: %+v", p)
	}
}

func TestLoadPromptsRejectsMissingAndEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, []byte(" \n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, files := range []PromptFiles{
		{Assistant: filepath.Join(dir, "missing.txt")},
		{Retry: empty},
	} {
		if _, err := LoadPrompts(files); err == nil {
			t.Fatalf("accepted %+v", files)
		}
	}
}

func TestFallbackUsesAssistantPromptOverride(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prompts Prompts
		want    string
	}{
		{"built-in", Prompts{}, defaultAssistantPrompt},
		{"file", Prompts{Assistant: "свой ассистент"}, "свой ассистент"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var system string
			e := &Engine{Prompts: tc.prompts, LLM: completionFunc(func(_ context.Context, m []llm.Message, _ bool) (string, error) {
				system = m[0].Content
				return "ответ", nil
			})}
			if _, err := e.Fallback(context.Background(), "привет"); err != nil {
				t.Fatal(err)
			}
			if system != tc.want {
				t.Fatalf("system prompt: %q", system)
			}
		})
	}
}
