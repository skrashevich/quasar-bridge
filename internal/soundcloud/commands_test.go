package soundcloud

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestProviderRouting(t *testing.T) {
	p := providerFixture(t, nil)
	ctx := providerContext(t, "speaker", new(providerRecorder))
	for _, text := range []string{"включи трек First", "включи музыку", "найди песни Artist", "включи альбом Album", "включи плейлист List", "включи исполнителя Artist", "play song First", "включи саунд клауд", "включи https://soundcloud.com/artist/song", "https://soundcloud.com/artist/song", "soundcloud.com/artist/song", "play SoundCloud", "поставь First на саундклауде", "включи на SoundCloud песню Свет", "включи на SoundCloud Светлана", "включи SoundCloud Кондиционер", "включи трек Кондиционер", "включи песню Яркость на 50", "включи песню Почему", "включи трек Не хочу", "play song What Is Love"} {
		t.Run(text, func(t *testing.T) {
			if !p.CanHandle(ctx, text) {
				t.Fatal("musical request not claimed")
			}
		})
	}
	for _, text := range []string{"не включай SoundCloud", "не ставь музыку", "что такое SoundCloud", "как работает soundcloud", "включи свет", "включи чайник", "включи кондиционер", "включи бра", "поставь яркость на 50", "включи неизвестное устройство", "найди неизвестное устройство", "включи телевизор", "включи таймер", "включи https://example.com/song", "включи example.com/song", "включи музыку на spotify", "включи музыку на Яндексе", "включи AirPlay", "сколько времени", "что играет", "следующий трек", "пауза", "какая погода на soundcloud", "выключи свет SoundCloud"} {
		t.Run(text, func(t *testing.T) {
			if p.CanHandle(ctx, text) {
				t.Fatal("unrelated request claimed")
			}
		})
	}
	p.SetDefault(false)
	if p.CanHandle(ctx, "включи First") || !p.CanHandle(ctx, "включи First SoundCloud") {
		t.Fatal("default policy")
	}
	p.cfg.Enabled = false
	if p.CanHandle(ctx, "включи SoundCloud") {
		t.Fatal("disabled")
	}
}
func TestProviderCommandDetails(t *testing.T) {
	for _, tc := range []struct {
		text, action, query string
		kind                Kind
	}{
		{"включи https://soundcloud.com/artist/song", "resolve", "https://soundcloud.com/artist/song", TrackKind},
		{"ВКЛЮЧИ https://on.soundcloud.com/AbCdE?Token=MiXeD&Other=Aa", "resolve", "https://on.soundcloud.com/AbCdE?Token=MiXeD&Other=Aa", TrackKind},
		{"PLAY HTTPS://ON.SOUNDCLOUD.COM/AbCdE?Token=MiXeD", "resolve", "https://on.soundcloud.com/AbCdE?Token=MiXeD", TrackKind},
		{"включи артиста First на саундклауде", "search", "first", ArtistKind},
		{"найди песню First SoundCloud", "search", "first", TrackKind},
		{"включи плейлист Collection SoundCloud", "search", "collection", PlaylistKind},
		{"включи альбом Collection SoundCloud", "search", "collection", AlbumKind},
		{"включи избранное SoundCloud", "likes", "", TrackKind},
	} {
		t.Run(tc.text, func(t *testing.T) {
			c := parseProviderCommand(tc.text)
			if c.action != tc.action || c.query != tc.query || c.kind != tc.kind {
				t.Fatalf("got %+v", c)
			}
		})
	}
}

func TestProviderResolvePreservesLinkPathAndQuery(t *testing.T) {
	const expected = "https://on.soundcloud.com/AbCdE?Token=MiXeD&Other=Aa"
	resolved := make(chan string, 1)
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/resolve" {
			resolved <- r.URL.Query().Get("url")
			fmt.Fprint(w, `{"id":1,"kind":"track","title":"Resolved","duration":120000}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/a.m3u8"}`)
			return
		}
		t.Errorf("unexpected %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	ctx := providerContext(t, "speaker", new(providerRecorder))
	if text, err := p.Handle(ctx, "ВКЛЮЧИ HTTPS://ON.SOUNDCLOUD.COM/AbCdE?Token=MiXeD&Other=Aa"); err != nil || text != "" {
		t.Fatalf("resolve %q %v", text, err)
	}
	select {
	case got := <-resolved:
		if got != expected {
			t.Fatalf("resolver received %q, want %q", got, expected)
		}
	default:
		t.Fatal("resolver was not called")
	}
}
