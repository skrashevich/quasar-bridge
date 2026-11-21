package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
)

func TestHomeQuestionRoutesOneFreeFormReply(t *testing.T) {
	const question = "Выключить весь свет во всём доме или только в спальне?"
	var replies []string
	e := &Engine{
		Home: homeFunc(func(_ context.Context, text string) (string, error) {
			replies = append(replies, text)
			if len(replies) == 1 {
				return question, nil
			}
			return "Готово.", nil
		}),
		LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
			return `{"route":"yandex"}`, nil
		}),
	}
	ctx := dialogue.WithSpeaker(t.Context(), "street")
	if got, err := e.Resolve(ctx, "Выключи свет"); err != nil || got.Answer != question {
		t.Fatal(got, err)
	}
	const reply = "Во всей квартире"
	other := dialogue.WithSpeaker(t.Context(), "another-speaker")
	if got := e.Decide(other, reply); got.Route != "yandex" {
		t.Fatal("another speaker inherited the question", got)
	}
	// Voice requests reserve their ticket before the recognizer supplies text.
	prepared := e.Prepare(ctx)
	decision := e.Decide(prepared, reply)
	if decision.Route != "home" || decision.Source != "dialogue" {
		t.Fatal("free-form reply went to the classifier", decision)
	}
	if _, err := e.ResolveDecision(prepared, reply, decision); err != nil || len(replies) != 2 || replies[1] != reply {
		t.Fatal(replies, err)
	}
	if got := e.Decide(ctx, reply); got.Route != "yandex" {
		t.Fatal("completed home action captured a new request", got)
	}
}

func TestExplicitRouteOverridesHomeFollowup(t *testing.T) {
	for _, text := range []string{"Какая погода сегодня", "спроси нейросеть: новый вопрос", "начни новый разговор"} {
		t.Run(text, func(t *testing.T) {
			e := &Engine{Home: homeFunc(func(context.Context, string) (string, error) {
				return "Включить только доступные?", nil
			}), LLM: completionFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
				if structured {
					return `{"route":"yandex"}`, nil
				}
				if len(messages) != 2 {
					t.Errorf("new LLM topic inherited home conversation: %+v", messages)
				}
				return "Новый ответ", nil
			})}
			if _, err := e.Resolve(t.Context(), "Включи свет в кабинете"); err != nil {
				t.Fatal(err)
			}
			if result, err := e.Resolve(t.Context(), text); err != nil || result.Route == "home" {
				t.Fatalf("explicit route retained home: %+v %v", result, err)
			}
			if got := e.Decide(t.Context(), "да"); got.Route == "home" {
				t.Fatal("obsolete home confirmation remained active", got)
			}
		})
	}
}

func TestSupersededHomeAnswerDoesNotReplaceNewTopic(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	e := &Engine{Home: homeFunc(func(context.Context, string) (string, error) {
		close(entered)
		<-release
		return "Включить только доступные?", nil
	}), LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) { return "Новый ответ", nil })}
	ctx := dialogue.WithSpeaker(t.Context(), "same-speaker")
	done := make(chan error, 1)
	go func() { _, err := e.Resolve(ctx, "Включи свет в кабинете"); done <- err }()
	<-entered
	if _, err := e.Resolve(ctx, "спроси нейросеть: новый вопрос"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("superseded home reply was accepted: %v", err)
	}
	if snapshot := e.dialogue().View(ctx); snapshot.Route != "llm" || len(snapshot.Messages) != 2 || snapshot.Messages[1].Content != "Новый ответ" {
		t.Fatalf("old home reply replaced newer context: %+v", snapshot)
	}
}

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
