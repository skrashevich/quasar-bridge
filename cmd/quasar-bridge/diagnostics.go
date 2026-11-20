package main

import (
	"bytes"
	_ "embed"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"quasarbridge/internal/update"
)

//go:embed diagnostics.html
var diagnosticsHTML string

//go:embed diagnostics.js
var diagnosticsJS string

//go:embed diagnostics.css
var diagnosticsCSS string

// diagnosticLog retains a bounded tail of the same JSON records sent to stderr.
// It never opens configuration, identity or conversation-history files.
type diagnosticLog struct {
	mu      sync.Mutex
	data    []byte
	limit   int
	cursor  int64
	run     string
	partial bool
}

func newDiagnosticLog(limit int) *diagnosticLog {
	return &diagnosticLog{limit: limit, data: make([]byte, 0, limit), run: strconv.FormatInt(time.Now().UnixNano(), 36)}
}

func (l *diagnosticLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	l.cursor += int64(n)
	if n >= l.limit {
		if n > l.limit {
			l.partial = p[n-l.limit-1] != '\n'
		} else if len(l.data) > 0 {
			l.partial = l.data[len(l.data)-1] != '\n'
		}
		l.data = append(l.data[:0], p[n-l.limit:]...)
	} else {
		if excess := len(l.data) + n - l.limit; excess > 0 {
			l.partial = l.data[excess-1] != '\n'
			l.data = l.data[:copy(l.data, l.data[excess:])]
		}
		l.data = append(l.data, p...)
	}
	return n, nil
}

type diagnosticSnapshot struct {
	Run       string         `json:"run"`
	Cursor    int64          `json:"cursor"`
	Reset     bool           `json:"reset"`
	Truncated bool           `json:"truncated"`
	Text      string         `json:"text"`
	Version   string         `json:"version"`
	AirPlay   map[string]any `json:"airplay"`
}

func (l *diagnosticLog) snapshot(after int64) diagnosticSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	start := l.cursor - int64(len(l.data))
	reset := after < start || after > l.cursor
	if reset {
		after = start
	}
	data := l.data[after-start:]
	if l.partial && after == start {
		// An overwritten record must not appear as a complete JSON entry.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	return diagnosticSnapshot{Run: l.run, Cursor: l.cursor, Reset: reset, Truncated: start > 0, Text: string(data)}
}

func localDiagnostics(next http.HandlerFunc) http.HandlerFunc {
	return diagnosticsAccess(nil, next)
}

func registerDiagnostics(mux *http.ServeMux, logs *diagnosticLog, status func() map[string]any, network ...func() bool) {
	var access func() bool
	if len(network) > 0 {
		access = network[0]
	}
	guard := func(next http.HandlerFunc) http.HandlerFunc { return diagnosticsAccess(access, next) }
	mux.HandleFunc("GET /diagnostics", guard(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/diagnostics/", http.StatusTemporaryRedirect)
	}))
	for _, asset := range []struct{ path, contentType, body string }{
		{"/diagnostics/{$}", "text/html; charset=utf-8", diagnosticsHTML},
		{"/diagnostics/app.js", "text/javascript; charset=utf-8", diagnosticsJS},
		{"/diagnostics/app.css", "text/css; charset=utf-8", diagnosticsCSS},
	} {
		mux.HandleFunc("GET "+asset.path, guard(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", asset.contentType)
			_, _ = fmt.Fprint(w, asset.body)
		}))
	}
	mux.HandleFunc("GET /diagnostics/logs", guard(func(w http.ResponseWriter, r *http.Request) {
		after := int64(0)
		if r.URL.Query().Get("run") == logs.run {
			if value := r.URL.Query().Get("after"); value != "" {
				var err error
				after, err = strconv.ParseInt(value, 10, 64)
				if err != nil || after < 0 {
					http.Error(w, "Invalid log cursor", http.StatusBadRequest)
					return
				}
			}
		}
		snapshot := logs.snapshot(after)
		snapshot.Version, snapshot.AirPlay = update.Version, status()
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(snapshot)
		_, _ = w.Write(body)
	}))
	mux.HandleFunc("GET /diagnostics/download", guard(func(w http.ResponseWriter, r *http.Request) {
		snapshot := logs.snapshot(0)
		state, _ := json.Marshal(status())
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="quasar-bridge-diagnostics-`+time.Now().UTC().Format("20060102-150405")+`.txt"`)
		_, _ = fmt.Fprintf(w, "Quasar Bridge diagnostics\nVersion: %s\nCaptured: %s\nLog truncated: %t\nAirPlay: %s\n\n%s", update.Version, time.Now().UTC().Format(time.RFC3339), snapshot.Truncated, state, snapshot.Text)
	}))
}
