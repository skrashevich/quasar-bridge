package soundcloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"quasarbridge/internal/sound"
)

func TestPersonalVoiceCommandParsing(t *testing.T) {
	for _, tc := range []struct {
		text, action, query string
		explicit            bool
	}{
		{"включи мой плейлист Ночной бег на SoundCloud", "personal_playlist", "ночной бег", true},
		{"включи мой плейлист Ночной бег", "personal_playlist", "ночной бег", false},
		{"покажи мои плейлисты на SoundCloud", "library", "", true},
		{"моя библиотека", "library", "", false},
		{"включи мои треки на SoundCloud", "uploads", "", true},
		{"включи мои песни", "uploads", "", false},
		{"включи мои лайки", "likes", "", false},
	} {
		t.Run(tc.text, func(t *testing.T) {
			got := parseProviderCommand(tc.text)
			if got.action != tc.action || got.query != tc.query || got.explicit != tc.explicit {
				t.Fatalf("command=%+v", got)
			}
		})
	}
	for _, text := range []string{"включи мои шторы", "включи мой телевизор", "покажи мои треки на SoundCloud", "назови мой плейлист Осень на SoundCloud", "list my uploads on SoundCloud", "не включай мой плейлист Ночной бег", "что такое моя библиотека", "включи мой плейлист Ночной бег на Яндексе"} {
		if got := parseProviderCommand(text); got.action != "" {
			t.Fatalf("non-SoundCloud intent became personal music: %q %+v", text, got)
		}
	}
}

func TestProviderPersonalLibraryAdmissionAndNativeQueue(t *testing.T) {
	client := libraryWebFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/library/all":
			fmt.Fprint(w, `{"collection":[{"type":"playlist","playlist":{"id":7,"title":"Ночной бег","sharing":"private"}}]}`)
		case "/playlists/7", "/users/42/tracks":
			tracks := `[{"id":11,"title":"Own full","duration":120000,"sharing":"private","metadata_artist":"svk","media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/11","preset":"aac_160","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}]`
			if r.URL.Path == "/playlists/7" {
				fmt.Fprintf(w, `{"kind":"playlist","tracks":%s}`, tracks)
			} else {
				fmt.Fprintf(w, `{"collection":%s}`, tracks)
			}
		case "/media/11":
			fmt.Fprint(w, `{"url":"https://cf-media.sndcdn.com/full.m3u8"}`)
		case "/me":
			fmt.Fprint(w, `{"id":42,"username":"owner"}`)
		default:
			t.Error("personal voice escaped authenticated collection", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	relay, err := NewRelay("http://bridge.invalid", client.http)
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider(Config{Enabled: true, API: "web", QueueLimit: 2}, client, relay)
	r := new(providerRecorder)
	ctx := providerContext(t, "personal-speaker", r)
	for _, text := range []string{"включи мой плейлист Ночной бег", "покажи мои плейлисты", "включи мои треки", "включи мои лайки"} {
		if p.CanHandle(ctx, text) {
			t.Fatal("personal command stole non-default playback", text)
		}
	}
	if !p.CanHandle(ctx, "включи мой плейлист Ночной бег на SoundCloud") {
		t.Fatal("explicit personal playlist was not admitted")
	}
	if text, err := p.Handle(ctx, "включи мой плейлист Ночной бег на SoundCloud"); err != nil || text != "" {
		t.Fatal(text, err)
	}
	first := r.all()[0]
	if first.Provider != "SoundCloud" || first.TrackID != "soundcloud:tracks:11" || first.Title != "Own full" || first.Subtitle != "svk" || first.Format != "HLS" || first.SessionID == "" || first.CallbackToken == "" {
		t.Fatalf("personal playlist did not use native queue metadata: %+v", first)
	}
	if _, err := p.HandleEvent(ctx, EventPrefix+"started", eventPayload(first)); err != nil {
		t.Fatal(err)
	}
	if !p.CanHandle(ctx, "покажи мои плейлисты") || !p.CanHandle(ctx, "включи мои лайки") {
		t.Fatal("current SoundCloud ownership did not admit personal commands")
	}
	for _, text := range []string{"покажи мои треки на SoundCloud", "назови мой плейлист Ночной бег на SoundCloud", "перечисли мои песни на SoundCloud"} {
		if p.CanHandle(ctx, text) {
			t.Fatal("read-only personal request was admitted as playback", text)
		}
		if answer, err := p.Handle(ctx, text); answer != "" || err != nil || len(r.all()) != 1 {
			t.Fatal("read-only personal request started audio", answer, err)
		}
	}
	answer, err := p.Handle(ctx, "покажи мои плейлисты")
	if err != nil || !strings.Contains(answer, "Ночной бег") || len(r.all()) != 1 || !p.CanDeliver(ctx, first) {
		t.Fatal("library listing mutated playback", answer, err)
	}
	if text, err := p.Handle(ctx, "включи мои треки"); err != nil || text != "" || len(r.all()) != 2 {
		t.Fatal("owned uploads did not play", text, err)
	}
	if p.CanHandle(ctx, "включи мои шторы") {
		t.Fatal("personal commands stole home control")
	}
	foreign := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: "foreign", PlayerState: "Playing"})
	if p.CanHandle(foreign, "включи мой плейлист Ночной бег") {
		t.Fatal("personal command replaced a foreign source")
	}
}

func TestProviderPersonalErrorsExplainWebAuthorization(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrapped: %w", ErrWebChallenge), "CAPTCHA"},
		{fmt.Errorf("wrapped: %w", ErrAuthorization), "веб-сессию"},
	} {
		answer, err := providerError(t.Context(), tc.err)
		if err != nil || !strings.Contains(answer, tc.want) || strings.Contains(answer, "Попробуйте другой трек") {
			t.Fatal("personal authorization failure was generic", answer, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := providerError(ctx, ErrWebChallenge); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled personal operation announced an unrelated challenge", err)
	}
}
