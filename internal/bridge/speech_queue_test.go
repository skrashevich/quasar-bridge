package bridge

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestSpeechQueuePreservesOrder(t *testing.T) {
	var got []string
	q := newSpeechQueue(context.Background(), func(_ context.Context, text string) error {
		got = append(got, text)
		return nil
	})
	for _, text := range []string{"one", "two", "three"} {
		if err := q.add(text); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.finish(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestSpeechQueueStopsOnSynthesisError(t *testing.T) {
	want := errors.New("synthesis failed")
	q := newSpeechQueue(context.Background(), func(context.Context, string) error { return want })
	if err := q.add("one"); err != nil {
		t.Fatal(err)
	}
	if err := q.finish(); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}

func TestSpeechQueueCancellationUnblocksProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	q := newSpeechQueue(ctx, func(ctx context.Context, _ string) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err := q.add("one"); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	if err := q.add("two"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if err := q.finish(); !errors.Is(err, context.Canceled) {
		t.Fatalf("finish error = %v", err)
	}
}
