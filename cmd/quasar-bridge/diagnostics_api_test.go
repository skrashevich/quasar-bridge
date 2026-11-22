package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	airplay "github.com/skrashevich/station-airplay"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/yandex"
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
	for _, endpoint := range []string{"query", "decide"} {
		response = diagnosticAPIRequest(otherListener, "POST", endpoint, `{"text":"Второй"}`, "application/json")
		if response.Code != http.StatusConflict {
			t.Fatalf("parallel %s accepted: status=%d body=%s", endpoint, response.Code, response.Body)
		}
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

func TestDiagnosticAPIYandexReturnsAnswerOrError(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			engine := new(bridge.Engine)
			mux := http.NewServeMux()
			registerDiagnosticsAPI(mux, diagnosticControl{Engine: engine, YandexQuery: func(ctx context.Context, text string, _ *queryDialog) (string, error) {
				if text != "который час" {
					t.Errorf("original query changed: %q", text)
				}
				if fail {
					return "", errors.New("private-provider-detail")
				}
				return "В Москве 5:46.", nil
			}})
			r := diagnosticAPIRequest(mux, "POST", "query", `{"text":"который час"}`, "application/json")
			if fail {
				if r.Code != http.StatusBadGateway || strings.Contains(r.Body.String(), "private-provider-detail") {
					t.Fatalf("unsafe Yandex failure: %d %s", r.Code, r.Body)
				}
				return
			}
			var result struct {
				Route  string `json:"route"`
				Answer string `json:"answer"`
			}
			if json.Unmarshal(r.Body.Bytes(), &result) != nil || r.Code != http.StatusOK || result.Route != "yandex" || result.Answer != "В Москве 5:46." {
				t.Fatalf("Yandex answer missing: %d %s", r.Code, r.Body)
			}
		})
	}
}

func TestDiagnosticAPIResumesIndependentDialogs(t *testing.T) {
	c := configurationFixture(t, `{"mode":"hybrid","llm_base_url":"http://127.0.0.1:1/v1","llm_model":"fixture","airplay":{"disabled":true},"routing":{"rules":[],"default_route":"llm"}}`)
	c.engine.LLM = diagnosticCompleter(func(ctx context.Context, messages []llm.Message, structured bool) (string, error) {
		last := messages[len(messages)-1].Content
		seen := false
		for _, m := range messages[:len(messages)-1] {
			seen = seen || m.Role == "user" && m.Content == "Запомни число 47"
		}
		if last == "Какое число?" && !seen {
			t.Error("same-dialog follow-up lost history")
		}
		if last == "Отдельный разговор" && seen {
			t.Error("history crossed dialog boundaries")
		}
		return "Ответ", nil
	})
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Engine: c.engine})
	first := diagnosticAPIRequest(mux, "POST", "query", `{"text":"Запомни число 47"}`, "application/json")
	var result struct {
		ID string `json:"dialog_id"`
	}
	if json.Unmarshal(first.Body.Bytes(), &result) != nil || first.Code != 200 || result.ID == "" {
		t.Fatalf("first query did not return dialog_id: %d %s", first.Code, first.Body)
	}
	body, _ := json.Marshal(map[string]string{"text": "Какое число?", "dialog_id": result.ID})
	second := diagnosticAPIRequest(mux, "POST", "query", string(body), "application/json")
	var resumed struct {
		ID string `json:"dialog_id"`
	}
	if json.Unmarshal(second.Body.Bytes(), &resumed) != nil || second.Code != 200 || resumed.ID != result.ID {
		t.Fatalf("dialog continuation failed: %d %s", second.Code, second.Body)
	}
	third := diagnosticAPIRequest(mux, "POST", "query", `{"text":"Отдельный разговор"}`, "application/json")
	var isolated struct {
		ID string `json:"dialog_id"`
	}
	if json.Unmarshal(third.Body.Bytes(), &isolated) != nil || third.Code != 200 || isolated.ID == "" || isolated.ID == result.ID {
		t.Fatalf("new conversation was not isolated: %d %s", third.Code, third.Body)
	}
	missing := diagnosticAPIRequest(mux, "POST", "query", `{"text":"Продолжи","dialog_id":"00000000-0000-4000-8000-000000000000"}`, "application/json")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown dialog silently started over: %d %s", missing.Code, missing.Body)
	}
}

