package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/reply"
)

// Recovery must run before configuration routing, user rules and classification,
// even with configuration tools disabled and both model backends unavailable.
func TestRecoveryBypassesModelsAndRouting(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, tc := range []struct{ text, action string }{
			{"переведи квазар бридж в observe", "observe"},
			{"аварийный откат quasar bridge", "rollback"},
		} {
			t.Run(tc.action+map[bool]string{false: "/tools_disabled", true: "/tools_enabled"}[enabled], func(t *testing.T) {
				calls := 0
				e := &Engine{Configuration: enabled, Recovery: func(ctx context.Context, action string) (string, error) {
					calls++
					if action != tc.action {
						t.Fatalf("action=%q, want %q", action, tc.action)
					}
					return "Восстановление запланировано.", nil
				}}
				e.LLM = completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
					t.Error("emergency request contacted LLM")
					return "", errors.New("offline")
				})
				if enabled {
					e.Assistant = e.LLM
				}
				cfg := DefaultRoutingConfig()
				cfg.Rules = []RoutingRule{{Name: "catch_all", Route: "home", Match: RoutingMatch{Regex: ".*"}}}
				if err := e.SetRouting(cfg); err != nil {
					t.Fatal(err)
				}
				ctx := reply.WithPolicy(t.Context())
				d := e.Decide(ctx, tc.text)
				if d.Route != "recovery" || d.Source != "bridge_recovery" || calls != 0 {
					t.Fatalf("recovery not selected without side effects: %+v, calls=%d", d, calls)
				}
				r, err := e.ResolveDecision(ctx, tc.text, d)
				if err != nil || r.Route != "recovery" || r.Answer != "Восстановление запланировано." || calls != 1 {
					t.Fatalf("result=%+v err=%v calls=%d", r, err, calls)
				}
				if reply.ListeningAllowed(ctx) {
					t.Fatal("microphone reopened before scheduled recovery")
				}
			})
		}
	}
}

func TestCanceledRecoveryNeverSchedulesAnOperation(t *testing.T) {
	e := &Engine{Recovery: func(context.Context, string) (string, error) {
		t.Fatal("canceled request scheduled recovery")
		return "", nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r, err := e.Resolve(ctx, "переведи quasar bridge в observe")
	if !errors.Is(err, context.Canceled) || r.Route != "recovery" {
		t.Fatalf("canceled recovery: %+v %v", r, err)
	}
}

func TestRecoveryFailureDoesNotFallBackToModel(t *testing.T) {
	failure := errors.New("unmanaged installation")
	e := &Engine{Recovery: func(context.Context, string) (string, error) { return "", failure }}
	ctx := reply.WithPolicy(t.Context())
	r, err := e.Resolve(ctx, "переведи quasar bridge в observe")
	if !errors.Is(err, failure) || r.Route != "recovery" || r.Answer != "" || !reply.ListeningAllowed(ctx) {
		t.Fatalf("wrong failure: %+v %v", r, err)
	}
	if r, err := (&Engine{}).Resolve(t.Context(), "аварийный откат квазар бридж"); err == nil || r.Route != "recovery" {
		t.Fatalf("unavailable recovery reached another backend: %+v %v", r, err)
	}
}

func TestNativeRecoveryFailureExplainsManualRecovery(t *testing.T) {
	n, _, down := soundSession(t, resolverFunc(func(context.Context, string) (Resolution, error) {
		return Resolution{Route: "recovery"}, errors.New("operation locked")
	}))
	defer n.Close()
	if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("recovery-failure")); err != nil {
		t.Fatal(err)
	}
	r := airPlayListeningResponse(t, down)
	text := r.GetVoiceResponse().GetOutputSpeech().GetText()
	if !strings.Contains(text, "аварийное восстановление") || !strings.Contains(text, "SSH") || strings.Contains(text, "локальной модели") {
		t.Fatalf("misleading recovery failure: %q", text)
	}
}

func TestNativeRecoveryKeepsMicrophoneClosedAfterScheduling(t *testing.T) {
	e := &Engine{Recovery: func(context.Context, string) (string, error) {
		return "Аварийный откат запланирован.", nil
	}}
	n, _, down := soundSession(t, e)
	defer n.Close()
	if err := n.Client(t.Context(), websocket.MessageBinary, airPlayListeningInput("recovery-scheduled", "аварийный откат квазар бридж")); err != nil {
		t.Fatal(err)
	}
	r := airPlayListeningResponse(t, down)
	if r.GetVoiceResponse().GetShouldListen() || r.GetVoiceResponse().GetOutputSpeech().GetText() != "Аварийный откат запланирован." {
		t.Fatalf("wrong scheduled recovery response: %v", r)
	}
}

// A stuck request for the same speaker must not hold up emergency routing.
func TestRecoveryDoesNotWaitForInFlightClassification(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	defer close(release)
	e := &Engine{LLM: completionFunc(func(ctx context.Context, _ []llm.Message, _ bool) (string, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return "", errors.New("offline")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}), Recovery: func(context.Context, string) (string, error) { return "Запланировано.", nil }}
	cfg := DefaultRoutingConfig()
	cfg.Rules = nil
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := dialogue.WithSpeaker(t.Context(), "recovery-speaker")
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = e.Resolve(ctx, "неизвестный вопрос")
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initial request did not reach classifier")
	}
	recoveryCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	r, err := e.Resolve(recoveryCtx, "переведи quasar bridge в observe")
	if err != nil || r.Route != "recovery" || r.Answer != "Запланировано." {
		t.Fatalf("recovery waited for classifier: %+v %v", r, err)
	}
	select {
	case <-finished:
		t.Fatal("classifier finished before it was released")
	default:
	}
}
