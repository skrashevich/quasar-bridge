package dialogue

import (
	"context"
	"testing"
	"time"
)

func TestPendingMusicSourceIsScopedExpiresAndCannotBeResurrected(t *testing.T) {
	store := New(DefaultConfig())
	now := time.Unix(1000, 0)
	store.now = func() time.Time { return now }
	a, b := WithSpeaker(t.Context(), "a"), WithSpeaker(t.Context(), "b")
	_, old := store.Begin(a)
	if !store.SetPendingMusicSource(a, old, true) || !store.View(a).PendingMusicSource || store.View(b).PendingMusicSource {
		t.Fatal("pending address is not scoped")
	}
	_, newer := store.Begin(a)
	if store.SetPendingMusicSource(a, old, false) || !store.View(a).PendingMusicSource {
		t.Fatal("stale request consumed pending address")
	}
	canceled, cancel := context.WithCancel(a)
	cancel()
	if store.SetPendingMusicSource(canceled, newer, false) || !store.View(a).PendingMusicSource {
		t.Fatal("canceled request consumed pending address")
	}
	now = now.Add(301 * time.Second)
	if store.View(a).PendingMusicSource {
		t.Fatal("pending address survived TTL")
	}
	if store.SetPendingMusicSource(a, newer, true) {
		t.Fatal("expired request resurrected pending address")
	}
	store.Reset(a)
	if store.SetPendingMusicSource(a, newer, true) || store.View(a).PendingMusicSource {
		t.Fatal("reset resurrected pending address")
	}
}

func TestPendingMusicSourceClearsOnNormalCommitAndClear(t *testing.T) {
	store := New(DefaultConfig())
	ctx := WithSpeaker(t.Context(), "a")
	_, ticket := store.Begin(ctx)
	store.SetPendingMusicSource(ctx, ticket, true)
	store.Commit(ctx, ticket, "question", "answer", false)
	if store.View(ctx).PendingMusicSource {
		t.Fatal("normal turn retained pending address")
	}
	store.SetPendingMusicSource(ctx, ticket, true)
	store.Clear(ctx, ticket)
	if store.View(ctx).PendingMusicSource {
		t.Fatal("clear retained pending address")
	}
}
