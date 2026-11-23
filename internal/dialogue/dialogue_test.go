package dialogue

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestHomeRouteSharesContextExpiryAndLimits(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxTurns = 1
	s := New(cfg)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	for _, text := range []string{"Включи свет в кабинете", "да"} {
		_, ticket := s.Begin(t.Context())
		if !s.CommitRoute(t.Context(), ticket, "home", text, "Уточнение", false) {
			t.Fatal("home turn not committed")
		}
	}
	if got := s.View(t.Context()); got.Route != "home" || len(got.Messages) != 2 || got.Messages[0].Content != "да" {
		t.Fatalf("home context ignored limits: %+v", got)
	}
	now = now.Add(301 * time.Second)
	if got := s.View(t.Context()); got.Route != "" || len(got.Messages) != 0 {
		t.Fatal("expired home confirmation remained active", got)
	}
	_, ticket := s.Begin(t.Context())
	s.Commit(t.Context(), ticket, "новый вопрос", "ответ", false)
	if got := s.View(t.Context()); got.Route != "llm" {
		t.Fatal("ordinary model turns lost route", got)
	}
}

func TestHomeQuestionIsConsumedOnceByCurrentTurn(t *testing.T) {
	s := New(DefaultConfig())
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	ctx := t.Context()
	for _, elapsed := range []time.Duration{44 * time.Second, 45 * time.Second} {
		_, question := s.Begin(ctx)
		s.CommitRoute(ctx, question, "home", "Выключи свет", "Уточните комнату.", false)
		_, older := s.Begin(ctx)
		now = now.Add(elapsed)
		_, current := s.Begin(ctx)
		if s.TakeHomeQuestion(ctx, older) {
			t.Fatal("superseded request consumed the question")
		}
		if got := s.TakeHomeQuestion(ctx, current); got != (elapsed < 45*time.Second) {
			t.Fatalf("question available after %s: %t", elapsed, got)
		}
		if s.TakeHomeQuestion(ctx, current) {
			t.Fatal("question was available twice")
		}
	}
}

