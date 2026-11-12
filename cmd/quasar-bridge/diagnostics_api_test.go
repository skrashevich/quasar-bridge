package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	airplay "github.com/skrashevich/station-airplay"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/llm"
)

type diagnosticCompleter func(context.Context, []llm.Message, bool) (string, error)

func (f diagnosticCompleter) Complete(ctx context.Context, messages []llm.Message, structured bool) (string, error) {
	return f(ctx, messages, structured)
}

func diagnosticAPIRequest(mux *http.ServeMux, method, path, body, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost:18443/diagnostics/api/"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestDiagnosticAPIValidatesBeforeSavingAndKeepsApplySeparate(t *testing.T) {
	c := configurationFixture(t, `{"llm_model":"before","llm_base_url":"https://private.example/v1?token=private-secret","airplay":{"disabled":true}}`)
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Configuration: c})
	for _, tc := range []struct{ body, contentType string }{
		{`{"changes":{"llm_model":"invalid"}}`, "text/plain"},
		{`{"changes":{"llm_model":"invalid"},"command":"sh"}`, "application/json"},
		{`{"changes":{"llm_model":"invalid"}} {}`, "application/json"},
		{`{"changes":{"tls_key":"private-secret"}}`, "application/json"},
		{`{"changes":{"llm_model":"` + strings.Repeat("x", 256<<10) + `"}}`, "application/json"},
	} {
		response := diagnosticAPIRequest(mux, "POST", "config", tc.body, tc.contentType)
		if response.Code < 400 || response.Header().Get("Content-Type") != "application/json" || !strings.Contains(response.Body.String(), `"error"`) {
			t.Fatalf("invalid save accepted: status=%d body=%s", response.Code, response.Body)
		}
	}
	raw, err := os.ReadFile(c.path)
	if err != nil || !strings.Contains(string(raw), `"llm_model":"before"`) {
		t.Fatalf("invalid requests changed configuration: %s %v", raw, err)
	}
	response := diagnosticAPIRequest(mux, "GET", "config", "", "")
	if response.Code != 200 || strings.Contains(response.Body.String(), "private-secret") || strings.Contains(response.Body.String(), "private.example") {
		t.Fatalf("safe catalog failed: status=%d body=%s", response.Code, response.Body)
	}
	response = diagnosticAPIRequest(mux, "POST", "config", `{"changes":{"routing.default_route":"llm"}}`, "application/json; charset=utf-8")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"status":"saved"`) {
		t.Fatalf("save failed: status=%d body=%s", response.Code, response.Body)
	}
	if c.engine.RoutingStatus().DefaultRoute != "yandex" {
		t.Fatal("saving settings applied them without explicit reload")
	}
	response = diagnosticAPIRequest(mux, "POST", "apply", `{"action":"reload"}`, "application/json")
	if response.Code != 200 || c.engine.RoutingStatus().DefaultRoute != "llm" {
		t.Fatalf("reload failed: status=%d body=%s", response.Code, response.Body)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("management response may be cached")
	}
}

func TestDiagnosticAPIPreviewDoesNotExecuteAndQueryIsBounded(t *testing.T) {
	c := configurationFixture(t, `{"mode":"hybrid","llm_base_url":"http://127.0.0.1:1/v1","llm_model":"fixture","airplay":{"disabled":true},"routing":{"rules":[],"default_route":"llm"}}`)
	started, finish := make(chan struct{}), make(chan struct{})
	c.engine.LLM = diagnosticCompleter(func(ctx context.Context, messages []llm.Message, structured bool) (string, error) {
		if messages[len(messages)-1].Content != "Проверка" || structured {
			t.Errorf("unexpected diagnostic request: %+v structured=%t", messages, structured)
		}
		close(started)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-finish:
			return "Связь есть.", nil
		}
	})
	c.engine.Dialogue = dialogue.New(dialogue.DefaultConfig())
	mux := http.NewServeMux()
	control := diagnosticControl{Configuration: c, Engine: c.engine, Status: func() map[string]any { return map[string]any{"mode": "hybrid"} }, QueryBusy: new(atomic.Bool)}
	registerDiagnosticsAPI(mux, control)
	otherListener := http.NewServeMux()
	registerDiagnosticsAPI(otherListener, control)
	for _, body := range []string{`{"text":" "}`, `{"text":"` + strings.Repeat("я", 8001) + `"}`, `{"text":"Проверка","command":"sh"}`} {
		if response := diagnosticAPIRequest(mux, "POST", "query", body, "application/json"); response.Code != 400 {
			t.Fatalf("invalid query accepted: status=%d body=%s", response.Code, response.Body)
		}
	}
	response := diagnosticAPIRequest(mux, "POST", "route", `{"text":"Проверка"}`, "application/json")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"classifier_called":false`) {
		t.Fatalf("static preview failed: status=%d body=%s", response.Code, response.Body)
	}
	select {
	case <-started:
		t.Fatal("static preview contacted backend")
	default:
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- diagnosticAPIRequest(mux, "POST", "query", `{"text":"Проверка"}`, "application/json")
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("query did not reach backend")
	}
	response = diagnosticAPIRequest(otherListener, "POST", "query", `{"text":"Второй"}`, "application/json")
	if response.Code != http.StatusConflict {
		t.Fatalf("parallel query accepted: status=%d body=%s", response.Code, response.Body)
	}
	close(finish)
	response = <-done
	var result struct {
		Route  string `json:"route"`
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 || result.Route != "llm" || result.Answer != "Связь есть." {
		t.Fatalf("query failed: status=%d body=%s error=%v", response.Code, response.Body, err)
	}
	if len(c.engine.Dialogue.View(t.Context()).Messages) != 0 {
		t.Fatal("dashboard query contaminated unnamed speaker conversation")
	}
}

