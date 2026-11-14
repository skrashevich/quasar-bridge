package bridge

import (
	"context"
	"errors"
	"testing"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
)

type playbackFixture struct {
	active bool
	action string
	after  func()
}

type musicStarterFixture struct {
	called bool
	can    bool
	err    error
}

func (m *musicStarterFixture) CanStartMusic(string) bool { return m.can }
func (m *musicStarterFixture) StartMusic(context.Context, string) (string, error) {
	m.called = true
	return "Включаю музыку.", m.err
}

func TestExplicitWebsiteMusicDoesNotDependOnModel(t *testing.T) {
	starter := &musicStarterFixture{can: true}
	e := &Engine{Music: starter, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("literalmusicrequestwaitedformodel")
		return "", nil
	})}
	d := e.Decide(t.Context(), "Включи музыку с сайта abc.su/music")
	if d.Route != "music" || d.Rule != "music_from_site" {
		t.Fatalf("decision=%+v", d)
	}
	result, err := e.ResolveDecision(t.Context(), "Включи музыку с сайта abc.su/music", d)
	if err != nil || !starter.called || result.Route != "music" {
		t.Fatalf("result=%+v called=%v err=%v", result, starter.called, err)
	}
	policy := DefaultRoutingConfig()
	policy.Rules = []RoutingRule{{Name: "custom-cloud", Route: "yandex", Match: RoutingMatch{ContainsAny: []string{"музык"}}}}
	if err := e.SetRouting(policy); err != nil {
		t.Fatal(err)
	}
	if d := e.Decide(t.Context(), "Включи музыку с сайта abc.su/music"); d.Route != "yandex" {
		t.Fatalf("explicitpolicychanged: %+v", d)
	}
}

func (p *playbackFixture) Active(context.Context) bool { return p.active }
func (p *playbackFixture) Control(_ context.Context, action string) (string, error) {
	p.action = action
	if p.after != nil {
		p.after()
	}
	return "Готово", nil
}

func TestPlaybackResultDiscardedWhenNewRequestArrives(t *testing.T) {
	ctx := dialogue.WithSpeaker(t.Context(), "cabinet-test")
	p := &playbackFixture{active: true}
	e := &Engine{Playback: p}
	p.after = func() { e.Prepare(ctx) }
	d := e.Decide(ctx, "Пауза")
	if _, err := e.ResolveDecision(ctx, "Пауза", d); !errors.Is(err, context.Canceled) {
		t.Fatalf("stalecontrolaccepted: %v", err)
	}
}

func TestCurrentQueueControlSkipsModelAndHome(t *testing.T) {
	p := &playbackFixture{active: true}
	e := &Engine{Playback: p, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("player control reached model")
		return "", nil
	}), Home: homeFunc(func(context.Context, string) (string, error) { t.Fatal("player control reached home"); return "", nil })}
	for _, tc := range []struct{ text, action string }{{"Следующий трек", "next"}, {"Предыдущий трек", "previous"}, {"Пауза", "pause"}, {"Продолжи", "resume"}, {"Хватит", "stop"}, {"Алиса, хватит", "stop"}, {"Начни песню сначала", "restart"}} {
		d := e.Decide(t.Context(), tc.text)
		if d.Route != "playback" || d.Source != "playback" {
			t.Fatalf("%q: %+v", tc.text, d)
		}
		result, err := e.ResolveDecision(t.Context(), tc.text, d)
		if err != nil || result.Route != "playback" || p.action != tc.action {
			t.Fatalf("result=%+v action=%q err=%v", result, p.action, err)
		}
	}
}

func TestTimerResumeStaysOnYandexWhileQueueIsActive(t *testing.T) {
	p := &playbackFixture{active: true}
	e := &Engine{Playback: p}
	cfg := DefaultRoutingConfig()
	cfg.UnknownToHome = true
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Продолжи таймер", "Удали таймер", "Таймер на паузу"} {
		d := e.Decide(t.Context(), text)
		if d.Route != "yandex" || d.Source != "rule" || p.action != "" {
			t.Fatalf("%q: %+v action=%q", text, d, p.action)
		}
	}
}

func TestUnmanagedPlaybackKeepsYandexControls(t *testing.T) {
	p := &playbackFixture{}
	e := &Engine{Playback: p}
	if d := e.Decide(t.Context(), "Пауза"); d.Route != "yandex" {
		t.Fatalf("%+v", d)
	}
	if p.action != "" {
		t.Fatal("controlled foreign player")
	}
}