func TestStoreIsolationExpiryAndReset(t *testing.T) {
	s := New(DefaultConfig())
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	a, b := WithSpeaker(t.Context(), "speaker-a"), WithSpeaker(t.Context(), "speaker-b")
	_, ticket := s.Begin(a)
	s.Commit(a, ticket, "вопрос", "ответ", true)
	s.Commit(b, ticket, "wrong", "cross-device", false)
	got := s.View(a)
	if len(got.Messages) != 2 || !got.PrivateHistory {
		t.Fatal(got)
	}
	got.Messages[0].Content = "changed"
	if s.View(a).Messages[0].Content != "вопрос" {
		t.Fatal("caller changed stored messages")
	}
	if len(s.View(b).Messages) != 0 {
		t.Fatal("another speaker saw private context")
	}
	now = now.Add(301 * time.Second)
	if len(s.View(a).Messages) != 0 {
		t.Fatal("expired memory retained")
	}
	_, old := s.Begin(a)
	s.Reset(a)
	_, newer := s.Begin(a)
	s.Commit(a, old, "old", "stale", false)
	if len(s.View(a).Messages) != 0 {
		t.Fatal("completion resurrected reset memory")
	}
	s.Commit(a, newer, "new", "fresh", false)
	if s.View(a).Messages[1].Content != "fresh" {
		t.Fatal("fresh turn missing")
	}
}
func TestStoreCancellationAndSupersededRequests(t *testing.T) {
	s := New(DefaultConfig())
	ctx, cancel := context.WithCancel(t.Context())
	_, ticket := s.Begin(ctx)
	cancel()
	s.Commit(ctx, ticket, "canceled", "answer", false)
	if len(s.View(t.Context()).Messages) != 0 {
		t.Fatal("canceled request committed")
	}
	_, older := s.Begin(t.Context())
	_, newer := s.Begin(t.Context())
	s.Commit(t.Context(), newer, "new", "answer", false)
	s.Commit(t.Context(), older, "old", "answer", false)
	if got := s.View(t.Context()); len(got.Messages) != 2 || got.Messages[0].Content != "new" {
		t.Fatal("late turn reordered conversation", got)
	}
}
func TestStoreBoundsAndDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxTurns = 2
	cfg.MaxChars = 512
	s := New(cfg)
	for _, q := range []string{"first", "second", "third"} {
		_, ticket := s.Begin(t.Context())
		s.Commit(t.Context(), ticket, q, "answer", false)
	}
	if got := s.View(t.Context()); len(got.Messages) != 4 || got.Messages[0].Content != "second" {
		t.Fatal(got)
	}
	_, ticket := s.Begin(t.Context())
	s.Commit(t.Context(), ticket, strings.Repeat("я", 513), "answer", false)
	if len(s.View(t.Context()).Messages) != 0 {
		t.Fatal("oversized dialogue retained")
	}
	cfg.Enabled = false
	s = New(cfg)
	_, ticket = s.Begin(t.Context())
	s.Commit(t.Context(), ticket, "a", "b", true)
	if len(s.View(t.Context()).Messages) != 0 {
		t.Fatal("disabled context retained memory")
	}
}
func TestStoreCapacityEvictsOldSessions(t *testing.T) {
	s := New(DefaultConfig())
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	for i := range 140 {
		now = now.Add(time.Second)
		ctx := WithSpeaker(t.Context(), strings.Repeat("a", i+1))
		_, ticket := s.Begin(ctx)
		s.Commit(ctx, ticket, "a", "b", false)
	}
	if len(s.entries) != 128 {
		t.Fatal("unbounded speaker metadata", len(s.entries))
	}
}
func TestConfigValidation(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []Config{{TTLSeconds: 0, MaxTurns: 6, MaxChars: 12000}, {TTLSeconds: 300, MaxTurns: 0, MaxChars: 12000}, {TTLSeconds: 300, MaxTurns: 6, MaxChars: 0}} {
		if c.Validate() == nil {
			t.Fatal("invalid limits accepted")
		}
	}
}

func TestRequestReservationPrecedesClassification(t *testing.T) {
	s := New(DefaultConfig())
	ctx := WithSpeaker(t.Context(), "speaker-a")
	first, _, ticket := s.Start(ctx)
	_, _, again := s.Start(first)
	if ticket != again {
		t.Fatal("one request reserved multiple generations")
	}
	_, _, newer := s.Start(ctx)
	if s.Current(first, ticket) || s.Commit(first, ticket, "old", "answer", false) {
		t.Fatal("superseded request remained current")
	}
	if !s.Clear(ctx, newer) {
		t.Fatal("latest request could not clear topic")
	}
}
func TestIdleTTLDoesNotDiscardInFlightAnswer(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TTLSeconds = 30
	s := New(cfg)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	ctx, _, ticket := s.Start(t.Context())
	now = now.Add(45 * time.Second)
	s.View(ctx)
	other := WithSpeaker(t.Context(), "other-device")
	s.Begin(other)
	if !s.Current(ctx, ticket) || !s.Commit(ctx, ticket, "question", "late answer", false) {
		t.Fatal("processing time was mistaken for idle time")
	}
	if len(s.View(ctx).Messages) != 2 {
		t.Fatal("answer lost after TTL while processing")
	}
	now = now.Add(31 * time.Second)
	if len(s.View(ctx).Messages) != 0 {
		t.Fatal("completed dialogue ignored idle expiry")
	}
}

func TestCloudRefusalTicketSurvivesDialogueExpiry(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TTLSeconds = 30
	s := New(cfg)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	ctx, _, ticket := s.Start(t.Context())
	if !s.Clear(ctx, ticket) {
		t.Fatal("cloud route did not clear old topic")
	}
	now = now.Add(45 * time.Second)
	_, view, same := s.Start(ctx)
	if same != ticket || !s.Current(ctx, ticket) || len(view.Messages) != 0 {
		t.Fatal("expired topic discarded live cloud fallback ticket")
	}
}
