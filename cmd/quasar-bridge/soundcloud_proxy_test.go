package main

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"quasarbridge/internal/dialogue"
)

func TestSoundCloudProxyEnvironmentAndRedaction(t *testing.T) {
	const key = "QUASAR_SOUNDCLOUD_PROXY_URL"
	body := soundCloudHybridJSON(t, `{"soundcloud":{"enabled":true,"proxy_url":"socks5://file.example.invalid:1080"}}`)
	t.Setenv(key, "socks5h://env-user:env-password@proxy.example.invalid:1081")
	cfg, err := parseConfig(body, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cfg.SoundCloud)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["proxy_url"] != "socks5h://env-user:env-password@proxy.example.invalid:1081" {
		t.Fatal("proxy environment did not override JSON")
	}
	health, err := json.Marshal(soundCloudHealth(cfg, true))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(health), "env-user") || strings.Contains(string(health), "env-password") || strings.Contains(string(health), "proxy.example.invalid") {
		t.Fatal("health exposed proxy credentials or address")
	}
	t.Setenv(key, "")
	cfg, err = parseConfig(body, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(cfg.SoundCloud)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "proxy_url") {
		t.Fatal("empty proxy environment did not clear JSON override")
	}
	t.Setenv(key, "http://secret-user:secret-password@proxy.example.invalid:1080")
	_, err = parseConfig(body, false)
	if err == nil || strings.Contains(err.Error(), "secret-") || strings.Contains(err.Error(), "proxy.example.invalid") {
		t.Fatal("invalid proxy accepted or credentials exposed")
	}
}

// This SOCKS server rejects every CONNECT after recording its destination. It
// proves routing without contacting SoundCloud or requiring external DNS.
func rejectingSoundCloudProxy(t *testing.T) (string, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	targets := make(chan string, 8)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Error(err)
					return
				}
				var greeting [2]byte
				if _, err := io.ReadFull(conn, greeting[:]); err != nil {
					t.Error(err)
					return
				}
				methods := make([]byte, int(greeting[1]))
				if _, err := io.ReadFull(conn, methods); err != nil {
					t.Error(err)
					return
				}
				if _, err := conn.Write([]byte{5, 0}); err != nil {
					t.Error(err)
					return
				}
				var request [5]byte
				if _, err := io.ReadFull(conn, request[:]); err != nil {
					t.Error(err)
					return
				}
				if request[0] != 5 || request[1] != 1 || request[3] != 3 {
					t.Error("SOCKS CONNECT did not carry a destination hostname")
					return
				}
				destination := make([]byte, int(request[4])+2)
				if _, err := io.ReadFull(conn, destination); err != nil {
					t.Error(err)
					return
				}
				if binary.BigEndian.Uint16(destination[len(destination)-2:]) != 443 {
					t.Error("unexpected destination port")
				}
				targets <- string(destination[:len(destination)-2])
				_, _ = conn.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
			})
		}
	})
	t.Cleanup(func() { listener.Close(); wg.Wait() })
	return "socks5://" + listener.Addr().String(), targets
}

func TestSoundCloudProviderRoutesCatalogMediaAndArtworkThroughProxy(t *testing.T) {
	// Any route that bypasses the explicit SOCKS setting fails locally. The
	// shared transport itself must remain unchanged for other integrations.
	original := http.DefaultTransport
	base := original.(*http.Transport).Clone()
	base.Proxy = func(*http.Request) (*url.URL, error) { return nil, errors.New("direct route disabled by test") }
	http.DefaultTransport = base
	t.Cleanup(func() { http.DefaultTransport = original; base.CloseIdleConnections() })
	proxyURL, targets := rejectingSoundCloudProxy(t)
	body, err := json.Marshal(map[string]any{"soundcloud": map[string]any{"enabled": true, "api": "web", "client_id": "fixture", "proxy_url": proxyURL}})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfig(soundCloudHybridJSON(t, string(body)), false)
	if err != nil {
		t.Fatal(err)
	}
	provider, relay, err := newSoundCloudProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertTarget := func(want string) {
		t.Helper()
		select {
		case got := <-targets:
			if got != want {
				t.Fatalf("SOCKS target = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s bypassed the SoundCloud proxy", want)
		}
	}
	reply, err := provider.Handle(dialogue.WithSpeaker(t.Context(), "proxy-speaker"), "найди test SoundCloud")
	if err != nil || !strings.HasPrefix(reply, "Не удалось") {
		t.Fatalf("proxy refusal did not produce a SoundCloud error: %q, %v", reply, err)
	}
	assertTarget("api-v2.soundcloud.com")
	media, err := relay.Register("https://i1.sndcdn.com/art.jpg", func(context.Context) (string, error) {
		return "https://cf-hls-media.sndcdn.com/track.m3u8", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	for _, raw := range []string{media.URL, media.ArtworkURL} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().Get(server.URL + u.RequestURI())
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("proxy refusal returned HTTP %d, want 502", response.StatusCode)
		}
	}
	assertTarget("cf-hls-media.sndcdn.com")
	assertTarget("i1.sndcdn.com")
	if _, err := base.Proxy(new(http.Request)); err == nil {
		t.Fatal("SoundCloud proxy mutated the shared default transport")
	}
}