func TestDiagnosticAPIHistoryUsesJournalAndValidatesLimits(t *testing.T) {
	logs := new(journal.Writer)
	if err := logs.Configure(journal.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "conversation.jsonl"), MaxSizeMB: 1, MaxBackups: 1, MaxTextChars: 1000}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	logs.Record(journal.Event{Session: "speaker", Request: "one", Event: "request", Text: "Какая погода?"})
	logs.Record(journal.Event{Session: "speaker", Request: "one", Event: "response", Text: "Солнечно.", Route: "llm", Delivered: true})
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Logs: logs})
	response := diagnosticAPIRequest(mux, "GET", "history?query=погода&limit=20", "", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Солнечно.") || !strings.Contains(response.Body.String(), `"recording":true`) {
		t.Fatalf("history failed: status=%d body=%s", response.Code, response.Body)
	}
	for _, path := range []string{"history?limit=0", "history?limit=21", "history?limit=invalid", "history?since=invalid"} {
		if response := diagnosticAPIRequest(mux, "GET", path, "", ""); response.Code != 400 {
			t.Errorf("invalid history accepted: %s status=%d", path, response.Code)
		}
	}
}

func TestDiagnosticAPICapabilitiesAndAirPlayUseRealControl(t *testing.T) {
	r := &fakeAirPlayReceiver{state: airplay.StateStopped}
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{AirPlay: &airPlayControl{receiver: r, app: t.Context()}, Engine: new(bridge.Engine), Status: func() map[string]any { return map[string]any{"mode": "observe", "client_frames": 7} }})
	response := diagnosticAPIRequest(mux, "GET", "status", "", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"query":false`) || !strings.Contains(response.Body.String(), `"client_frames":7`) {
		t.Fatalf("wrong capabilities/status: %s", response.Body)
	}
	if response := diagnosticAPIRequest(mux, "POST", "query", `{"text":"Проверка"}`, "application/json"); response.Code != 503 {
		t.Fatalf("observe query executed: status=%d body=%s", response.Code, response.Body)
	}
	response = diagnosticAPIRequest(mux, "POST", "airplay", `{"action":"start"}`, "application/json")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"state":"running"`) {
		t.Fatalf("start did not change reported state: status=%d body=%s", response.Code, response.Body)
	}
	response = diagnosticAPIRequest(mux, "POST", "airplay", `{"action":"invalid"}`, "application/json")
	if response.Code != 400 || r.state != airplay.StateRunning {
		t.Fatalf("invalid action changed receiver: status=%d body=%s", response.Code, response.Body)
	}
}
