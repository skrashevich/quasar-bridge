package soundcloud

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestLikesDistinguishesEmptyAndUnplayableCollections(t *testing.T) {
	for _, mode := range []string{"web", "official"} {
		for _, tc := range []struct {
			name, items string
			wantTracks  int
			unplayable  bool
		}{
			{"empty", `[]`, 0, false},
			{"unsupported", `[{"track":{"id":1,"title":"Unavailable","duration":120000,"access":"blocked"}}]`, 0, true},
			{"mixed", `[{"track":{"id":1,"title":"Unavailable","duration":120000,"access":"blocked"}},{"track":{"id":2,"title":"Available","duration":120000,"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/2","preset":"aac_160","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}}]`, 1, false},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/me" {
						fmt.Fprint(w, `{"id":42}`)
						return
					}
					if r.URL.Path != "/users/42/track_likes" && r.URL.Path != "/me/likes/tracks" {
						t.Error("unexpected likes endpoint", r.URL.Path)
						http.NotFound(w, r)
						return
					}
					fmt.Fprintf(w, `{"collection":%s}`, tc.items)
				})
				cfg := Config{API: mode, ClientID: "public"}
				if mode == "web" {
					cfg.WebAccessToken = "web-user"
				} else {
					cfg.AccessToken = "official-user"
				}
				client, err := NewClient(cfg, hc)
				if err != nil {
					t.Fatal(err)
				}
				tracks, err := client.Likes(t.Context(), 2)
				if errors.Is(err, ErrNoPlayableLikes) != tc.unplayable || err != nil && !tc.unplayable || len(tracks) != tc.wantTracks {
					t.Fatal("empty likes and unavailable likes were confused", tracks, err)
				}
				if len(tracks) != 0 && tracks[0].Title != "Available" {
					t.Fatal("unavailable track was selected", tracks)
				}
			})
		}
	}
}

func TestProviderUnsupportedLikedStreamsExplainsAvailability(t *testing.T) {
	client := libraryWebFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me":
			fmt.Fprint(w, `{"id":42}`)
		case "/users/42/track_likes":
			fmt.Fprint(w, `{"collection":[{"track":{"id":1,"title":"Unsupported full stream","duration":120000,"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/1","preset":"unsupported","format":{"protocol":"dash","mime_type":"audio/unknown"}}]}}}]}`)
		default:
			t.Error("unplayable likes unexpectedly started a stream", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	if tracks, err := client.Likes(t.Context(), 2); !errors.Is(err, ErrNoPlayableLikes) || len(tracks) != 0 {
		t.Fatal("unsupported streams were reported as empty likes", tracks, err)
	}
	relay, err := NewRelay("http://bridge.invalid", client.http)
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider(Config{Enabled: true, API: "web", QueueLimit: 2}, client, relay)
	r := new(providerRecorder)
	answer, err := p.Handle(providerContext(t, "likes-speaker", r), "включи мои лайки на SoundCloud")
	if err != nil || !strings.Contains(answer, "есть треки") || !strings.Contains(answer, "воспроизведения на колонке") || strings.Contains(answer, "ничего не найдено") || len(r.all()) != 0 {
		t.Fatal("unplayable personal likes did not get an honest voice answer", answer, err, r.all())
	}
}

func TestProviderMixedLikedStreamsPlaysAvailableTrack(t *testing.T) {
	client := libraryWebFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me":
			fmt.Fprint(w, `{"id":42}`)
		case "/users/42/track_likes":
			fmt.Fprint(w, `{"collection":[{"track":{"id":1,"title":"Unsupported","duration":120000,"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/1","preset":"unsupported","format":{"protocol":"dash","mime_type":"audio/unknown"}}]}}},{"track":{"id":2,"title":"Available","duration":120000,"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/2","preset":"aac_160","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}}]}`)
		case "/media/2":
			fmt.Fprint(w, `{"url":"https://cf-media.sndcdn.com/full.m3u8"}`)
		default:
			t.Error("mixed likes selected an unsupported stream", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	relay, err := NewRelay("http://bridge.invalid", client.http)
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider(Config{Enabled: true, API: "web", QueueLimit: 2}, client, relay)
	r := new(providerRecorder)
	answer, err := p.Handle(providerContext(t, "likes-speaker", r), "включи мои лайки на SoundCloud")
	requests := r.all()
	if err != nil || answer != "" || len(requests) != 1 || requests[0].TrackID != "soundcloud:tracks:2" || requests[0].Title != "Available" {
		t.Fatal("mixed likes did not play the available full stream", answer, err, requests)
	}
}

func TestOfficialLikesKeepsScanningBlockedPagesForPlayableTrack(t *testing.T) {
	pages := 0
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/likes/tracks" {
			t.Error("unexpected likes endpoint", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		pages++
		if r.URL.Query().Get("page") == "second" {
			fmt.Fprint(w, `{"collection":[{"id":100,"title":"Available after blocked page","duration":120000}]}`)
			return
		}
		fmt.Fprint(w, `{"collection":[`)
		for i := range inspectionLimit(1) + 1 {
			if i != 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"id":%d,"title":"Blocked","access":"blocked"}`, i+1)
		}
		fmt.Fprint(w, `],"next_href":"?page=second"}`)
	})
	client, err := NewClient(Config{API: "official", AccessToken: "user-token"}, hc)
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := client.Likes(t.Context(), 1)
	if err != nil || len(tracks) != 1 || tracks[0].URN != "soundcloud:tracks:100" || pages != 2 {
		t.Fatal("raw inspection limit hid a later playable like", tracks, err, pages)
	}
}
