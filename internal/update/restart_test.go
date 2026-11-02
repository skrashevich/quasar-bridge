package update

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"quasarbridge/internal/reply"
)

func TestRestartWaitsForAnswerContext(t *testing.T) {
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "app"), 0700); err != nil {
		t.Fatal(err)
	}
	// Run the real detached helper with a harmless service controller.
	if err := os.WriteFile(filepath.Join(state, "app", "control.sh"), []byte("#!/bin/sh\ntest \"$1\" = restart || exit 1\nprintf restarted > \"$QUASAR_DATA_DIR/result\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(state, "lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	delivery := reply.WithPolicy(t.Context())
	ctx, cancel := context.WithCancel(delivery)
	defer cancel()
	done, err := launchRestart(ctx, state, lock)
	if err != nil {
		t.Fatal(err)
	}
	// The model has returned, but TTS has not been forwarded to the speaker.
	cancel()
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(state, "result")); !os.IsNotExist(err) {
		t.Fatal("restarted before answer completed")
	}
	reply.Delivered(delivery)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, _ := os.ReadFile(filepath.Join(state, "result"))
		if string(body) == "restarted" {
			if _, err := os.Stat(lock); os.IsNotExist(err) {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("helper completion not reported")
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("restart helper did not run or release its operation lock")
}

func TestRestartRejectsUnmanagedInstallation(t *testing.T) {
	u := New(filepath.Join(t.TempDir(), "config.json"))
	if _, err := u.Restart(t.Context()); err == nil {
		t.Fatal("unmanaged restart accepted")
	}
}
