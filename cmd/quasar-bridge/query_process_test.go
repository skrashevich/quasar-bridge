package main

import (
	"encoding/json/v2"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/sound"
)

// Removing the CLI speaker/player context or its diagnostic queue breaks this flow.
func TestQueryPlaybackProcess(t *testing.T) {
	if path := os.Getenv("QUASAR_TEST_QUERY_CONFIG"); path != "" {
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", path, "-query", os.Getenv("QUASAR_TEST_QUERY_TEXT")}
		main()
		os.Exit(0)
	}
	var calls atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected backend request", http.StatusInternalServerError)
	}))
	defer api.Close()
	for _, tc := range []struct {
		name, listen, tls, text, wantURL string
		queue                            bool
	}{
		{name: "direct HLS", listen: "localhost:8443", text: "Включи радио https://hls.rpfm.ru/rpfm/aac_hifi.m3u8", wantURL: "https://hls.rpfm.ru/rpfm/aac_hifi.m3u8"},
		{name: "direct HLS without queue origin", listen: ":8443", tls: `,"tls_cert":"unused-cert","tls_key":"unused-key"`, text: "Включи радио https://hls.rpfm.ru/rpfm/aac_hifi.m3u8", wantURL: "https://hls.rpfm.ru/rpfm/aac_hifi.m3u8"},
		{name: "MP3 without queue origin", listen: ":8443", text: "Включи музыку https://audio.example/track.mp3", queue: true},
		{name: "plain query", listen: "localhost:8443", text: "Какая погода"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := configFile(t, fmt.Sprintf(`{"mode":"hybrid","airplay":{"disabled":true},"listen":%q%s,"llm_base_url":%q,"llm_model":"fixture","assistant_tools":{"enabled":true,"web":true,"play_sound":true},"routing":{"rules":[{"name":"music","route":"llm","match":{"starts_with_any":["включи"]}}],"default_route":"yandex","classifier":{"enabled":false}}}`, tc.listen, tc.tls, api.URL))
			stdout, stderr, err := runQueryProcess(t, path, tc.text)
			if err != nil {
				t.Fatalf("query failed: %v stdout=%s stderr=%s", err, stdout, stderr)
			}
			var report struct {
				Route    string
				Answer   string
				Playback []struct {
					sound.Request
					Tracks []assistant.WebLink `json:"tracks"`
				} `json:"playback"`
				PlaybackSimulated bool `json:"playback_simulated"`
			}
			if err := json.Unmarshal([]byte(stdout), &report); err != nil {
				t.Fatalf("missing query JSON: %v stdout=%s stderr=%s", err, stdout, stderr)
			}
			if tc.wantURL == "" && !tc.queue {
				if report.Route != "yandex" || strings.Contains(stdout, "playback") {
					t.Fatalf("plain query acquired simulated playback: %s", stdout)
				}
				return
			}
			if report.Route != "music" || !report.PlaybackSimulated || len(report.Playback) != 1 {
				t.Fatalf("missing simulated music request: %s", stdout)
			}
			played := report.Playback[0]
			if played.StreamID == "" || played.Sound != "" || played.Command != "" {
				t.Fatalf("incorrect music request: %+v", played)
			}
			if tc.queue {
				u, err := url.Parse(played.URL)
				if err != nil || !strings.HasSuffix(u.Hostname(), ".invalid") || !strings.HasPrefix(u.Path, "/music-stream/") || !strings.HasSuffix(u.Path, ".mp3") {
					t.Fatalf("missing diagnostic queue URL: %s", played.URL)
				}
				if len(played.Tracks) != 1 || played.Tracks[0].URL != "https://audio.example/track.mp3" {
					t.Fatalf("missing original MP3 source: %+v", played.Tracks)
				}
			} else if played.URL != tc.wantURL {
				t.Fatalf("URL=%q want=%q", played.URL, tc.wantURL)
			} else if len(played.Tracks) != 1 || played.Tracks[0].URL != tc.wantURL {
				t.Fatalf("missing original HLS source: %+v", played.Tracks)
			}
			if strings.Contains(stderr, tc.text) {
				t.Fatalf("query transcript leaked to logs: %s", stderr)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("direct music or Yandex diagnostics contacted the backend: %d requests", calls.Load())
	}
}

func TestQuerySoundProcess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		fail    bool
	}{
		{name: "simulated signal", enabled: true},
		{name: "disabled tool stays disabled"},
		{name: "LLM fails after capture", enabled: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []llm.Message `json:"messages"`
					Tools    []llm.Tool    `json:"tools"`
				}
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				found := false
				for _, tool := range request.Tools {
					found = found || tool.Function.Name == "play_sound"
				}
				if found != tc.enabled {
					t.Errorf("play_sound advertised=%t want=%t", found, tc.enabled)
				}
				step := calls.Add(1)
				message := llm.Message{Role: "assistant", Content: "Сигнал подготовлен."}
				if tc.enabled && step == 1 {
					message.Content = ""
					message.ToolCalls = []llm.ToolCall{{ID: "signal", Type: "function", Function: llm.FunctionCall{Name: "play_sound", Arguments: `{"source":"notification"}`}}}
				} else if tc.enabled {
					last := request.Messages[len(request.Messages)-1]
					if last.Role != "tool" || !strings.Contains(last.Content, `"status":"queued"`) {
						t.Errorf("signal was not queued: %+v", last)
					}
					if tc.fail {
						http.Error(w, "fixture failure", http.StatusInternalServerError)
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": message}}})
			}))
			defer api.Close()
			path := configFile(t, fmt.Sprintf(`{"mode":"hybrid","airplay":{"disabled":true},"llm_base_url":%q,"llm_model":"fixture","assistant_tools":{"enabled":true,"play_sound":%t,"time":false},"routing":{"rules":[],"default_route":"llm","classifier":{"enabled":false}}}`, api.URL, tc.enabled))
			stdout, stderr, err := runQueryProcess(t, path, "Подай короткий сигнал")
			if tc.fail {
				if err == nil || stdout != "" || calls.Load() != 2 {
					t.Fatalf("failed query reported playback: error=%v calls=%d stdout=%s stderr=%s", err, calls.Load(), stdout, stderr)
				}
				return
			}
			if err != nil {
				t.Fatalf("signal query failed: %v stdout=%s stderr=%s", err, stdout, stderr)
			}
			var report struct {
				Route             string
				Answer            string
				Playback          []queryPlayback `json:"playback"`
				PlaybackSimulated bool            `json:"playback_simulated"`
			}
			if err := json.Unmarshal([]byte(stdout), &report); err != nil || report.Route != "llm" || report.Answer != "Сигнал подготовлен." {
				t.Fatalf("signal report=%s error=%v", stdout, err)
			}
			if tc.enabled {
				if calls.Load() != 2 || !report.PlaybackSimulated || len(report.Playback) != 1 || report.Playback[0].Request != (sound.Request{Sound: "notification"}) || strings.Contains(stdout, "tracks") {
					t.Fatalf("missing simulated signal: calls=%d report=%s", calls.Load(), stdout)
				}
			} else if calls.Load() != 1 || strings.Contains(stdout, "playback") {
				t.Fatalf("disabled sound acquired playback: calls=%d report=%s", calls.Load(), stdout)
			}
		})
	}
}

func runQueryProcess(t *testing.T, configPath, text string) (string, string, error) {
	t.Helper()
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestQueryPlaybackProcess$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "QUASAR_") {
			child.Env = append(child.Env, entry)
		}
	}
	child.Env = append(child.Env, "QUASAR_TEST_QUERY_CONFIG="+configPath, "QUASAR_TEST_QUERY_TEXT="+text)
	var stdout, stderr strings.Builder
	child.Stdout, child.Stderr = &stdout, &stderr
	err := child.Run()
	return stdout.String(), stderr.String(), err
}
