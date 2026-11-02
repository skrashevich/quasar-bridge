package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/coder/websocket"
	"quasarbridge/internal/llm"
)

type airPlayFunc func(context.Context, string) (string, error)

func (f airPlayFunc) Control(ctx context.Context, action string) (string, error) {
	return f(ctx, action)
}

func TestAirPlayCommandsBypassAllBackends(t *testing.T) {
	for _, tc := range []struct{ text, action string }{
		{"Алиса, включи эйрплей", "start"}, {"Включи AirPlay, пожалуйста!", "start"},
		{"запусти эйр плей", "start"}, {"enable airplay", "start"}, {"turn on air play", "start"},
		{"Алиса, выключи эйрплей", "stop"}, {"останови airplay", "stop"}, {"disable airplay", "stop"},
		{"turn off airplay", "stop"}, {"статус эйрплей", "status"}, {"эйрплей работает?", "status"}, {"airplay status", "status"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			calls := 0
			e := &Engine{AirPlay: airPlayFunc(func(ctx context.Context, action string) (string, error) {
				calls++
				if action != tc.action {
					t.Fatalf("action=%q, want %q", action, tc.action)
				}
				return "готово", nil
			})}
			e.LLM = completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
				t.Fatal("AirPlay called model")
				return "", nil
			})
			e.Assistant = e.LLM
			e.Home = homeFunc(func(context.Context, string) (string, error) { t.Fatal("AirPlay called home/tools"); return "", nil })
			cfg := DefaultRoutingConfig()
			cfg.DefaultRoute = "home"
			if err := e.SetRouting(cfg); err != nil {
				t.Fatal(err)
			}
			r, err := e.Resolve(t.Context(), tc.text)
			if err != nil || r.Route != "airplay" || r.Answer != "готово" || calls != 1 {
				t.Fatalf("result=%+v err=%v calls=%d", r, err, calls)
			}
		})
	}
}

func TestAirPlayOnlyRecognizesExplicitWholeCommands(t *testing.T) {
	e := &Engine{AirPlay: airPlayFunc(func(context.Context, string) (string, error) {
		t.Fatal("executed ambiguous or negated command")
		return "", nil
	})}
	cfg := DefaultRoutingConfig()
	cfg.Rules = nil
	cfg.DefaultRoute = "yandex"
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"не включай эйрплей", "не включи airplay", "не надо включать airplay", "как включить airplay", "расскажи про airplay", "найди в истории включи airplay", "выключи свет и включи airplay", "включи airplay и выключи свет", "включи airplay в спальне", "don't enable airplay", "не выключай эйрплей", "stop music", "включи музыку"} {
		d := e.Decide(t.Context(), text)
		if d.Route != "yandex" {
			t.Errorf("%q routed as %+v", text, d)
		}
	}
	if d := (&Engine{}).Decide(t.Context(), "включи airplay"); d.Route == "airplay" {
		t.Fatal("disabled controller claimed query", d)
	}
}

func TestNativeAirPlayFailureDoesNotBlameModel(t *testing.T) {
	n, _, down := soundSession(t, resolverFunc(func(context.Context, string) (Resolution, error) {
		return Resolution{Route: "airplay"}, errors.New("bind failed")
	}))
	defer n.Close()
	if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("airplay-failure")); err != nil {
		t.Fatal(err)
	}
	for {
		f := nextSoundFrame(t, down)
		if r := f.Server.GetEvent().GetAliceResponse(); r != nil {
			if got := r.GetVoiceResponse().GetOutputSpeech().GetText(); got != "Не удалось изменить состояние AirPlay. Проверьте журнал Quasar Bridge." {
				t.Fatalf("speech=%q", got)
			}
			break
		}
	}
}

type airPlayMediaFake struct {
	active bool
	action string
}

func (f *airPlayMediaFake) Active(context.Context) bool { return f.active }
func (f *airPlayMediaFake) Control(_ context.Context, action string) (string, error) {
	return "", nil
}
func (f *airPlayMediaFake) ControlMedia(_ context.Context, action string) (string, error) {
	f.action = action
	return "", nil
}

func TestAirPlayOwnsMediaCommandsOnlyWithSender(t *testing.T) {
	for _, tc := range []struct{ text, action string }{
		{"Алиса, пауза", "pause"}, {"продолжи музыку", "resume"}, {"стоп", "stop"},
		{"следующий трек", "next"}, {"предыдущий трек", "previous"}, {"громче", "volume_up"}, {"сделай тише", "volume_down"},
		{"громкость 5", "volume:50"}, {"Алиса, установи громкость на десять", "volume:100"}, {"громкость 0", "volume:0"}, {"громкость 35 процентов", "volume:35"}, {"поставь громкость на 5%", "volume:5"},
	} {
		for _, active := range []bool{false, true} {
			t.Run(tc.text+map[bool]string{false: "/inactive", true: "/active"}[active], func(t *testing.T) {
				control := &airPlayMediaFake{active: active}
				e := &Engine{AirPlay: control}
				cfg := DefaultRoutingConfig()
				cfg.DefaultRoute = "yandex"
				if err := e.SetRouting(cfg); err != nil {
					t.Fatal(err)
				}
				d := e.Decide(t.Context(), tc.text)
				if !active {
					if d.Route != "yandex" {
						t.Fatalf("inactive receiver claimed media: %+v", d)
					}
					return
				}
				if d.Route != "airplay" || d.Source != "airplay_media" {
					t.Fatalf("sender media lost: %+v", d)
				}
				if _, err := e.ResolveDecision(t.Context(), tc.text, d); err != nil {
					t.Fatal(err)
				}
				if control.action != tc.action {
					t.Fatalf("action=%q want=%q", control.action, tc.action)
				}
			})
		}
	}
}

func TestAirPlayDoesNotClaimUnrelatedVolume(t *testing.T) {
	e := &Engine{AirPlay: &airPlayMediaFake{active: true}}
	cfg := DefaultRoutingConfig()
	cfg.Rules = nil
	cfg.DefaultRoute = "yandex"
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"не делай громче", "сделай телевизор тише", "прибавь яркость", "громче на кухне", "установи громкость телевизора на пять", "пауза и выключи свет", "громкость 11", "громкость 101 процент", "громкость телевизора 5", "не ставь громкость 5", "громкость минус один"} {
		if d := e.Decide(t.Context(), text); d.Route != "yandex" {
			t.Fatalf("claimed %q: %+v", text, d)
		}
	}
}