func TestDiagnosticAPIExplicitResetClearsYandexContext(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Engine: new(bridge.Engine), YandexQuery: func(_ context.Context, _ string, dialog *queryDialog) (string, error) {
		calls++
		if dialog.yandex != nil {
			t.Error("explicit dialogue reset kept old Yandex client/session")
		}
		dialog.yandex = new(yandex.Client)
		return "Тестовый ответ", nil
	}})
	ask := func(text, id string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"text": text, "dialog_id": id})
		response := diagnosticAPIRequest(mux, "POST", "query", string(body), "application/json")
		var result struct {
			ID string `json:"dialog_id"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatalf("query failed: %d %s", response.Code, response.Body)
		}
		return result.ID
	}
	id := ask("который час", "")
	if got := ask("начни новый разговор", id); got != id {
		t.Fatal("reset changed local dialog handle")
	}
	if calls != 1 {
		t.Fatal("reset reached Yandex backend")
	}
	ask("который час", id)
	if calls != 2 {
		t.Fatal("missing Yandex turn")
	}
}

func TestDiagnosticAPIDecideUsesClassifierWithoutExecutingQuery(t *testing.T) {
	c := configurationFixture(t, `{"mode":"observe","airplay":{"disabled":true},"routing":{"rules":[],"default_route":"classify","classifier":{"on_error":"yandex","timeout_ms":60000}}}`)
	started, finish := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(finish) })
	defer release()
	c.engine.LLM = diagnosticCompleter(func(ctx context.Context, messages []llm.Message, structured bool) (string, error) {
		if !structured || messages[len(messages)-1].Content != "Куда направить запрос?" {
			t.Errorf("live route generated an answer instead of classifying: messages=%+v structured=%t", messages, structured)
		}
		if dialogue.SpeakerKey(ctx) == "" {
			t.Error("live route used unnamed speaker context")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 75*time.Second {
			t.Error("live route has no bounded backend context")
		}
		close(started)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-finish:
			return `{"route":"llm"}`, nil
		}
	})
	control := diagnosticControl{Configuration: c, Engine: c.engine, Status: func() map[string]any { return map[string]any{"mode": "observe"} }, QueryBusy: new(atomic.Bool)}
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, control)
	if response := diagnosticAPIRequest(mux, "POST", "decide", `{"text":"Куда направить запрос?","execute":true}`, "application/json"); response.Code != 400 {
		t.Fatalf("invalid live route accepted: status=%d body=%s", response.Code, response.Body)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- diagnosticAPIRequest(mux, "POST", "decide", `{"text":"Куда направить запрос?"}`, "application/json")
	}()
	select {
	case <-started:
	case response := <-done:
		t.Fatalf("live route did not classify: status=%d body=%s", response.Code, response.Body)
	case <-time.After(5 * time.Second):
		t.Fatal("live route did not reach classifier")
	}
	otherListener := http.NewServeMux()
	registerDiagnosticsAPI(otherListener, control)
	for _, endpoint := range []string{"query", "decide"} {
		if response := diagnosticAPIRequest(otherListener, "POST", endpoint, `{"text":"Второй"}`, "application/json"); response.Code != http.StatusConflict {
			t.Fatalf("parallel %s accepted while deciding: status=%d body=%s", endpoint, response.Code, response.Body)
		}
	}
	release()
	response := <-done
	var result struct {
		Route     string `json:"route"`
		Source    string `json:"source"`
		ElapsedMS int64  `json:"elapsed_ms"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 || result.Route != "llm" || result.Source != "classifier" || result.ElapsedMS < 0 {
		t.Fatalf("live route failed: status=%d body=%s error=%v", response.Code, response.Body, err)
	}
	if strings.Contains(response.Body.String(), `"answer"`) {
		t.Fatal("live route returned a generated answer")
	}

	r := &fakeAirPlayReceiver{state: airplay.StateStopped}
	c.engine.AirPlay = &airPlayControl{receiver: r, app: t.Context()}
	response = diagnosticAPIRequest(mux, "POST", "decide", `{"text":"Включи AirPlay"}`, "application/json")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"source":"airplay"`) || r.state != airplay.StateStopped || r.starts != 0 {
		t.Fatalf("special live route executed action or was not recognized: status=%d body=%s receiver=%+v", response.Code, response.Body, r)
	}
}

func TestDiagnosticAPIDecidePreservesClassifierFallback(t *testing.T) {
	c := configurationFixture(t, `{"airplay":{"disabled":true},"routing":{"rules":[],"default_route":"classify","classifier":{"on_error":"yandex"}}}`)
	c.engine.LLM = diagnosticCompleter(func(context.Context, []llm.Message, bool) (string, error) {
		return "", errors.New("private-provider-detail")
	})
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Engine: c.engine})
	response := diagnosticAPIRequest(mux, "POST", "decide", `{"text":"Куда направить запрос?"}`, "application/json")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"route":"yandex"`) || !strings.Contains(response.Body.String(), `"source":"classifier_error"`) || strings.Contains(response.Body.String(), "private-provider-detail") {
		t.Fatalf("classifier fallback differs from CLI: status=%d body=%s", response.Code, response.Body)
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
	c := configurationFixture(t, `{"mode":"observe","airplay":{"disabled":true},"routing":{"rules":[],"default_route":"llm"}}`)
	c.engine.LLM = diagnosticCompleter(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
		if structured || messages[len(messages)-1].Content != "Проверка" {
			t.Errorf("unexpected observe-mode query: messages=%+v structured=%t", messages, structured)
		}
		return "Ответ из backend.", nil
	})
	r := &fakeAirPlayReceiver{state: airplay.StateStopped}
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{AirPlay: &airPlayControl{receiver: r, app: t.Context()}, Engine: c.engine, Status: func() map[string]any { return map[string]any{"mode": "observe", "client_frames": 7} }})
	response := diagnosticAPIRequest(mux, "GET", "status", "", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"query":true`) || !strings.Contains(response.Body.String(), `"client_frames":7`) {
		t.Errorf("wrong capabilities/status: %s", response.Body)
	}
	if response := diagnosticAPIRequest(mux, "POST", "query", `{"text":"Проверка"}`, "application/json"); response.Code != 200 || !strings.Contains(response.Body.String(), `"answer":"Ответ из backend."`) {
		t.Errorf("observe query did not resolve like CLI: status=%d body=%s", response.Code, response.Body)
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

func TestPanelRoutingOptionsAndConflicts(t *testing.T) {
	c := configurationFixture(t, `{"routing":{"rules":[],"default_route":"yandex"}}`)
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Configuration: c})
	revision := func() string {
		t.Helper()
		r := diagnosticAPIRequest(mux, "GET", "routing", "", "")
		var state struct {
			SavedRevision string `json:"saved_revision"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &state) != nil || state.SavedRevision == "" {
			t.Fatal("missing saved revision")
		}
		return state.SavedRevision
	}
	post := func(endpoint, body string) *httptest.ResponseRecorder {
		return diagnosticAPIRequest(mux, "POST", endpoint, body, "application/json")
	}
	options := `{"expected_revision":"` + revision() + `","classifier_enabled":true,"default_route":"llm"}`
	if r := post("routing/options", options); r.Code != 200 || !c.engine.RoutingStatus().ClassifierEnabled {
		t.Fatal("classifier was not applied", r.Body)
	}
	if r := post("routing/options", options); r.Code != 409 {
		t.Fatalf("stale revision accepted: %d", r.Code)
	}
	if r := post("config", `{"changes":{"routing.classifier.enabled":false}}`); r.Code != 200 {
		t.Fatal(r.Body)
	}
	r := post("route", `{"text":"пример"}`)
	var preview struct {
		Active bridge.RouteDecision `json:"active"`
		Saved  bridge.RouteDecision `json:"saved"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &preview); err != nil || preview.Active.Route != "classify" || preview.Saved.Route != "llm" {
		t.Fatal("preview did not distinguish pending options", r.Body, err)
	}
	if r := post("routing/rule", `{"expected_revision":"`+revision()+`","action":"upsert","rule":{"name":"example","route":"llm","match":{"contains_any":["example"]}}}`); r.Code != 409 {
		t.Fatalf("unapplied changes overwritten: %d", r.Code)
	}
}
