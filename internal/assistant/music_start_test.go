package assistant

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

func TestCanStartMusicRequiresExplicitCurrentCommandAndURL(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("CanStartMusic fetched a website")
		return nil, nil
	})
	for _, text := range []string{
		"Включи музыку с сайта https://audio.example/music.",
		"Поставь аудио с сайта abc точка су слэш music",
		"Алиса, пожалуйста, включи музыку с https://audio.example/music!",
		"Спроси нейросеть: проиграй песни с https://audio.example/Track.MP3.",
	} {
		if !tools.CanStartMusic(text) {
			t.Errorf("missed explicit request: %q", text)
		}
	}
	for _, text := range []string{
		"Включи музыку", "Включи музыку с сайта", "Продолжи ту музыку",
		"Не включи музыку с https://audio.example/music",
		"Не надо включать музыку с https://audio.example/music",
		"Как включить музыку с https://audio.example/music?",
		"Объясни, как сказать включи музыку с https://audio.example/music",
		"Что я вчера просил: включи музыку с https://audio.example/music?",
		"Вчера я сказал: включи музыку с https://audio.example/music",
		"Прочитай сайт https://audio.example/music",
	} {
		if tools.CanStartMusic(text) {
			t.Errorf("unexpected direct playback: %q", text)
		}
	}
	text := "Включи музыку с https://audio.example/music"
	tools.Config.PlaySound = false
	if tools.CanStartMusic(text) {
		t.Fatal("playback disabled")
	}
	tools.Config.PlaySound, tools.Config.Web = true, false
	if tools.CanStartMusic(text) {
		t.Fatal("web disabled")
	}
	tools.Config.Web, tools.Playback = true, nil
	if tools.CanStartMusic(text) {
		t.Fatal("missing music player")
	}
}

func TestStartMusicUsesNormalizedLiteralSourceAndRealDiscovery(t *testing.T) {
	for _, tc := range []struct{ text, source string }{
		{"Включи музыку с https://audio.example/Music!", "https://audio.example/Music"},
		{"Включи музыку с сайта абв точка су слэш music.", "https://abv.su/music"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			var fetched []string
			tools := musicTools(t, func(req *http.Request) (*http.Response, error) {
				fetched = append(fetched, req.URL.String())
				body := `<script type="application/ld+json">{"name":"Track One","contentUrl":"https://audio.example/one.mp3"}</script>`
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})
			var played sound.Request
			ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "office"), func(_ context.Context, req sound.Request) error { played = req; return nil })
			message, err := tools.StartMusic(ctx, tc.text)
			if err != nil || message != "Включаю музыку." {
				t.Fatalf("message=%q error=%v", message, err)
			}
			if len(fetched) != 1 || fetched[0] != tc.source || !strings.Contains(played.URL, "/music-stream/") || played.Title != "Track One" {
				t.Fatalf("source or discovered track changed: fetched=%v played=%+v", fetched, played)
			}
		})
	}
}

func TestStartMusicRequiresLivePlayerAndPreservesCancellation(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) { t.Fatal("read without live player"); return nil, nil })
	if _, err := tools.StartMusic(t.Context(), "Включи музыку с https://audio.example/music"); err == nil || !strings.Contains(err.Error(), "current speaker") {
		t.Fatalf("error=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := tools.StartMusic(ctx, "Включи музыку с https://audio.example/music"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	if _, err := tools.StartMusic(t.Context(), "Включи музыку"); err == nil {
		t.Fatal("guessed URL from a request without an address")
	}
}

func TestStartMusicPreservesDirectAudioExtension(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("direct audio was fetched as a page")
		return nil, nil
	})
	var played sound.Request
	ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "office"), func(_ context.Context, req sound.Request) error { played = req; return nil })
	if _, err := tools.StartMusic(ctx, "Включи музыку с https://audio.example/Live.M3U8."); err != nil || played.URL != "https://audio.example/Live.M3U8" {
		t.Fatalf("extension changed: played=%+v error=%v", played, err)
	}
}

func TestUnresolvedMusicWebsiteRequestsNeedSourceClarification(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("clarification fetched a website")
		return nil, nil
	})
	handler, ok := any(tools).(interface{ NeedsMusicSource(string) bool })
	if !ok {
		t.Fatal("music requests with an unreadable address have no clarification handler")
	}
	for _, text := range []string{"Включи музыку с сайта абв точка слух", "Включи музыку с сайта abc.invalid", "Проиграй песни со страницы", "Алиса, включи музыку с сайта"} {
		if !handler.NeedsMusicSource(text) {
			t.Errorf("missing clarification: %q", text)
		}
	}
	for _, text := range []string{"Включи музыку с сайта abc.su", "Включи музыку", "Не включи музыку с сайта абв точка слух", "Как включить музыку с сайта абв точка слух?", "Вчера я просил включи музыку с сайта абв точка слух", "Прочитай сайт абв точка слух", "Включи музыку не с сайта абв точка слух", "Включи музыку с сайта не абв точка слух", "Включи музыку, только не с сайта абв точка слух"} {
		if handler.NeedsMusicSource(text) {
			t.Errorf("unexpected music clarification: %q", text)
		}
	}
	tools.Config.PlaySound = false
	if handler.NeedsMusicSource("Включи музыку с сайта абв точка слух") {
		t.Fatal("clarification despite disabled playback")
	}
}

func TestMusicSourceNegationDoesNotLaunchLiteralURL(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) { t.Fatal("negation fetched music"); return nil, nil })
	if tools.CanStartMusic("Включи музыку не с сайта https://audio.example.org") {
		t.Fatal("negated source started music")
	}
}
