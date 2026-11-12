package assistant

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/sound"
)

func musicTools(t *testing.T, fetch func(*http.Request) (*http.Response, error)) *Tools {
	t.Helper()
	tools := soundTools(t)
	tools.WebReader = NewWebReader(tools.Config)
	tools.WebReader.http.Transport = roundTripper(fetch)
	queue, err := NewMusicQueue("http://127.0.0.1:8443", tools.WebReader)
	if err != nil {
		t.Fatal(err)
	}
	tools.Playback = NewMusicPlayback(queue)
	return tools
}

func TestPlayMusicDiscoversNavigationAndQueuesTracks(t *testing.T) {
	var fetched []string
	tools := musicTools(t, func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.String())
		body := `<a href="/catalog">Музыка</a><a href="https://unrelated.example/music">Music</a>`
		if r.URL.Path == "/catalog" {
			body = `<script type="application/ld+json">[{"name":"First","contentUrl":"/songs/Первый.mp3"},{"name":"Second","contentUrl":"/songs/second.mp3"}]</script>`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	var played sound.Request
	ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "test-speaker"), func(_ context.Context, req sound.Request) error { played = req; return nil })
	result, err := tools.Call(ctx, "play_music", `{"source":"audio.example"}`)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	var report struct {
		Status     string `json:"status"`
		TrackCount int    `json:"track_count"`
	}
	if err := json.Unmarshal(data, &report); err != nil || report.Status != "queued" || report.TrackCount != 2 {
		t.Fatalf("report=%s err=%v", data, err)
	}
	if len(fetched) != 2 || fetched[1] != "https://audio.example/catalog" {
		t.Fatalf("navigation: %v", fetched)
	}
	if !strings.HasPrefix(played.URL, "http://127.0.0.1:8443/music-stream/") {
		t.Fatalf("not a stream: %+v", played)
	}
	tools.Playback.queue.mu.Lock()
	defer tools.Playback.queue.mu.Unlock()
	for _, entry := range tools.Playback.queue.entries {
		if len(entry.tracks) != 2 || entry.tracks[0].URL != "https://audio.example/songs/%D0%9F%D0%B5%D1%80%D0%B2%D1%8B%D0%B9.mp3" {
			t.Fatalf("tracks=%+v", entry.tracks)
		}
	}
}

func TestPlayMusicRequiresSpeakerBeforeFetching(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) { t.Fatal("fetch without current speaker"); return nil, nil })
	if _, err := tools.Call(t.Context(), "play_music", `{"source":"https://audio.example/music"}`); err == nil || !strings.Contains(err.Error(), "current speaker") {
		t.Fatalf("err=%v", err)
	}
}

func TestPlayMusicRPFMAnchorHandsHLSToCurrentSpeaker(t *testing.T) {
	tools := musicTools(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://rpfm.ru" {
			t.Fatalf("fetch=%s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(`<a href="https://hls.rpfm.ru/rpfm/aac_hifi.m3u8">...</a>`)), Request: r}, nil
	})
	var played sound.Request
	ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "test-speaker"), func(_ context.Context, req sound.Request) error { played = req; return nil })
	if _, err := tools.Call(ctx, "play_music", `{"source":"rpfm точка ru"}`); err != nil || played.URL != "https://hls.rpfm.ru/rpfm/aac_hifi.m3u8" {
		t.Fatalf("played=%+v err=%v", played, err)
	}
}

func TestPlayMusicDirectAndInvalidSources(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected fetch"); return nil, nil })
	var played sound.Request
	ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "test-speaker"), func(_ context.Context, r sound.Request) error { played = r; return nil })
	if _, err := tools.Call(ctx, "play_music", `{"source":"https://audio.example/Live.m3u8?token=a"}`); err != nil || played.URL != "https://audio.example/Live.m3u8?token=a" {
		t.Fatalf("play=%+v err=%v", played, err)
	}
	for _, args := range []string{`null`, `{}`, `{"source":null}`, `{"source":""}`, `{"source":"notification"}`, `{"source":"http://127.0.0.1/private"}`, `{"source":"https://u:p@audio.example/music"}`, `{"source":"https://audio.example/music","extra":true}`} {
		if _, err := tools.Call(ctx, "play_music", args); err == nil {
			t.Fatalf("accepted %s", args)
		}
	}
}

func TestMusicPrefetchDoesNotReadPageTwice(t *testing.T) {
	tools := soundTools(t)
	calls := tools.InitialCalls("Включи музыку с сайта audio.example/music")
	if len(calls) != 1 || calls[0].Name != "current_time" {
		t.Fatalf("calls=%+v", calls)
	}
	calls = tools.InitialCalls("Прочитай сайт аудио точка ком")
	if len(calls) != 2 || !strings.Contains(calls[1].Arguments, "https://audio.com") {
		t.Fatalf("normalized webpage calls=%+v", calls)
	}
}

func TestPrivateHistoryBlocksMusic(t *testing.T) {
	for _, withMCP := range []bool{false, true} {
		t.Run(map[bool]string{false: "builtin", true: "mcp"}[withMCP], func(t *testing.T) {
			tools := musicTools(t, func(*http.Request) (*http.Response, error) { t.Fatal("private fetch"); return nil, nil })
			tools.Config.Time = false
			if withMCP {
				tools.Config.MCPServers = []MCPServerConfig{{Name: "disabled", Disabled: true}}
			}
			ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "test-speaker"), func(context.Context, sound.Request) error { t.Fatal("private playback"); return nil })
			round := 0
			a := Agent{Tools: tools, MaxCalls: 2, MaxRounds: 2, Client: chatFunc(func(_ context.Context, _ []llm.Message, defs []llm.Tool) (llm.Message, error) {
				round++
				for _, d := range defs {
					if d.Function.Name == "play_music" {
						t.Error("offered after private history")
					}
				}
				if round == 1 {
					return llm.Message{ToolCalls: []llm.ToolCall{{ID: "music", Type: "function", Function: llm.FunctionCall{Name: "play_music", Arguments: `{"source":"https://audio.example/?history=private"}`}}}}, nil
				}
				return llm.Message{Content: "Запрос отклонён."}, nil
			})}
			if _, _, err := a.CompleteDialogue(ctx, []llm.Message{{Role: "user", Content: "Включи музыку"}}, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}
