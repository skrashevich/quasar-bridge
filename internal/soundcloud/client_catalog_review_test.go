package soundcloud

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestStreamMissingAndTransientStatus(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			hc := fixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
			c, _ := NewClient(Config{AccessToken: "user"}, hc)
			_, err := c.Stream(t.Context(), Track{URN: "soundcloud:tracks:2"})
			if status == http.StatusServiceUnavailable {
				if err == nil || errors.Is(err, ErrUnavailable) {
					t.Fatalf("transient error %v", err)
				}
			} else if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("missing error %v", err)
			}
		})
	}
}
func TestWebStreamMissingAlternativesPreserveTransientFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second int
		unavailable   bool
	}{{"both missing", 404, 410, true}, {"transient then missing", 503, 404, false}, {"missing then transient", 410, 503, false}} {
		t.Run(tc.name, func(t *testing.T) {
			hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/160" {
					w.WriteHeader(tc.first)
				} else {
					w.WriteHeader(tc.second)
				}
			})
			c, _ := NewClient(Config{API: "web", ClientID: "public-id"}, hc)
			a := transcoding{URL: "https://api-v2.soundcloud.com/160", Preset: "aac_160k"}
			a.Format.Protocol = "hls"
			b := transcoding{URL: "https://api-v2.soundcloud.com/96", Preset: "aac_96k"}
			b.Format.Protocol = "hls"
			_, err := c.Stream(t.Context(), Track{URN: "soundcloud:tracks:2", transcodings: []transcoding{a, b}})
			if tc.unavailable {
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("missing error %v", err)
				}
			} else {
				status, ok := errors.AsType[*httpStatusError](err)
				if !ok || status.Code != 503 {
					t.Fatalf("transient hidden by missing candidate: %v", err)
				}
			}
		})
	}
}
func TestMissingStreamBetweenPlayableTracksAdvances(t *testing.T) {
	for _, status := range []int{404, 410, 503} {
		for _, terminal := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/callback=%v", status, terminal), func(t *testing.T) {
				p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/tracks/soundcloud:tracks:2/streams":
						w.WriteHeader(status)
					default:
						if strings.HasSuffix(r.URL.Path, "/streams") {
							fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/track.m3u8"}`)
							return
						}
						fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000},{"id":2,"title":"Missing","duration":120000},{"id":3,"title":"Third","duration":120000}]}`)
					}
				})
				recorder := new(providerRecorder)
				ctx := providerContext(t, "speaker", recorder)
				first := providerStart(t, p, ctx, recorder)
				if terminal {
					handled, err := p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first))
					if !handled || (status == 503) != (err != nil) {
						t.Fatalf("callback: %v %v", handled, err)
					}
				} else {
					text, err := p.Handle(ctx, "следующий")
					if err != nil || (status == 503) != (text != "") {
						t.Fatalf("next: %q %v", text, err)
					}
				}
				requests := recorder.all()
				if status == 503 {
					if len(requests) != 1 {
						t.Fatalf("transient failure skipped to Third: %+v", requests)
					}
				} else if len(requests) != 2 || requests[1].Title != "Third" {
					t.Fatalf("playback requests %+v", requests)
				}
			})
		}
	}
}

func TestInitialMissingStreamSkipsToNextSearchResult(t *testing.T) {
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tracks/soundcloud:tracks:1/streams":
			w.WriteHeader(http.StatusNotFound)
		case "/tracks/soundcloud:tracks:2/streams":
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/available.m3u8"}`)
		default:
			fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000},{"id":2,"title":"Second","duration":120000}]}`)
		}
	})
	recorder := new(providerRecorder)
	ctx := providerContext(t, "speaker", recorder)
	if text, err := p.Handle(ctx, "включи First SoundCloud"); err != nil || text != "" {
		t.Fatalf("play: %q %v", text, err)
	}
	requests := recorder.all()
	if len(requests) != 1 || requests[0].Title != "Second" {
		t.Fatalf("playback %+v", requests)
	}
}
