package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

func musicScriptResponse(t *testing.T, r *http.Request, typ, body string) *http.Response {
	t.Helper()
	if r.Method != http.MethodGet {
		t.Fatalf("unexpected method: %s", r.Method)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {typ}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestResolveMusicRPFMExternalPlayerScript(t *testing.T) {
	var fetched []string
	fetch := func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.String())
		switch r.URL.String() {
		case "https://rpfm.ru":
			return musicScriptResponse(t, r, "text/html", `<script src="https://cdn.example/hls.js"></script><audio id="hlsAudioPlayer"></audio><script src="/bitrix/cache/js/s1/rpfm/template_29b82c77f5917de438a1e3a996bb363b/template_29b82c77f5917de438a1e3a996bb363b_v1.js?179153978419092"></script><script src="https://tracking.example/collect.js"></script>`), nil
		case "https://rpfm.ru/bitrix/cache/js/s1/rpfm/template_29b82c77f5917de438a1e3a996bb363b/template_29b82c77f5917de438a1e3a996bb363b_v1.js?179153978419092":
			return musicScriptResponse(t, r, "application/javascript", `$(document).ready(function () { var audioUrl = 'https://hls.rpfm.ru/rpfm/playlist.m3u8'; if (Hls.isSupported()) { hls.loadSource(audioUrl); } else { audio.src = audioUrl; } }); fetch('/must-not-execute', {method:'POST'});`), nil
		default:
			t.Fatalf("unexpected fetch: %s", r.URL)
			return nil, nil
		}
	}
	tools := musicTools(t, fetch)
	report, err := tools.WebReader.ResolveMusic(t.Context(), "rpfm точка ru")
	if err != nil || report.Source != "https://rpfm.ru" || len(report.Tracks) != 1 || report.Tracks[0].URL != "https://hls.rpfm.ru/rpfm/playlist.m3u8" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if !slices.Equal(fetched, []string{"https://rpfm.ru", "https://rpfm.ru/bitrix/cache/js/s1/rpfm/template_29b82c77f5917de438a1e3a996bb363b/template_29b82c77f5917de438a1e3a996bb363b_v1.js?179153978419092"}) {
		t.Fatalf("fetches=%v", fetched)
	}
	var played sound.Request
	ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(_ context.Context, r sound.Request) error { played = r; return nil })
	if _, err := tools.Call(ctx, "play_music", `{"source":"https://rpfm.ru"}`); err != nil || played.URL != "https://hls.rpfm.ru/rpfm/playlist.m3u8" {
		t.Fatalf("played=%+v err=%v", played, err)
	}
}

func TestWebReaderExtractsOnlyStaticPlayerScriptLiterals(t *testing.T) {
	for _, tt := range []struct{ name, script, want string }{
		{"audio assignment", `var audioUrl = 'https://audio.example/live.m3u8';`, "https://audio.example/live.m3u8"},
		{"stream assignment", `const stream_url = '/live.m3u8';`, "https://audio.example/live.m3u8"},
		{"literal loadSource", `hls.loadSource("https://audio.example/live.m3u8");`, "https://audio.example/live.m3u8"},
		{"comments", `// var audioUrl = 'https://audio.example/ad.mp3';` + "\n" + `/* hls.loadSource('https://audio.example/demo.mp3'); */`, ""},
		{"unrelated URLs", `var advertisement = 'https://audio.example/ad.mp3'; console.log('https://audio.example/demo.mp3');`, ""},
		{"quoted code", `var example = "var audioUrl = 'https://audio.example/demo.mp3';";`, ""},
		{"concatenation", `var audioUrl = 'https://audio.example/partial.m3u8' + '?token=dynamic';`, ""},
		{"template", "var audioUrl = `https://audio.example/${station}.m3u8`;", ""},
		{"tagged template", "var audioUrl = 'https://audio.example/partial.m3u8' `dynamic`;", ""},
		{"regex text", `var pattern = /[/]audioUrl = "https:\/\/audio.example\/demo.mp3";[/]/;`, ""},
		{"dynamic call", `hls.loadSource('/partial.m3u8' + '?token=dynamic');`, ""},
		{"private URL", `var audioUrl = 'http://127.0.0.1/private.mp3';`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := readMediaFixture(t, `<p>Player</p><audio></audio><script>`+tt.script+`</script>`, "text/html")
			if tt.want == "" {
				if len(result.Audio) != 0 {
					t.Fatalf("unexpected audio: %+v", result.Audio)
				}
			} else if len(result.Audio) != 1 || result.Audio[0].URL != tt.want {
				t.Fatalf("audio=%+v want=%s", result.Audio, tt.want)
			}
		})
	}
}

func TestResolveMusicExternalLiteralsUseDocumentBase(t *testing.T) {
	for _, tt := range []struct{ base, want string }{
		{"", "https://audio.example/pages/live.m3u8"},
		{`<base href="/radio/">`, "https://audio.example/radio/live.m3u8"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			reader := NewWebReader(DefaultConfig())
			reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/pages/player" {
					return musicScriptResponse(t, r, "text/html", tt.base+`<audio></audio><script src="/assets/player.js"></script>`), nil
				}
				if r.URL.Path != "/assets/player.js" {
					t.Fatalf("unexpected fetch: %s", r.URL)
				}
				return musicScriptResponse(t, r, "text/javascript", `var audioUrl = 'live.m3u8';`), nil
			})
			report, err := reader.ResolveMusic(t.Context(), "https://audio.example/pages/player")
			if err != nil || len(report.Tracks) != 1 || report.Tracks[0].URL != tt.want {
				t.Fatalf("report=%+v err=%v", report, err)
			}
		})
	}
}

