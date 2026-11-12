package bridge

import (
	"context"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

func TestSystemPromptAppliesToFollowupAndRetry(t *testing.T) {
	const custom = "Answer in English.\nExplain in detail."
	for _, prompt := range []string{"", " \n\t", custom} {
		t.Run(prompt, func(t *testing.T) {
			calls := 0
			e := &Engine{SystemPrompt: prompt, LLM: completionFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
				if structured {
					return `{"safe":true}`, nil
				}
				calls++
				if len(messages) == 0 || messages[0].Role != "system" {
					t.Fatal("missing initial system prompt")
				}
				system := messages[0].Content
				if prompt == custom {
					if !strings.Contains(system, custom) || strings.Contains(system, "по-русски") {
						t.Errorf("custom system prompt lost on call %d: %q", calls, system)
					}
				} else if !strings.Contains(system, "по-русски") {
					t.Errorf("blank prompt suppressed default instructions: %q", system)
				}
				if calls == 2 && (len(messages) != 4 || messages[1].Content != "первый вопрос" || messages[2].Content != "Answer") {
					t.Errorf("followup lost conversation: %+v", messages)
				}
				if calls == 3 && !strings.Contains(system, "не выполняй внешние действия и не вызывай инструменты") {
					t.Error("retry lost mandatory no-actions instructions")
				}
				return "Answer", nil
			})}
			for _, question := range []string{"первый вопрос", "следующий вопрос"} {
				if _, err := e.Fallback(t.Context(), question); err != nil {
					t.Fatal(err)
				}
			}
			e.RecordDelivered(t.Context(), "следующий вопрос", "Answer", true, false)
			if result, err := e.Resolve(t.Context(), "используй нейросеть"); err != nil || result.Answer != "Answer" || calls != 3 {
				t.Fatalf("retry failed: %+v, err=%v, calls=%d", result, err, calls)
			}
		})
	}
}

// Removing the TTS suffix from either completion path must fail this test:
// custom style instructions still need the synthesizer's markup vocabulary.
func TestCustomSystemPromptPreservesTTSInstructions(t *testing.T) {
	const custom = "Answer in English."
	for _, markup := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[markup], func(t *testing.T) {
			calls := 0
			e := &Engine{SystemPrompt: custom, TTSMarkup: markup, LLM: completionFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
				if structured {
					return `{"safe":true}`, nil
				}
				calls++
				system := messages[0].Content
				if !strings.HasPrefix(system, custom) {
					t.Errorf("custom prompt missing on completion %d: %q", calls, system)
				}
				if got := strings.Contains(system, `<speaker is_whisper="true">`); got != markup {
					t.Errorf("completion %d TTS instructions enabled = %v, want %v", calls, got, markup)
				}
				return `<speaker is_whisper="true">Answer`, nil
			})}
			for _, question := range []string{"первый вопрос", "следующий вопрос"} {
				result, err := e.Fallback(t.Context(), question)
				if err != nil || result.Answer != "Answer" {
					t.Fatalf("fallback = %+v, err = %v", result, err)
				}
				if got := result.speech != ""; got != markup {
					t.Errorf("synthesis markup enabled = %v, want %v", got, markup)
				}
			}
			e.RecordDelivered(t.Context(), "следующий вопрос", "Answer", true, false)
			result, err := e.Resolve(t.Context(), "используй нейросеть")
			if err != nil || result.Answer != "Answer" || calls != 3 {
				t.Fatalf("retry = %+v, calls = %d, err = %v", result, calls, err)
			}
			if got := result.speech != ""; got != markup {
				t.Errorf("retry synthesis markup enabled = %v, want %v", got, markup)
			}
		})
	}
}
