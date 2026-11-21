package assistant

import (
	"context"
	"testing"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

func TestNamedMusicNegationStaysInsideTitle(t *testing.T) {
	for _, command := range []struct{ text, title string }{
		{`Включи песню «Не надо» с сайта svk.su`, "Не надо"},
		{`Включи песню Не надо с сайта svk.su`, "Не надо"},
		{`Проиграй трек "Не нужно" с сайта svk.su`, "Не нужно"},
		{`Поставь песню Не хочу с сайта svk.su`, "Не хочу"},
		{`Включи песню «Не включай» с сайта svk.su`, "Не включай"},
		{`Включи мне песню «Не надо» с сайта svk.su`, "Не надо"},
	} {
		for _, path := range []string{"start", "default engine", "saved defaults"} {
			t.Run(path+"/"+command.text, func(t *testing.T) {
				tools, fetched := titleMusicTools(t, `<a href="/other.mp3">Other</a><a href="/wanted.mp3">`+command.title+`</a>`)
				var played sound.Request
				ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "negation-title"), func(_ context.Context, r sound.Request) error { played = r; return nil })
				if path == "start" {
					if _, err := tools.StartMusic(ctx, command.text); err != nil {
						t.Fatal(err)
					}
				} else {
					engine := &bridge.Engine{Music: tools}
					if path == "saved defaults" {
						if err := engine.SetRouting(bridge.DefaultRoutingConfig()); err != nil {
							t.Fatal(err)
						}
					}
					result, err := engine.Resolve(ctx, command.text)
					if err != nil || result.Route != "music" {
						t.Fatalf("resolution=%+v err=%v", result, err)
					}
				}
				tracks, err := tools.Playback.queue.Tracks(played.URL)
				if err != nil || len(tracks) != 1 || tracks[0].URL != "https://svk.su/wanted.mp3" || tracks[0].Title != command.title || len(*fetched) != 1 {
					t.Fatalf("title-selected playback=%+v fetches=%v err=%v", tracks, *fetched, err)
				}
			})
		}
	}
}

func TestNamedMusicCommandNegationStillPreventsPlayback(t *testing.T) {
	for _, text := range []string{
		`Не включай песню «Не надо» с сайта svk.su`,
		`Не надо включать песню «Не надо» с сайта svk.su`,
		`Включи песню «Не надо» с сайта svk.su, но не включай`,
		`Включи песню «Не надо», не нужно, с сайта svk.su`,
		`Включи, не надо, песню «Не надо» с сайта svk.su`,
	} {
		t.Run(text, func(t *testing.T) {
			tools, fetched := titleMusicTools(t, `<a href="/wanted.mp3">Не надо</a>`)
			ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "negated-command"), func(context.Context, sound.Request) error { t.Fatal("negated command played audio"); return nil })
			if _, err := tools.StartMusic(ctx, text); err == nil {
				t.Fatal("negated command was accepted")
			}
			engine := &bridge.Engine{Music: tools}
			result, err := engine.Resolve(ctx, text)
			if err != nil || result.Route != "yandex" || len(*fetched) != 0 {
				t.Fatalf("resolution=%+v fetches=%v err=%v", result, *fetched, err)
			}
			requireNoMusicQueue(t, tools)
		})
	}
}

func TestNamedMusicRetainsCustomRulePriorityAndLiteralConstraints(t *testing.T) {
	for _, modifiedDefault := range []bool{false, true} {
		t.Run(map[bool]string{false: "custom rule before default", true: "customized default rule"}[modifiedDefault], func(t *testing.T) {
			tools, fetched := titleMusicTools(t, `<a href="/wanted.mp3">Не надо</a>`)
			cfg := bridge.DefaultRoutingConfig()
			if modifiedDefault {
				for i := range cfg.Rules {
					if cfg.Rules[i].Name == "music_from_site" {
						cfg.Rules[i].Match.ContainsNone = append(cfg.Rules[i].Match.ContainsNone, "не")
					}
				}
			} else {
				cfg.Rules = append([]bridge.RoutingRule{{Name: "custom title block", Route: "yandex", Match: bridge.RoutingMatch{ContainsAny: []string{"не надо"}}}}, cfg.Rules...)
			}
			engine := &bridge.Engine{Music: tools}
			if err := engine.SetRouting(cfg); err != nil {
				t.Fatal(err)
			}
			ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "custom-rule"), func(context.Context, sound.Request) error { t.Fatal("custom rule was bypassed"); return nil })
			result, err := engine.Resolve(ctx, `Включи песню «Не надо» с сайта svk.su`)
			if err != nil || result.Route != "yandex" || len(*fetched) != 0 {
				t.Fatalf("resolution=%+v fetches=%v err=%v", result, *fetched, err)
			}
			requireNoMusicQueue(t, tools)
		})
	}
}

func TestGenericMusicDoesNotTreatLaterNounAsTitle(t *testing.T) {
	for _, text := range []string{
		`Включи музыку, но песню не надо с сайта svk.su`,
		`Включи радио, но трек не нужно с сайта svk.su`,
	} {
		for _, path := range []string{"start", "default engine"} {
			t.Run(path+"/"+text, func(t *testing.T) {
				tools, fetched := titleMusicTools(t, `<a href="/wanted.mp3">Wanted</a>`)
				plays := 0
				ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "generic-negation"), func(context.Context, sound.Request) error { plays++; return nil })
				if path == "start" {
					if _, err := tools.StartMusic(ctx, text); err == nil {
						t.Error("negated generic command was accepted")
					}
				} else {
					engine := &bridge.Engine{Music: tools}
					result, err := engine.Resolve(ctx, text)
					if err != nil || result.Route != "yandex" {
						t.Errorf("resolution=%+v err=%v", result, err)
					}
				}
				if plays != 0 || len(*fetched) != 0 {
					t.Errorf("negated command played=%d fetched=%v", plays, *fetched)
				}
				requireNoMusicQueue(t, tools)
			})
		}
	}
}
