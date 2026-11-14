package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/journal"
)

type diagnosticControl struct {
	Configuration *configurationControl
	Engine        *bridge.Engine
	Logs          *journal.Writer
	Status        func() map[string]any
	AirPlay       *airPlayControl
	Network       func() bool
	QueryBusy     *atomic.Bool
}

func registerDiagnosticsAPI(mux *http.ServeMux, control diagnosticControl) {
	if control.QueryBusy == nil {
		control.QueryBusy = new(atomic.Bool)
	}
	register := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, diagnosticsAccess(control.Network, handler))
	}
	register("GET /diagnostics/api/status", func(w http.ResponseWriter, r *http.Request) {
		state := map[string]any{}
		if control.Status != nil {
			maps.Copy(state, control.Status())
		}
		state["capabilities"] = map[string]bool{
			"query": control.Engine != nil, "decide": control.Engine != nil, "config": control.Configuration != nil,
			"history": control.Logs != nil && control.Logs.Config().Path != "", "airplay": control.AirPlay != nil,
		}
		writeDiagnosticJSON(w, http.StatusOK, state)
	})
	register("GET /diagnostics/api/config", func(w http.ResponseWriter, r *http.Request) {
		if control.Configuration == nil {
			writeDiagnosticError(w, http.StatusServiceUnavailable, "Configuration control is unavailable")
			return
		}
		value, err := control.Configuration.Get(r.Context(), r.URL.Query().Get("section"))
		writeDiagnosticResult(w, value, err)
	})
	register("POST /diagnostics/api/config", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Changes map[string]jsontext.Value `json:"changes"`
		}
		if !readDiagnosticJSON(w, r, &request) {
			return
		}
		if control.Configuration == nil {
			writeDiagnosticError(w, http.StatusServiceUnavailable, "Configuration control is unavailable")
			return
		}
		value, err := control.Configuration.Set(r.Context(), request.Changes)
		writeDiagnosticResult(w, value, err)
	})
	register("POST /diagnostics/api/apply", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Action string `json:"action"`
		}
		if !readDiagnosticJSON(w, r, &request) {
			return
		}
		if request.Action != "reload" && request.Action != "restart" {
			writeDiagnosticError(w, http.StatusBadRequest, "Configuration action must be reload or restart")
			return
		}
		if control.Configuration == nil {
			writeDiagnosticError(w, http.StatusServiceUnavailable, "Configuration control is unavailable")
			return
		}
		value, err := control.Configuration.Apply(r.Context(), request.Action)
		writeDiagnosticResult(w, value, err)
	})
	register("POST /diagnostics/api/route", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Text string `json:"text"`
		}
		if !readDiagnosticJSON(w, r, &request) || !validDiagnosticText(w, request.Text) {
			return
		}
		if control.Configuration == nil {
			writeDiagnosticError(w, http.StatusServiceUnavailable, "Routing control is unavailable")
			return
		}
		value, err := control.Configuration.TestRouting(r.Context(), request.Text)
		writeDiagnosticResult(w, value, err)
	})
	register("POST /diagnostics/api/decide", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Text string `json:"text"`
		}
		if !readDiagnosticJSON(w, r, &request) || !validDiagnosticText(w, request.Text) {
			return
		}
		if control.Engine == nil {
			writeDiagnosticError(w, http.StatusServiceUnavailable, "Live routing control is unavailable")
			return
		}
		if !control.QueryBusy.CompareAndSwap(false, true) {
			writeDiagnosticError(w, http.StatusConflict, "Another diagnostic query or route check is running")
			return
		}
		defer control.QueryBusy.Store(false)
		ctx, cancel := context.WithTimeout(dialogue.WithSpeaker(r.Context(), "quasar-bridge-dashboard"), 75*time.Second)
		defer cancel()
		started := time.Now()
		// Match CLI -route, including special handlers and classifier fallback,
		// without generating an answer or executing the chosen action. Observe
		// mode still permits this diagnostic path, as it does in the CLI.
		decision := control.Engine.Decide(ctx, request.Text)
		writeDiagnosticJSON(w, http.StatusOK, struct {
			bridge.RouteDecision
			ElapsedMS int64 `json:"elapsed_ms"`
		}{RouteDecision: decision, ElapsedMS: time.Since(started).Milliseconds()})
	})
	register("POST /diagnostics/api/query", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Text string `json:"text"`
		}
		if !readDiagnosticJSON(w, r, &request) || !validDiagnosticText(w, request.Text) {
			return
		}
		if control.Engine == nil {
			writeDiagnosticError(w, http.StatusServiceUnavailable, "Text query control is unavailable")
			return
		}
		if !control.QueryBusy.CompareAndSwap(false, true) {
			writeDiagnosticError(w, http.StatusConflict, "Another diagnostic query or route check is running")
			return
		}
		defer control.QueryBusy.Store(false)
		// Dashboard tests have their own dialogue identity, separate from every
		// speaker and unnamed CLI request. All tabs intentionally share one test
		// conversation, bounded by the normal dialogue policy and query lock.
		ctx, cancel := context.WithTimeout(dialogue.WithSpeaker(r.Context(), "quasar-bridge-dashboard"), 75*time.Second)
		defer cancel()
		started := time.Now()
		result, err := control.Engine.Resolve(ctx, request.Text)
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			// Runtime implementations may return URLs, paths or provider text in
			// errors. Keep diagnostics useful without copying those into logs.
			slog.Warn("diagnostic_query_failed", "route", result.Route, "timeout", errors.Is(err, context.DeadlineExceeded), "error_type", fmt.Sprintf("%T", err))
			writeDiagnosticError(w, status, "Diagnostic query failed; inspect the operational log")
			return
		}
		writeDiagnosticJSON(w, http.StatusOK, struct {
			Route     string `json:"route"`
			Answer    string `json:"answer"`
			ElapsedMS int64  `json:"elapsed_ms"`
		}{result.Route, result.Answer, time.Since(started).Milliseconds()})
	})
	register("GET /diagnostics/api/history", func(w http.ResponseWriter, r *http.Request) {
		if control.Logs == nil || control.Logs.Config().Path == "" {
			writeDiagnosticError(w, http.StatusServiceUnavailable, "Conversation history is unavailable")
			return
		}
		query := assistant.HistoryQuery{Query: r.URL.Query().Get("query"), Since: r.URL.Query().Get("since"), Until: r.URL.Query().Get("until")}
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			query.Limit, err = strconv.Atoi(raw)
			if err != nil || query.Limit < 1 || query.Limit > 20 {
				writeDiagnosticError(w, http.StatusBadRequest, "History limit must be an integer from 1 to 20")
				return
			}
		}
		settings := assistant.DefaultConfig()
		if control.Configuration != nil {
			control.Configuration.mu.Lock()
			settings = control.Configuration.active.AssistantTools
			control.Configuration.mu.Unlock()
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		result, err := assistant.SearchHistory(ctx, control.Logs.Config(), query, settings.HistoryMaxScanMB)
		result.Timezone = settings.Timezone
		writeDiagnosticResult(w, result, err)
	})
	register("POST /diagnostics/api/airplay", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Action string `json:"action"`
		}
		if !readDiagnosticJSON(w, r, &request) {
			return
		}
		media := slices.Contains([]string{"pause", "resume", "next", "previous", "restart", "volume_up", "volume_down", "media_stop"}, request.Action)
		if raw, volume := strings.CutPrefix(request.Action, "volume:"); volume {
			percent, err := strconv.Atoi(raw)
			if err != nil || percent < 0 || percent > 100 {
				writeDiagnosticError(w, http.StatusBadRequest, "AirPlay volume must be between 0 and 100")
				return
			}
			media = true
		}
		if !media && !slices.Contains([]string{"start", "stop", "status"}, request.Action) {
			writeDiagnosticError(w, http.StatusBadRequest, "Unknown AirPlay action")
			return
		}
		if control.AirPlay == nil {
			writeDiagnosticError(w, http.StatusServiceUnavailable, "AirPlay is unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		var message string
		var err error
		if media {
			action := request.Action
			if action == "media_stop" {
				action = "stop"
			}
			message, err = control.AirPlay.ControlMedia(ctx, action)
		} else {
			message, err = control.AirPlay.Control(ctx, request.Action)
		}
		if err != nil {
			writeDiagnosticError(w, http.StatusConflict, "AirPlay control failed; inspect the operational log and sender connection")
			return
		}
		writeDiagnosticJSON(w, http.StatusOK, map[string]any{"message": message, "airplay": airPlayHealth(control.AirPlay)})
	})
}

func readDiagnosticJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		writeDiagnosticError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		writeDiagnosticError(w, http.StatusRequestEntityTooLarge, "Request body exceeds 256 KiB")
		return false
	}
	if err != nil || len(data) == 0 || json.Unmarshal(data, target, json.RejectUnknownMembers(true)) != nil {
		writeDiagnosticError(w, http.StatusBadRequest, "Invalid JSON request or unknown fields")
		return false
	}
	return true
}

func validDiagnosticText(w http.ResponseWriter, text string) bool {
	if strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > 8000 {
		writeDiagnosticError(w, http.StatusBadRequest, "Text must contain between 1 and 8000 characters")
		return false
	}
	return true
}

func writeDiagnosticResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			status = http.StatusGatewayTimeout
		case strings.Contains(err.Error(), "changed externally"), strings.Contains(err.Error(), "restart already scheduled"), strings.Contains(err.Error(), "restart required"):
			status = http.StatusConflict
		case strings.Contains(err.Error(), "unavailable"), strings.Contains(err.Error(), "requires the standalone"):
			status = http.StatusServiceUnavailable
		}
		writeDiagnosticError(w, status, err.Error())
		return
	}
	writeDiagnosticJSON(w, http.StatusOK, value)
}

func writeDiagnosticError(w http.ResponseWriter, status int, message string) {
	writeDiagnosticJSON(w, status, map[string]string{"error": message})
}

func writeDiagnosticJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"Cannot encode response"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
