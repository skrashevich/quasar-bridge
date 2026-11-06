package bridge

import (
	"context"
	"errors"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/sound"
	"testing"
)

type soundCloudStub struct {
	can    bool
	handle func(context.Context, string) (string, error)
	event  func(context.Context, string, map[string]any) (bool, error)
}

func (s soundCloudStub) CanDeliver(context.Context, sound.Request) bool { return true }

func (s soundCloudStub) CanHandle(context.Context, string) bool { return s.can }
func (s soundCloudStub) Handle(ctx context.Context, text string) (string, error) {
	return s.handle(ctx, text)
}
func (s soundCloudStub) HandleEvent(ctx context.Context, name string, payload map[string]any) (bool, error) {
	if s.event == nil {
		return true, nil
	}
	return s.event(ctx, name, payload)
}
func TestEngineSoundCloudRouting(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			calls := 0
			e := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) { calls++; return `{"route":"yandex"}`, nil })}
			if enabled {
				e.SoundCloud = soundCloudStub{can: true, handle: func(context.Context, string) (string, error) { return "SoundCloud ответ", nil }}
			}
			d := e.Decide(t.Context(), "включи SoundCloud")
			want := "yandex"
			if enabled {
				want = "soundcloud"
			}
			if d.Route != want {
				t.Fatalf("route=%s want=%s", d.Route, want)
			}
			if enabled {
				r, err := e.ResolveDecision(t.Context(), "включи SoundCloud", d)
				if err != nil || r.Answer != "SoundCloud ответ" || r.Route != "soundcloud" || calls != 0 {
					t.Fatalf("resolution=%+v err=%v classifier=%d", r, err, calls)
				}
			}
		})
	}
}
func TestEngineSoundCloudErrorsStayLocal(t *testing.T) {
	e := &Engine{SoundCloud: soundCloudStub{can: true, handle: func(context.Context, string) (string, error) { return "", errors.New("unavailable") }}}
	r, err := e.Resolve(t.Context(), "включи SoundCloud")
	if err == nil || r.Route != "soundcloud" {
		t.Fatalf("resolution=%+v err=%v", r, err)
	}
}

func TestEngineSoundCloudPrecedenceAndEventIsolation(t *testing.T) {
	e := &Engine{SoundCloud: soundCloudStub{can: true}, AirPlay: &airPlayMediaFake{active: true}, Playback: &playbackFixture{active: true}}
	for _, tc := range []struct{ text, route string }{{"включи airplay", "airplay"}, {"следующий трек", "airplay"}, {"включи SoundCloud", "soundcloud"}} {
		if d := e.Decide(t.Context(), tc.text); d.Route != tc.route {
			t.Errorf("%q route=%s want=%s", tc.text, d.Route, tc.route)
		}
	}
	e.AirPlay = nil
	if d := e.Decide(t.Context(), "пауза"); d.Route != "soundcloud" {
		t.Fatalf("generic playback beat SoundCloud: %+v", d)
	}
	ctx := e.Prepare(t.Context())
	_, _, ticket := e.dialogue().Start(ctx)
	if handled, err := e.HandleEvent(t.Context(), "quasar_soundcloud_started", soundCloudPayload()); !handled || err != nil {
		t.Fatal("event not handled")
	}
	if !e.dialogue().Current(ctx, ticket) {
		t.Fatal("callback advanced dialogue generation")
	}
	if handled, err := e.HandleEvent(t.Context(), "unrelated", nil); handled || err != nil {
		t.Fatal("unrelated event claimed")
	}
}

func TestEngineSoundCloudRequestSequence(t *testing.T) {
	var sequences []uint64
	e := &Engine{SoundCloud: soundCloudStub{can: true, handle: func(ctx context.Context, _ string) (string, error) {
		sequences = append(sequences, sound.RequestSequence(ctx))
		return "", nil
	}}}
	prepared := e.Prepare(t.Context())
	if sound.RequestSequence(prepared) == 0 || sound.RequestSequence(e.Prepare(prepared)) != sound.RequestSequence(prepared) {
		t.Fatal("arrival sequence absent or reassigned")
	}
	if _, err := e.Resolve(prepared, "включи SoundCloud"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resolve(t.Context(), "включи SoundCloud"); err != nil {
		t.Fatal(err)
	}
	if sequences[0] != sound.RequestSequence(prepared) || sequences[1] <= sequences[0] {
		t.Fatalf("sequences=%v", sequences)
	}
}
