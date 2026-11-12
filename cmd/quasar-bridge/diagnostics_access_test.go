package main

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"quasarbridge/internal/assistant"
)

func TestDiagnosticsNetworkAccessAppliesThroughVoiceTools(t *testing.T) {
	c := configurationFixture(t, `{}`)
	tools := &assistant.Tools{Config: assistant.DefaultConfig(), Configuration: c}
	mux := http.NewServeMux()
	registerDiagnostics(mux, newDiagnosticLog(1024), func() map[string]any { return nil }, c.diagnosticsNetworkAccess)
	checkAccess := func(want int) {
		t.Helper()
		req := httptest.NewRequest("GET", "http://192.0.2.10:8080/diagnostics/logs", nil)
		req.RemoteAddr = "192.0.2.20:12345"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("network request status=%d, want %d", w.Code, want)
		}
	}
	previousStatus := http.StatusForbidden
	checkAccess(previousStatus)
	for _, enabled := range []string{"true", "false"} {
		value, err := tools.Call(t.Context(), "set_quasar_bridge_config", `{"changes":{"diagnostics.network_access":`+enabled+`}}`)
		if err != nil {
			t.Fatal(err)
		}
		result := value.(configurationResult)
		if result.RestartRequired || len(result.Pending) != 1 || result.Pending[0] != "diagnostics.network_access" {
			t.Fatalf("network access should await reload without restart: %+v", result)
		}
		checkAccess(previousStatus)
		if _, err := tools.Call(t.Context(), "apply_quasar_bridge_config", `{"action":"reload"}`); err != nil {
			t.Fatal(err)
		}
		value, err = tools.Call(t.Context(), "get_quasar_bridge_config", `{"section":"diagnostics"}`)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(value)
		var status struct {
			RestartRequired bool     `json:"restart_required"`
			Pending         []string `json:"pending"`
			Settings        []struct {
				Active bool `json:"active"`
				Saved  bool `json:"saved"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(raw, &status); err != nil {
			t.Fatal(err)
		}
		want := enabled == "true"
		if status.RestartRequired || len(status.Pending) != 0 || len(status.Settings) != 1 || status.Settings[0].Active != want || status.Settings[0].Saved != want {
			t.Fatalf("applied access not reflected in voice configuration: %s", raw)
		}
		previousStatus = http.StatusForbidden
		if want {
			previousStatus = http.StatusOK
		}
		checkAccess(previousStatus)
	}
}

func TestDiagnosticsConfigurationDefaultsAndValidation(t *testing.T) {
	cfg, err := parseConfig([]byte(`{}`), false)
	if err != nil || cfg.Diagnostics.NetworkAccess || cfg.Diagnostics.Port != 8080 {
		t.Fatalf("unsafe diagnostics default: %+v, %v", cfg.Diagnostics, err)
	}
	for _, body := range []string{
		`{"diagnostics":{"port":0}}`, `{"diagnostics":{"port":-1}}`, `{"diagnostics":{"port":65536}}`,
		`{"diagnostics":{"port":1.5}}`, `{"diagnostics":{"network_access":"true"}}`, `{"diagnostics":{"unexpected":true}}`,
		`{"diagnostics":null}`, `{"diagnostics":{"port":null}}`,
	} {
		if _, err := parseConfig([]byte(body), false); err == nil {
			t.Errorf("invalid diagnostics configuration accepted: %s", body)
		}
	}
	for _, port := range []string{"1", "65535"} {
		if _, err := parseConfig([]byte(`{"diagnostics":{"port":`+port+`}}`), false); err != nil {
			t.Errorf("valid port %s rejected: %v", port, err)
		}
	}
}

func TestDiagnosticsPortRequiresRestartAndCannotBeChangedByVoice(t *testing.T) {
	c := configurationFixture(t, `{}`)
	tools := &assistant.Tools{Config: assistant.DefaultConfig(), Configuration: c}
	if _, err := tools.Call(t.Context(), "set_quasar_bridge_config", `{"changes":{"diagnostics.port":9090}}`); err == nil {
		t.Fatal("voice can change listener port")
	}
	if err := os.WriteFile(c.path, []byte(`{"diagnostics":{"port":9090}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(t.Context(), "reload"); err == nil {
		t.Fatal("port change was falsely reported as live")
	}
	if c.active.Diagnostics.Port != 8080 {
		t.Fatal("failed reload changed active port")
	}
}

func TestDiagnosticsListenerFailureRollsBackRoutingAndAccess(t *testing.T) {
	c := configurationFixture(t, `{"routing":{"rules":[],"default_route":"yandex"}}`)
	tools := &assistant.Tools{Config: assistant.DefaultConfig(), Configuration: c}
	if _, err := tools.Call(t.Context(), "set_quasar_bridge_config", `{"changes":{"diagnostics.network_access":true,"routing.default_route":"llm"}}`); err != nil {
		t.Fatal(err)
	}
	bindError := errors.New("address already in use")
	c.applyDiagnostics = func(bool) error { return bindError }
	if _, err := c.Apply(t.Context(), "reload"); !errors.Is(err, bindError) {
		t.Fatalf("listener error lost: %v", err)
	}
	if c.diagnosticsNetworkAccess() || c.engine.Decide(t.Context(), "test").Route != "yandex" {
		t.Fatal("failed listener change partially applied configuration")
	}
	var bound bool
	c.applyDiagnostics = func(network bool) error { bound = network; return nil }
	if _, err := c.Apply(t.Context(), "reload"); err != nil {
		t.Fatal(err)
	}
	if !bound || !c.diagnosticsNetworkAccess() || c.engine.Decide(t.Context(), "test").Route != "llm" {
		t.Fatal("retry failed to apply listener and routing together")
	}
}

func TestDiagnosticsRejectsForeignOrigin(t *testing.T) {
	for _, origin := range []string{"http://attacker.example", "http://localhost:8081", "https://localhost:8080", "null"} {
		req := httptest.NewRequest("GET", "http://localhost:8080/diagnostics/logs", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		localDiagnostics(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("foreign Origin %q accepted: %d", origin, w.Code)
		}
	}
}

func TestDiagnosticsNetworkGuard(t *testing.T) {
	for _, tc := range []struct {
		name, remote, host, origin, fetchSite string
		network                               bool
		want                                  int
	}{
		{name: "local default", remote: "127.0.0.1:12345", host: "localhost:8080", want: 204},
		{name: "local IPv6", remote: "[::1]:12345", host: "[::1]:8080", want: 204},
		{name: "mapped loopback", remote: "[::ffff:127.0.0.1]:12345", host: "127.0.0.1:8080", want: 204},
		{name: "network denied by default", remote: "192.0.2.20:12345", host: "192.0.2.10:8080", want: 403},
		{name: "local peer network host denied", remote: "127.0.0.1:12345", host: "192.0.2.10:8080", want: 403},
		{name: "network enabled", remote: "192.0.2.20:12345", host: "192.0.2.10:8080", network: true, want: 204},
		{name: "network IPv6", remote: "[2001:db8::20]:12345", host: "[2001:db8::10]:8080", network: true, want: 204},
		{name: "matching Origin", remote: "192.0.2.20:12345", host: "192.0.2.10:8080", origin: "http://192.0.2.10:8080", network: true, want: 204},
		{name: "foreign Origin", remote: "192.0.2.20:12345", host: "192.0.2.10:8080", origin: "http://attacker.example", network: true, want: 403},
		{name: "cross site", remote: "192.0.2.20:12345", host: "192.0.2.10:8080", fetchSite: "cross-site", network: true, want: 403},
		{name: "DNS rebinding host", remote: "192.0.2.20:12345", host: "attacker.example:8080", network: true, want: 403},
		{name: "malformed peer", remote: "unknown", host: "192.0.2.10:8080", network: true, want: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://"+tc.host+"/diagnostics/logs", nil)
			req.RemoteAddr = tc.remote
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			w := httptest.NewRecorder()
			diagnosticsAccess(func() bool { return tc.network }, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })(w, req)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestDiagnosticsRequestsDuringNetworkReload(t *testing.T) {
	c := configurationFixture(t, `{}`)
	tools := &assistant.Tools{Config: assistant.DefaultConfig(), Configuration: c}
	handler := diagnosticsAccess(c.diagnosticsNetworkAccess, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	var requests sync.WaitGroup
	for range 4 {
		requests.Go(func() {
			for range 50 {
				req := httptest.NewRequest("GET", "http://192.0.2.10:8080/diagnostics/logs", nil)
				req.RemoteAddr = "192.0.2.20:12345"
				w := httptest.NewRecorder()
				handler(w, req)
				if w.Code != http.StatusForbidden && w.Code != http.StatusNoContent {
					t.Errorf("unexpected request status during reload: %d", w.Code)
				}
			}
		})
	}
	for _, enabled := range []string{"true", "false", "true", "false"} {
		if _, err := tools.Call(t.Context(), "set_quasar_bridge_config", `{"changes":{"diagnostics.network_access":`+enabled+`}}`); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Apply(t.Context(), "reload"); err != nil {
			t.Fatal(err)
		}
	}
	requests.Wait()
}
