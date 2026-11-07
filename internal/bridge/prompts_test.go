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
