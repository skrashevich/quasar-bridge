package bridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
)

func followupTestEngine(enabled bool) *Engine {
	cfg := dialogue.DefaultConfig()
	cfg.Enabled = enabled
	return &Engine{Dialogue: dialogue.New(cfg), LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return `{"route":"yandex"}`, nil
	})}
}

func TestHomeQuestionIsIsolatedBySpeaker(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, interruption := range []string{"оба", "Поставь таймер на 5 минут"} {
			t.Run(fmt.Sprintf("history=%v/%s", enabled, interruption), func(t *testing.T) {
				e := followupTestEngine(enabled)
				h := &followupHome{}
				e.Home = h
				a := dialogue.WithSpeaker(t.Context(), "speaker-a")
				b := dialogue.WithSpeaker(t.Context(), "speaker-b")
				if r, err := e.Resolve(a, "Выключи телевизор"); err != nil || r.Route != "home" {
					t.Fatalf("initial request: %+v %v", r, err)
				}
				if r, err := e.Resolve(b, interruption); err != nil || r.Route == "home" {
					t.Fatalf("other speaker used pending question: %+v %v", r, err)
				}
				if r, err := e.Resolve(a, "оба"); err != nil || r.Route != "home" {
					t.Fatalf("original speaker lost pending question: %+v %v", r, err)
				}
				if len(h.calls) != 1 || len(h.followups) != 1 || h.followups[0][0] != "оба" || h.followups[0][1] != "Выключи телевизор" {
					t.Fatalf("calls=%v followups=%v", h.calls, h.followups)
				}
			})
		}
	}
}

type followupAirPlay struct{}

func (followupAirPlay) Control(context.Context, string) (string, error) { return "Готово", nil }
func (followupAirPlay) Active(context.Context) bool                     { return true }
func (followupAirPlay) ControlMedia(context.Context, string) (string, error) {
	return "Готово", nil
}

func TestEarlyRoutesAbandonHomeQuestion(t *testing.T) {
	for _, tc := range []struct{ name, text, route string }{
		{"airplay_start", "включи airplay", "airplay"},
		{"airplay_media", "громче", "airplay"},
		{"recovery", "аварийный откат quasar bridge", "recovery"},
		{"configuration", "измени настройки квазар бридж", "llm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := followupTestEngine(true)
			h := &followupHome{}
			e.Home, e.AirPlay, e.Configuration = h, followupAirPlay{}, true
			if _, err := e.Resolve(t.Context(), "Выключи телевизор"); err != nil {
				t.Fatal(err)
			}
			if d := e.Decide(t.Context(), tc.text); d.Route != tc.route {
				t.Fatalf("interruption decision=%+v", d)
			}
			if r, err := e.Resolve(t.Context(), "оба"); err != nil || r.Route != "yandex" || len(h.followups) != 0 {
				t.Fatalf("abandoned question resumed: %+v %v followups=%v", r, err, h.followups)
			}
		})
	}
}

func TestLateHomeAnswerCannotRestoreSupersededQuestion(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("history=%v", enabled), func(t *testing.T) {
			e := followupTestEngine(enabled)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var calls atomic.Int32
			e.Home = homeFunc(func(context.Context, string) (string, error) {
				if calls.Add(1) == 1 {
					close(entered)
				}
				// Simulate a backend that finishes after cancellation.
				<-release
				return "Выключить телевизор или приставку?", nil
			})
			done := make(chan error, 1)
			go func() { _, err := e.Resolve(t.Context(), "Выключи телевизор"); done <- err }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("home backend did not start")
			}
			if r, err := e.Resolve(t.Context(), "Поставь таймер на 5 минут"); err != nil || r.Route != "yandex" {
				t.Fatalf("new request: %+v %v", r, err)
			}
			unblock()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("late answer error=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("late backend did not return")
			}
			if r, err := e.Resolve(t.Context(), "оба"); err != nil || r.Route != "yandex" || calls.Load() != 1 {
				t.Fatalf("late question restored: %+v %v calls=%d", r, err, calls.Load())
			}
		})
	}
}

func TestConcurrentPrepareKeepsOnlyNewestHomeRequest(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("history=%v", enabled), func(t *testing.T) {
			for iteration := 0; iteration < 32; iteration++ {
				e := followupTestEngine(enabled)
				calls := 0
				e.Home = homeFunc(func(context.Context, string) (string, error) { calls++; return "Готово", nil })
				contexts := make([]context.Context, 2)
				start := make(chan struct{})
				var wg sync.WaitGroup
				for i := range contexts {
					wg.Go(func() { <-start; contexts[i] = e.Prepare(t.Context()) })
				}
				close(start)
				wg.Wait()
				canceled := 0
				for _, ctx := range contexts {
					d := e.Decide(ctx, "Выключи телевизор")
					_, err := e.ResolveDecision(ctx, "Выключи телевизор", d)
					if errors.Is(err, context.Canceled) {
						canceled++
					} else if err != nil {
						t.Fatal(err)
					}
				}
				if calls != 1 || canceled != 1 {
					t.Fatalf("iteration=%d calls=%d canceled=%d", iteration, calls, canceled)
				}
			}
		})
	}
}

func TestNewestPreparedHomeRequestWinsAndPrepareIsIdempotent(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, resolveNewestFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("history=%v/newest_first=%v", enabled, resolveNewestFirst), func(t *testing.T) {
				e := followupTestEngine(enabled)
				var calls []string
				e.Home = homeFunc(func(_ context.Context, text string) (string, error) {
					calls = append(calls, text)
					return "Готово", nil
				})
				old := e.Prepare(t.Context())
				latest := e.Prepare(t.Context())
				// Reusing the old context must not promote it above the newer request.
				old = e.Prepare(old)
				requests := []struct {
					ctx      context.Context
					text     string
					canceled bool
				}{
					{old, "Выключи телевизор", true},
					{latest, "Включи свет в кабинете", false},
				}
				if resolveNewestFirst {
					requests[0], requests[1] = requests[1], requests[0]
				}
				for _, request := range requests {
					d := e.Decide(request.ctx, request.text)
					_, err := e.ResolveDecision(request.ctx, request.text, d)
					if request.canceled && !errors.Is(err, context.Canceled) {
						t.Fatalf("old request error=%v", err)
					}
					if !request.canceled && err != nil {
						t.Fatalf("latest request error=%v", err)
					}
				}
				if len(calls) != 1 || calls[0] != "Включи свет в кабинете" {
					t.Fatalf("executed requests=%v", calls)
				}
			})
		}
	}
}
