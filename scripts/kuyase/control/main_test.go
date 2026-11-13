package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"quasarbridge/internal/home"
	"quasarbridge/internal/llm"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestCoreScenariosThroughProxyAndHTTPBackends(t *testing.T) {
	upstream := httptest.NewServer(mockUpstream())
	defer upstream.Close()
	model := &llm.Client{BaseURL: upstream.URL + "/v1", Model: "kuyase-fixture"}
	engine := &bridge.Engine{LLM: model, StripPrefixes: bridge.DefaultStripPrefixes(), Home: &home.Controller{Tools: &home.MCP{URL: upstream.URL + "/mcp"}, Planner: model}}
	policy := bridge.DefaultRoutingConfig()
	policy.DefaultRoute = "yandex"
	policy.Classifier.Enabled = new(false)
	if err := engine.SetRouting(policy); err != nil {
		t.Fatal(err)
	}
	proxy := &bridge.Proxy{Upstream: "ws" + strings.TrimPrefix(upstream.URL, "http") + "/uni.ws"}
	proxy.NewSession = func(ctx context.Context, up, down bridge.Sender) bridge.Session {
		return bridge.NewNative(ctx, up, down, engine)
	}
	mux := http.NewServeMux()
	mux.Handle("/uni.ws", proxy)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_ = json.MarshalWrite(w, map[string]any{"status": "ok", "mode": "hybrid", "airplay": map[string]any{"enabled": true, "state": "stopped"}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	for _, name := range []string{"weather", "music", "timer", "llm", "text", "dialogue", "isolation", "reset", "home", "failure"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := scenario(ctx, server.URL, upstream.URL, name); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("rejects-routing-regression", func(t *testing.T) {
		// Deliberately send a Yandex weather request to the model. The E2E
		// controller must reject a healthy server with the wrong behavior.
		broken := policy
		broken.Rules = nil
		broken.DefaultRoute = "llm"
		if err := engine.SetRouting(broken); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := scenario(ctx, server.URL, upstream.URL, "weather"); err == nil {
			t.Fatal("accepted an incorrect backend route")
		}
		counts, err := readCounters(ctx, upstream.URL)
		if err != nil || counts.Unexpected == 0 {
			t.Fatalf("regression did not reach the unexpected backend: %+v, %v", counts, err)
		}
	})
}
