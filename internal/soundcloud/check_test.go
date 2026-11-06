package soundcloud

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type accessFixture struct {
	mu          sync.Mutex
	requests    map[string]int
	broken      string
	stall       bool
	master      bool
	missing     bool
	catalog     string
	unavailable map[string]bool
}

func newAccessFixture(t *testing.T, f *accessFixture) Config {
	t.Helper()
	f.requests = make(map[string]int)
	server, roots := proxyTLSFixture(t, f.serve)
	proxy := newSOCKSFixture(t, server.Listener.Addr().String(), "check-user", "check-secret", false, false)
	previous := http.DefaultTransport
	transport := previous.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	transport.Proxy = func(*http.Request) (*url.URL, error) {
		return nil, errors.New("direct access forbidden by fixture")
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous; transport.CloseIdleConnections() })
	return Config{API: "web", ProxyURL: proxy.proxyURL("socks5h")}
}

func (f *accessFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests[r.Host+r.URL.Path]++
	f.mu.Unlock()
	if f.stall && r.URL.Path == "/search/tracks" {
		<-r.Context().Done()
		return
	}
	if r.URL.Path == f.broken {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if f.unavailable[r.URL.Path] {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Host + r.URL.Path {
	case "soundcloud.com/":
		io.WriteString(w, `<script src="https://a-v2.sndcdn.com/assets/app.js"></script>`)
	case "a-v2.sndcdn.com/assets/app.js":
		io.WriteString(w, `var config={client_id:"0123456789abcdef0123456789abcdef"};`)
	case "secure.soundcloud.com/oauth/token":
		io.WriteString(w, `{"access_token":"probe-token","expires_in":3600}`)
	case "api-v2.soundcloud.com/search/tracks", "api.soundcloud.com/tracks":
		if f.catalog != "" {
			io.WriteString(w, f.catalog)
			return
		}
		io.WriteString(w, `{"collection":[{"id":12,"urn":"soundcloud:tracks:12","title":"Song","artwork_url":"https://i1.sndcdn.com/cover.jpg","access":"playable","policy":"ALLOW","streamable":true,"user":{"id":4,"username":"Artist"},"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/1","preset":"aac_160k","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}]}`)
	case "api-v2.soundcloud.com/media/1", "api-v2.soundcloud.com/media/2", "api-v2.soundcloud.com/media/3", "api-v2.soundcloud.com/media/4":
		io.WriteString(w, `{"url":"https://cf-hls-media.sndcdn.com/playlist.m3u8"}`)
	case "api.soundcloud.com/tracks/soundcloud:tracks:12/streams":
		io.WriteString(w, `{"hls_aac_160_url":"https://cf-hls-media.sndcdn.com/playlist.m3u8"}`)
	case "cf-hls-media.sndcdn.com/playlist.m3u8":
		if f.missing {
			io.WriteString(w, "#EXTM3U\n#EXT-X-ENDLIST\n")
		} else if f.master {
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=160000\nmedia.m3u8\n")
		} else {
			io.WriteString(w, "#EXTM3U\n#EXTINF:2,\nsegment.aac\n")
		}
	case "cf-hls-media.sndcdn.com/media.m3u8":
		io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:2,\nsegment.aac\n")
	case "cf-hls-media.sndcdn.com/segment.aac", "cf-hls-media.sndcdn.com/init.mp4", "cf-hls-media.sndcdn.com/key.bin", "i1.sndcdn.com/cover.jpg":
		if r.Header.Get("Range") != "bytes=0-1023" {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "actual bytes")
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestCheckAccessUsesCatalogCDNAndCover(t *testing.T) {
	for _, api := range []string{"web", "official"} {
		t.Run(api, func(t *testing.T) {
			f := &accessFixture{master: true}
			cfg := newAccessFixture(t, f)
			cfg.API = api
			if api == "official" {
				cfg.ClientID, cfg.ClientSecret = "client-id", "client-secret"
			}
			if err := CheckAccess(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, path := range []string{"cf-hls-media.sndcdn.com/playlist.m3u8", "cf-hls-media.sndcdn.com/media.m3u8", "cf-hls-media.sndcdn.com/segment.aac", "i1.sndcdn.com/cover.jpg"} {
				if f.requests[path] == 0 {
					t.Fatalf("probe did not verify %s", path)
				}
			}
			if api == "web" && f.requests["soundcloud.com/"] == 0 || api == "official" && f.requests["secure.soundcloud.com/oauth/token"] == 0 {
				t.Fatal("probe skipped discovery or authorization")
			}
		})
	}
}

func TestCheckAccessRejectsBlockedCatalogMediaAndCover(t *testing.T) {
	for _, broken := range []string{"/search/tracks", "/playlist.m3u8", "/segment.aac", "/cover.jpg"} {
		t.Run(broken, func(t *testing.T) {
			f := &accessFixture{broken: broken}
			cfg := newAccessFixture(t, f)
			if err := CheckAccess(t.Context(), cfg); err == nil {
				t.Fatal("blocked SoundCloud request passed access check")
			} else if strings.Contains(err.Error(), "check-secret") || strings.Contains(err.Error(), "https://") {
				t.Fatal("access check leaked URL or credentials")
			}
		})
	}
}

func TestCheckAccessRejectsPlaylistWithoutAudio(t *testing.T) {
	f := &accessFixture{missing: true}
	if err := CheckAccess(t.Context(), newAccessFixture(t, f)); err == nil {
		t.Fatal("HLS playlist without an audio segment passed access check")
	}
}

func TestCheckAccessRejectsBlockedHLSPrerequisites(t *testing.T) {
	for _, path := range []string{"/init.mp4", "/key.bin"} {
		t.Run(path, func(t *testing.T) {
			f := &accessFixture{master: true, broken: path}
			if err := CheckAccess(t.Context(), newAccessFixture(t, f)); err == nil {
				t.Fatal("access check ignored blocked HLS initialization or key")
			}
		})
	}
}

func TestCheckAccessHonorsEarlierCallerDeadline(t *testing.T) {
	f := &accessFixture{stall: true}
	cfg := newAccessFixture(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := CheckAccess(ctx, cfg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline not preserved: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("access check ignored caller deadline")
	}
}

func TestCheckAccessRejectsInvalidProxyWithoutLeakingIt(t *testing.T) {
	err := CheckAccess(t.Context(), Config{ProxyURL: "http://secret-user:secret-password@proxy.invalid:1080"})
	if err == nil || strings.Contains(fmt.Sprint(err), "secret-") || strings.Contains(fmt.Sprint(err), "proxy.invalid") {
		t.Fatal("bad proxy was accepted or exposed")
	}
}

func accessTrackCatalog(n int) string {
	var tracks []string
	for id := range n {
		tracks = append(tracks, fmt.Sprintf(`{"id":%d,"title":"music %d","policy":"ALLOW","streamable":true,"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/%d","preset":"aac_160k","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}`, id+1, id+1, id+1))
	}
	return `{"collection":[` + strings.Join(tracks, ",") + `]}`
}

func TestCheckAccessSkipsOnlyUnavailableTracksWithinBudget(t *testing.T) {
	for _, scenario := range []string{"skip-unavailable", "stop-network-error", "three-track-limit"} {
		t.Run(scenario, func(t *testing.T) {
			f := &accessFixture{catalog: accessTrackCatalog(4), unavailable: map[string]bool{"/media/1": true}}
			if scenario == "stop-network-error" {
				f.unavailable = nil
				f.broken = "/media/1"
			}
			if scenario == "three-track-limit" {
				f.unavailable["/media/2"], f.unavailable["/media/3"] = true, true
			}
			err := CheckAccess(t.Context(), newAccessFixture(t, f))
			if (err == nil) != (scenario == "skip-unavailable") {
				t.Fatalf("unexpected access result for %s: %v", scenario, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if scenario == "skip-unavailable" && f.requests["api-v2.soundcloud.com/media/2"] == 0 {
				t.Fatal("probe did not skip unavailable track")
			}
			if scenario == "stop-network-error" && f.requests["api-v2.soundcloud.com/media/2"] != 0 {
				t.Fatal("probe retried a network failure against another track")
			}
			if f.requests["api-v2.soundcloud.com/media/4"] != 0 {
				t.Fatal("probe exceeded three-track budget")
			}
		})
	}
}