func TestResolveMusicScriptBudgetIsSharedWithNavigation(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	var fetched []string
	reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.String())
		switch r.URL.Path {
		case "/":
			return musicScriptResponse(t, r, "text/html", `<audio></audio><script src="/one.js"></script><script src="/two.js"></script><a href="/music">Music</a>`), nil
		case "/music":
			return musicScriptResponse(t, r, "text/html", `<audio></audio><script src="/three.js"></script><script src="/four.js"></script>`), nil
		case "/four.js":
			return musicScriptResponse(t, r, "application/javascript", `var audioUrl = '/must-not-find.m3u8';`), nil
		default:
			return musicScriptResponse(t, r, "application/javascript", `import '/must-not-follow.js'; var unrelated = '/demo.mp3';`), nil
		}
	})
	report, err := reader.ResolveMusic(t.Context(), "https://audio.example/")
	if err == nil || len(report.Tracks) != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if !slices.Equal(fetched, []string{"https://audio.example/", "https://audio.example/one.js", "https://audio.example/two.js", "https://audio.example/music", "https://audio.example/three.js"}) {
		t.Fatalf("script budget: %v", fetched)
	}
}

func TestResolveMusicScriptFetchRequiresPlayerAndMissingAudio(t *testing.T) {
	for _, body := range []string{
		`<p>Text</p><script src="/player.js"></script>`,
		`<audio></audio><a href="/live.m3u8">Live</a><script src="/player.js"></script>`,
		`<p>Player</p><base href="https://cdn.example/"><audio></audio><script src="player.js"></script>`,
	} {
		t.Run(body, func(t *testing.T) {
			reader := NewWebReader(DefaultConfig())
			reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://audio.example/" {
					t.Fatalf("unexpected script fetch: %s", r.URL)
				}
				return musicScriptResponse(t, r, "text/html", body), nil
			})
			_, _ = reader.ResolveMusic(t.Context(), "https://audio.example/")
		})
	}
}

func TestResolveMusicRejectsScriptRedirectsOutsideDocumentOrigin(t *testing.T) {
	for _, tt := range []struct{ target, want string }{
		{"http://127.0.0.1/private.js?token=secret", "private"},
		{"https://tracking.example/player.js?token=secret", "page origin"},
		{"http://audio.example/player.js?token=secret", "page origin"},
		{"https://audio.example:8443/player.js?token=secret", "page origin"},
	} {
		t.Run(tt.target, func(t *testing.T) {
			reader := NewWebReader(DefaultConfig())
			reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case "https://audio.example/":
					return musicScriptResponse(t, r, "text/html", `<audio></audio><script src="/player.js"></script>`), nil
				case "https://audio.example/player.js":
					resp := musicScriptResponse(t, r, "application/javascript", "")
					resp.StatusCode = 302
					resp.Header.Set("Location", tt.target)
					return resp, nil
				default:
					t.Fatalf("unsafe redirect fetched: %s", r.URL)
					return nil, nil
				}
			})
			report, err := reader.ResolveMusic(t.Context(), "https://audio.example/")
			if report.Source != "https://audio.example/" || len(report.Tracks) != 0 || err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("report=%+v err=%v", report, err)
			}
		})
	}
}

func TestResolveMusicScriptLimits(t *testing.T) {
	for _, tt := range []struct{ typ, body, want string }{
		{"text/html", `var audioUrl = '/live.m3u8';`, "content type"},
		{"text/plain", `var audioUrl = '/live.m3u8';`, "content type"},
		{"application/javascript", `var audioUrl = '/live.m3u8';` + strings.Repeat(" ", 300), "size limit"},
	} {
		t.Run(tt.want+tt.typ, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.WebMaxBytes = 256
			reader := NewWebReader(cfg)
			reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/" {
					return musicScriptResponse(t, r, "text/html", `<audio></audio><script src="/player.js"></script>`), nil
				}
				return musicScriptResponse(t, r, tt.typ, tt.body), nil
			})
			report, err := reader.ResolveMusic(t.Context(), "https://audio.example/")
			if len(report.Tracks) != 0 || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("report=%+v err=%v", report, err)
			}
		})
	}
}

func TestResolveMusicStopsWhenPlayerScriptCancelsContext(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/":
			return musicScriptResponse(t, r, "text/html", `<audio></audio><script src="/one.js"></script><script src="/two.js"></script>`), nil
		case "/one.js":
			cancel()
			return musicScriptResponse(t, r, "application/javascript", `var audioUrl = '/live.m3u8';`), nil
		default:
			t.Fatalf("fetch after cancellation: %s", r.URL)
			return nil, nil
		}
	})
	report, err := reader.ResolveMusic(ctx, "https://audio.example/")
	if len(report.Tracks) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestWebReaderDoesNotFetchOrExposePlayerScripts(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/" {
			t.Fatalf("read_webpage fetched script: %s", r.URL)
		}
		return musicScriptResponse(t, r, "text/html", `<p>Player</p><audio></audio><script src="/player.js"></script>`), nil
	})
	page, err := reader.Read(t.Context(), "https://audio.example/")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(page)
	if err != nil || strings.Contains(string(encoded), "player.js") {
		t.Fatalf("private script metadata exposed: %s err=%v", encoded, err)
	}
	reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		return musicScriptResponse(t, r, "application/javascript", `var audioUrl = '/live.m3u8';`), nil
	})
	if _, err := reader.Read(t.Context(), "https://audio.example/player.js"); err == nil || !strings.Contains(err.Error(), "content type") {
		t.Fatalf("ordinary reader accepted JavaScript: %v", err)
	}
}
