package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"quasarbridge/internal/assistant"
	"quasarbridge/internal/home"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"quasarbridge/internal/bridge"
)

type fakeAirPlay struct{ running atomic.Bool }

func (f *fakeAirPlay) Control(_ context.Context, action string) (string, error) {
	switch action {
	case "start":
		f.running.Store(true)
		return "AirPlay включён. Выберите колонку.", nil
	case "stop":
		f.running.Store(false)
		return "AirPlay выключен.", nil
	}
	return "", fmt.Errorf("unexpected action %s", action)
}

// This verifies the controller's wire seam against the existing Proxy/Native/
// Engine; it does not claim to test the real receiver or an actual Bridge main.
func TestControllerUsesVoiceAndASRThroughExistingProxy(t *testing.T) {
	upstream := httptest.NewServer(mockUpstream())
	defer upstream.Close()
	airplay := new(fakeAirPlay)
	engine := &bridge.Engine{AirPlay: airplay}
	proxy := &bridge.Proxy{Upstream: "ws" + strings.TrimPrefix(upstream.URL, "http") + "/uni.ws"}
	proxy.NewSession = func(ctx context.Context, up, down bridge.Sender) bridge.Session {
		return bridge.NewNative(ctx, up, down, engine)
	}
	mux := http.NewServeMux()
	mux.Handle("/uni.ws", proxy)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		state := "stopped"
		if airplay.running.Load() {
			state = "running"
		}
		json.MarshalWrite(w, map[string]any{"status": "ok", "mode": "hybrid", "airplay": map[string]any{"enabled": true, "state": state, "error": ""}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	for _, action := range []string{"start", "stop"} {
		if err := command(t.Context(), server.URL, action); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
}

func TestLoopbackControlRejectsExternalHosts(t *testing.T) {
	for _, host := range []string{"203.0.113.1", "uniproxy.alice.yandex.net", "0.0.0.0"} {
		if loopback(host) == nil {
			t.Fatalf("accepted external endpoint %q", host)
		}
	}
}

type coreHarness struct {
	base, upstream string
	engine         *bridge.Engine
	policy         bridge.RoutingConfig
}

func newCoreHarness(t *testing.T, fault string) coreHarness {
	t.Helper()
	upstream := httptest.NewServer(mockUpstream())
	t.Cleanup(upstream.Close)
	model := &llm.Client{BaseURL: upstream.URL + "/v1", Model: "kuyase-fixture"}
	engine := &bridge.Engine{LLM: model, StripPrefixes: bridge.DefaultStripPrefixes(), Home: &home.Controller{Tools: &home.MCP{URL: upstream.URL + "/mcp"}, Planner: model}}
	policy := bridge.DefaultRoutingConfig()
	policy.DefaultRoute = "yandex"
	policy.Classifier.Enabled = new(false)
	if err := engine.SetRouting(policy); err != nil {
		t.Fatal(err)
	}
	logs := new(journal.Writer)
	config := journal.DefaultConfig()
	config.Enabled = true
	config.Path = filepath.Join(t.TempDir(), "conversations.jsonl")
	if err := logs.Configure(config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	proxy := &bridge.Proxy{Journal: logs, Upstream: "ws" + strings.TrimPrefix(upstream.URL, "http") + "/uni.ws"}
	proxy.NewSession = func(ctx context.Context, up, down bridge.Sender) bridge.Session {
		alter := func(sender bridge.Sender, direction wire.Direction) bridge.Sender {
			return func(ctx context.Context, kind websocket.MessageType, data []byte) error {
				frame := wire.Decode(data, direction)
				changed := false
				if speech := frame.Client.GetEvent().GetTtsGenerate(); fault == "synthesis" && speech != nil && speech.GetText() == llmURLsSpeech {
					speech.Text = new(llmURLsAnswer)
					changed = true
				}
				if response := frame.Server.GetEvent().GetAliceResponse(); response != nil && response.GetVoiceResponse().GetOutputSpeech().GetText() == llmURLsSpeech {
					if fault == "voice" {
						response.VoiceResponse.OutputSpeech.Text = new(llmURLsAnswer)
						changed = true
					} else if fault == "display" && len(response.GetResponse().GetCards()) == 1 {
						response.Response.Cards[0].Text = new(llmURLsSpeech)
						changed = true
					}
				}
				if changed {
					var err error
					data, err = frame.Marshal()
					if err != nil {
						return err
					}
				}
				return sender(ctx, kind, data)
			}
		}
		up, down = alter(up, wire.ClientToServer), alter(down, wire.ServerToClient)
		return bridge.NewNative(ctx, up, down, engine)
	}
	mux := http.NewServeMux()
	mux.Handle("/uni.ws", proxy)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_ = json.MarshalWrite(w, map[string]any{"status": "ok", "mode": "hybrid", "airplay": map[string]any{"enabled": true, "state": "stopped"}})
	})
	mux.HandleFunc("GET /diagnostics/api/history", func(w http.ResponseWriter, r *http.Request) {
		result, err := assistant.SearchHistory(r.Context(), logs.Config(), assistant.HistoryQuery{Query: r.URL.Query().Get("query"), Since: r.URL.Query().Get("since"), Limit: 20}, 1)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if fault == "history" {
			for i := range result.Items {
				if result.Items[i].Query == phrases["llm_urls"] {
					result.Items[i].Answer = llmURLsSpeech
				}
			}
		}
		_ = json.MarshalWrite(w, result)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return coreHarness{base: server.URL, upstream: upstream.URL, engine: engine, policy: policy}
}

func TestCoreScenariosThroughProxyAndHTTPBackends(t *testing.T) {
	harness := newCoreHarness(t, "")
	for _, name := range []string{"weather", "music", "timer", "llm", "llm_urls", "text", "dialogue", "isolation", "reset", "home", "failure"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := scenario(ctx, harness.base, harness.upstream, name); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("rejects-routing-regression", func(t *testing.T) {
		// Deliberately send a Yandex weather request to the model. The E2E
		// controller must reject a healthy server with the wrong behavior.
		broken := harness.policy
		broken.Rules = nil
		broken.DefaultRoute = "llm"
		if err := harness.engine.SetRouting(broken); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := scenario(ctx, harness.base, harness.upstream, "weather"); err == nil {
			t.Fatal("accepted an incorrect backend route")
		}
		counts, err := readCounters(ctx, harness.upstream)
		if err != nil || counts.Unexpected == 0 {
			t.Fatalf("regression did not reach the unexpected backend: %+v, %v", counts, err)
		}
	})
}

func TestLLMURLsScenarioRejectsBoundaryRegressions(t *testing.T) {
	for _, tc := range []struct{ fault, want string }{
		{"voice", "voice answer="},
		{"synthesis", "synthesis="},
		{"display", "display answer or request ID changed"},
		{"history", "history lost the original URL answer"},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			harness := newCoreHarness(t, tc.fault)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := scenario(ctx, harness.base, harness.upstream, "llm_urls"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("boundary regression was not rejected at %s: %v", tc.fault, err)
			}
		})
	}
}
