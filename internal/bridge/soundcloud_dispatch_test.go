package bridge

import (
	"context"
	"errors"
	"fmt"
	"quasarbridge/internal/dialogue"
	"sync"
	"testing"
	"time"
)

func TestSoundCloudDispatchCancellationAndIdleCleanup(t *testing.T) {
	e := new(Engine)
	ctx := dialogue.WithSpeaker(t.Context(), "speaker")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	releaseGate := sync.OnceFunc(func() { close(release) })
	defer releaseGate()
	go func() { done <- e.DispatchSoundCloud(ctx, func() error { close(entered); <-release; return nil }) }()
	<-entered
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.DispatchSoundCloud(canceled, func() error { t.Error("canceled dispatch ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	waiting, cancelWait := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancelWait()
	if err := e.DispatchSoundCloud(waiting, func() error { t.Error("expired waiter ran"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	// A different speaker is independent of the blocked dispatch.
	if err := e.DispatchSoundCloud(dialogue.WithSpeaker(t.Context(), "other"), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	releaseGate()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for i := range 300 {
		if err := e.DispatchSoundCloud(dialogue.WithSpeaker(t.Context(), fmt.Sprintf("speaker-%d", i)), func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	e.dispatchMu.Lock()
	defer e.dispatchMu.Unlock()
	if len(e.dispatches) != 0 {
		t.Fatalf("idle dispatch state retained: %d", len(e.dispatches))
	}
}
