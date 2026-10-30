package dialogue

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestReplayPreservesCloudContextWithoutChangingLocalTopic(t *testing.T) {
	s := New(DefaultConfig())
	ctx := WithSpeaker(t.Context(), "a")
	_, first := s.Begin(ctx)
	s.CaptureReplay(ctx, first, "Марс?", "Красная планета.", true, false, true, false)
	_, second := s.Begin(ctx)
	s.CaptureReplay(ctx, second, "Спутники?", "Два.", true, false, false, false)
	retry, _, ticket := s.Start(ctx)
	got, ok := s.Replay(retry, ticket)
	if !ok || got.Question != "Спутники?" || got.PreviousAnswer != "Два." || len(got.Messages) != 2 || got.Messages[0].Content != "Марс?" || !got.PrivateHistory {
		t.Fatalf("replay=%+v ready=%v", got, ok)
	}
	if len(s.View(ctx).Messages) != 0 {
		t.Fatal("cloud history changed automatic local routing context")
	}
	if !s.CommitRetry(retry, ticket, "Фобос и Деймос.") {
		t.Fatal("retry commit rejected")
	}
	s.CaptureReplay(retry, ticket, "используй нейросеть", "Фобос и Деймос.", true, false, true, true)
	view := s.View(ctx)
	if len(view.Messages) != 4 || view.Messages[2].Content != "Спутники?" || view.Messages[3].Content != "Фобос и Деймос." || !view.PrivateHistory {
		t.Fatalf("local retry topic=%+v", view)
	}
	_, next := s.Begin(ctx)
	got, ok = s.Replay(ctx, next)
	if !ok || got.Question != "Спутники?" || got.PreviousAnswer != "Фобос и Деймос." || len(got.Messages) != 2 {
		t.Fatalf("retry became its own target: %+v", got)
	}
}

func TestReplayStreamCapturesFinalOnlyAndProtectsOrdering(t *testing.T) {
	s := New(DefaultConfig())
	ctx := WithSpeaker(t.Context(), "a")
	_, first := s.Begin(ctx)
	s.CaptureReplay(ctx, first, "Облака?", "Вода", false, false, false, false)
	_, retry := s.Begin(ctx)
	if _, ok := s.Replay(ctx, retry); ok {
		t.Fatal("partial stream became replayable")
	}
	s.CaptureReplay(ctx, first, "Облака?", "Вода в небе", true, false, false, false)
	if _, ok := s.Replay(ctx, retry); ok {
		t.Fatal("late old cloud answer overwrote newer target")
	}
	s.CaptureReplay(ctx, retry, "Новый вопрос?", "Новый", false, false, false, false)
	s.CaptureReplay(ctx, retry, "Новый вопрос?", "Новый ответ.", true, false, false, false)
	_, next := s.Begin(ctx)
	got, ok := s.Replay(ctx, next)
	if !ok || got.PreviousAnswer != "Новый ответ." {
		t.Fatalf("cumulative stream duplicated text: %+v", got)
	}
	s.CaptureReplay(ctx, next, "Последний?", "Часть ", false, false, false, false)
	s.CaptureReplay(ctx, next, "Последний?", "ответа.", true, false, false, false)
	_, last := s.Begin(ctx)
	got, ok = s.Replay(ctx, last)
	if !ok || got.PreviousAnswer != "Часть ответа." {
		t.Fatalf("delta stream lost text: %+v", got)
	}
	if !s.ClearReplay(ctx, last) {
		t.Fatal("reset rejected")
	}
	s.CaptureReplay(ctx, retry, "старый", "поздний", true, false, false, false)
	_, reset := s.Begin(ctx)
	if _, ok := s.Replay(ctx, reset); ok {
		t.Fatal("late response resurrected reset replay")
	}
}

func TestReplayCapsExpiryIsolationAndIncompleteTarget(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxTurns = 2
	cfg.MaxChars = 512
	cfg.TTLSeconds = 30
	s := New(cfg)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	ctx := WithSpeaker(t.Context(), "a")
	for _, q := range []string{"first", "second", "third"} {
		_, ticket := s.Begin(ctx)
		s.CaptureReplay(ctx, ticket, q, "answer", true, false, false, false)
	}
	_, ticket := s.Begin(ctx)
	got, ok := s.Replay(ctx, ticket)
	if !ok || len(got.Messages) != 2 || got.Messages[0].Content != "second" {
		t.Fatalf("turn cap ignored: %+v", got)
	}
	s.CaptureReplay(ctx, ticket, "используй нейросеть", "guard error", true, false, false, true)
	_, ticket = s.Begin(ctx)
	if _, ok := s.Replay(ctx, ticket); !ok {
		t.Fatal("guard error lost previous completed question")
	}
	_, other := s.Begin(WithSpeaker(t.Context(), "b"))
	if _, ok := s.Replay(WithSpeaker(t.Context(), "b"), other); ok {
		t.Fatal("another device inherited replay")
	}
	now = now.Add(31 * time.Second)
	if _, ok := s.Replay(ctx, ticket); ok {
		t.Fatal("ticket kept expired target")
	}
	_, ticket = s.Begin(ctx)
	s.CaptureReplay(ctx, ticket, "oversized", strings.Repeat("я", 513), true, false, false, false)
	_, ticket = s.Begin(ctx)
	if _, ok := s.Replay(ctx, ticket); ok {
		t.Fatal("oversized response retained")
	}
	s.CaptureReplay(ctx, ticket, "normal", "answer", true, false, false, false)
	_, _ = s.Begin(ctx) // A newer incomplete question must not reveal the older target.
	_, ticket = s.Begin(ctx)
	if _, ok := s.Replay(ctx, ticket); ok {
		t.Fatal("incomplete latest request allowed replay of older question")
	}
}

func TestReplayCancellationDoesNotCommit(t *testing.T) {
	s := New(DefaultConfig())
	ctx, cancel := context.WithCancel(t.Context())
	_, ticket := s.Begin(ctx)
	cancel()
	s.CaptureReplay(ctx, ticket, "question", "answer", true, false, false, false)
	_, next := s.Begin(t.Context())
	if _, ok := s.Replay(t.Context(), next); ok {
		t.Fatal("canceled response became replayable")
	}
}

func TestSkipEmptyDoesNotExtendExpiryOrUndoActions(t *testing.T) {
	for _, action := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry", true: "action"}[action], func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.TTLSeconds = 30
			s := New(cfg)
			now := time.Unix(1000, 0)
			s.now = func() time.Time { return now }
			ctx := WithSpeaker(t.Context(), "speaker")
			_, seed := s.Begin(ctx)
			s.CaptureReplay(ctx, seed, "Небо?", "Голубое.", true, false, false, false)
			now = now.Add(20 * time.Second)
			_, empty := s.Begin(ctx)
			if action {
				s.CaptureReplay(ctx, empty, "", "", false, true, false, false)
			}
			s.SkipEmpty(ctx, empty)
			if action {
				_, next := s.Begin(ctx)
				if _, ok := s.Replay(ctx, next); ok {
					t.Fatal("empty input restored an action-invalidated target")
				}
			} else {
				now = now.Add(11 * time.Second)
				_, next := s.Begin(ctx)
				if _, ok := s.Replay(ctx, next); ok {
					t.Fatal("empty listening extended the original question's lifetime")
				}
			}
		})
	}
}
