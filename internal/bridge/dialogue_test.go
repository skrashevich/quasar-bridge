package bridge

import (
	"context"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

func TestFollowupKeepsPreviousQuestionAndAnswer(t *testing.T) {
	calls := 0
	e := &Engine{LLM: completionFunc(func(ctx context.Context, m []llm.Message, structured bool) (string, error) {
		if structured {
			return `{"route":"yandex"}`, nil
		}
		calls++
		if calls == 2 {
			if len(m) != 4 || m[1].Content != "о чём мы сегодня общались?" || m[2].Role != "assistant" || m[2].Content != "Сегодня говорили о музыке." || m[3].Content != "а вчера?" {
				t.Errorf("followup lost dialogue: %+v", m)
			}
		}
		return "Сегодня говорили о музыке.", nil
	})}
	first, err := e.Resolve(t.Context(), "о чём мы сегодня общались?")
	if err != nil || first.Route != "llm" {
		t.Fatal(first, err)
	}
	next, err := e.Resolve(t.Context(), "а вчера?")
	if err != nil || next.Route != "llm" || calls != 2 {
		t.Fatalf("followup not local: %+v err=%v calls=%d", next, err, calls)
	}
}

func TestExplicitNewRouteEndsLocalDialogue(t *testing.T) {
	e := &Engine{LLM: completionFunc(func(_ context.Context, _ []llm.Message, structured bool) (string, error) {
		if structured {
			return `{"route":"yandex"}`, nil
		}
		return "Первый ответ", nil
	})}
	e.Resolve(t.Context(), "спроси нейросеть: первый вопрос")
	if got, err := e.Resolve(t.Context(), "какая погода сегодня"); err != nil || got.Route != "yandex" {
		t.Fatal(got, err)
	}
	if got := e.Decide(t.Context(), "а вчера?"); got.Route != "yandex" {
		t.Fatal("stale local topic captured cloud followup", got)
	}
}

func TestResetDialogueHasNoModelCall(t *testing.T) {
	e := &Engine{LLM: completionFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
		if structured {
			return `{"route":"yandex"}`, nil
		}
		for _, v := range m {
			if strings.Contains(v.Content, "начни новый разговор") {
				t.Fatal("reset reached model")
			}
		}
		return "Первый ответ", nil
	})}
	e.Resolve(t.Context(), "спроси нейросеть: первый вопрос")
	result, err := e.Resolve(t.Context(), "начни новый разговор")
	if err != nil || result.Route != "llm" || result.Answer != "Начинаем новый разговор." {
		t.Fatalf("reset failed: %+v %v", result, err)
	}
	if e.Decide(t.Context(), "а вчера?").Route != "yandex" {
		t.Fatal("reset retained dialogue")
	}
}

func TestClassifierReceivesRecentDialogue(t *testing.T) {
	classified := false
	e := &Engine{LLM: completionFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
		if structured {
			classified = true
			if len(m) != 4 || m[1].Content != "спроси нейросеть: расскажи о звёздах" || m[2].Content != "Ответ о звёздах" {
				t.Errorf("classifier lost context: %+v", m)
			}
			return `{"route":"llm"}`, nil
		}
		return "Ответ о звёздах", nil
	})}
	e.Resolve(t.Context(), "спроси нейросеть: расскажи о звёздах")
	e.Resolve(t.Context(), "Можно объяснить это иначе")
	if !classified {
		t.Fatal("classifier not exercised")
	}
}

func TestFailedAnswerIsNotRemembered(t *testing.T) {
	e := &Engine{LLM: completionFunc(func(_ context.Context, _ []llm.Message, structured bool) (string, error) {
		if structured {
			return `{"route":"yandex"}`, nil
		}
		return "", context.DeadlineExceeded
	})}
	if _, err := e.Resolve(t.Context(), "спроси нейросеть: первый вопрос"); err == nil {
		t.Fatal("failure hidden")
	}
	if e.Decide(t.Context(), "а вчера?").Route != "yandex" {
		t.Fatal("failed answer created a topic")
	}
}

func TestSlowClassifierCannotUndoNewerDialogueTurn(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-answer", true: "reset"}[reset], func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			e := &Engine{LLM: completionFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
				if structured {
					close(entered)
					<-release
					if reset {
						return `{"route":"llm"}`, nil
					}
					return `{"route":"yandex"}`, nil
				}
				return "Новый ответ", nil
			})}
			done := make(chan struct{})
			go func() {
				defer close(done)
				e.Resolve(t.Context(), "Старый неоднозначный вопрос")
			}()
			<-entered
			if reset {
				e.Resolve(t.Context(), "начни новый разговор")
			} else {
				e.Resolve(t.Context(), "спроси нейросеть: новый вопрос")
			}
			close(release)
			<-done
			got := e.dialogue().View(t.Context())
			if reset {
				if len(got.Messages) != 0 {
					t.Fatal("old classifier resurrected reset context")
				}
			} else {
				if len(got.Messages) != 2 || got.Messages[0].Content != "спроси нейросеть: новый вопрос" {
					t.Fatal("old classifier erased newer context")
				}
			}
		})
	}
}
