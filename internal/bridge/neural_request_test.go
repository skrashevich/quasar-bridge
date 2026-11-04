package bridge

import (
	"context"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

func TestStripNeuralRequestPrefix(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"спроси нейросеть почему небо голубое", "почему небо голубое"},
		{"используй нейросеть, Почему небо голубое?", "Почему небо голубое?"},
		{"  СПРОСИ  НЕЙРОСЕТЬ: Как устроен HTTP/2?", "Как устроен HTTP/2?"},
		{"Алиса, используй нейросеть — Напиши код на Go.", "Напиши код на Go."},
		{"алиса спроси\tнейросеть\nПервый вопрос\nВторой вопрос", "Первый вопрос\nВторой вопрос"},
		{"спроси нейросеть", "спроси нейросеть"},
		{"используй нейросеть:  ", "используй нейросеть:  "},
		{"Объясни фразу «спроси нейросеть»", "Объясни фразу «спроси нейросеть»"},
		{"не используй нейросеть для этого", "не используй нейросеть для этого"},
		{"спроси нейросетью почему", "спроси нейросетью почему"},
		{"спроси нейросетьПочему", "спроси нейросетьПочему"},
		{"почему небо голубое", "почему небо голубое"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got := stripNeuralRequestPrefix(tc.input); got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestNeuralPrefixIsRemovedFromModelInputAndHistory(t *testing.T) {
	for _, prefix := range []string{"спроси нейросеть", "используй нейросеть"} {
		t.Run(prefix, func(t *testing.T) {
			calls := 0
			e := &Engine{LLM: completionFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
				if structured {
					t.Fatal("explicit neural request reached classifier")
				}
				calls++
				if calls == 1 && m[len(m)-1].Content != "какая погода на Марсе?" {
					t.Fatalf("unclean current request: %+v", m)
				}
				if calls == 2 && (len(m) != 4 || m[1].Content != "какая погода на Марсе?" || m[3].Content != "а подробнее?") {
					t.Fatalf("unclean history: %+v", m)
				}
				return "Ответ", nil
			})}
			got, err := e.Resolve(t.Context(), prefix+", какая погода на Марсе?")
			if err != nil || got.Route != "llm" {
				t.Fatalf("result=%+v error=%v", got, err)
			}
			got, err = e.Resolve(t.Context(), "а подробнее?")
			if err != nil || got.Route != "llm" || calls != 2 {
				t.Fatalf("followup=%+v error=%v calls=%d", got, err, calls)
			}
		})
	}
}

func TestStandaloneNeuralCommandsStillRetry(t *testing.T) {
	for _, command := range []string{"спроси нейросеть", "используй нейросеть"} {
		t.Run(command, func(t *testing.T) {
			e := &Engine{LLM: completionFunc(func(_ context.Context, _ []llm.Message, _ bool) (string, error) {
				t.Fatal("bare routing command reached model without a completed question")
				return "", nil
			})}
			got, err := e.Resolve(t.Context(), command)
			if err != nil || !strings.Contains(got.Answer, "Нет завершённого вопроса") {
				t.Fatalf("result=%+v error=%v", got, err)
			}
		})
	}
}

func TestRetryCaptureStoresQuestionWithoutNeuralPrefix(t *testing.T) {
	calls := 0
	e := &Engine{LLM: completionFunc(func(_ context.Context, m []llm.Message, _ bool) (string, error) {
		calls++
		if m[len(m)-1].Content != "почему небо голубое" {
			t.Fatalf("retry contains routing command: %+v", m)
		}
		return "Рассеяние света.", nil
	})}
	ctx := e.Prepare(t.Context())
	e.RecordDelivered(ctx, "спроси нейросеть, почему небо голубое", "Первый ответ.", true, false)
	result, err := e.Resolve(t.Context(), "используй нейросеть")
	if err != nil || result.Answer != "Рассеяние света." || calls != 1 {
		t.Fatalf("result=%+v error=%v calls=%d", result, err, calls)
	}
}
