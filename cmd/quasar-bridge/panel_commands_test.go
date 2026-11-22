package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/bridge"
)

func TestCommandEditorRequiresOwnerKeyOverNetwork(t *testing.T) {
	c := configurationFixture(t, `{"routing":{"commands":{"note":{"path":"/opt/voice-note","args":["private argument"]}}}}`)
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Configuration: c, Network: func() bool { return true }})
	get := func(method, token, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://192.0.2.1:2704/diagnostics/api/routing/command?id=note", nil)
		r.RemoteAddr = "192.0.2.2:3456"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	for _, method := range []string{"GET", "POST"} {
		w := get(method, "", "")
		if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "private argument") {
			t.Fatalf("%s improperly exposed the editor: %d", method, w.Code)
		}
	}
	path := filepath.Join(filepath.Dir(c.path), "command-editor.key")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("missing private editor key", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(string(data))
	if w := get("GET", "wrong", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key returned %d", w.Code)
	}
	if w := get("GET", key, "https://elsewhere.invalid"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site request returned %d", w.Code)
	}
	if w := get("GET", key, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "private argument") || strings.Contains(w.Body.String(), key) {
		t.Fatalf("authorized editor failed: %d", w.Code)
	}
	if w := get("POST", key, ""); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("editor bypassed JSON validation: %d", w.Code)
	}
}

func TestCommandEditorRejectsInvalidAndStaleEdits(t *testing.T) {
	c := configurationFixture(t, `{"routing":{"rules":[],"commands":{"existing":{"path":"/opt/old","args":[]}}}}`)
	edit := commandEdit{Action: "create", ID: "note", Command: &bridge.RoutingCommand{Path: "relative"},
		ExpectedRevision: c.engine.RoutingStatus().Revision}
	before, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.editCommand(t.Context(), edit); err == nil {
		t.Fatal("accepted relative program path")
	}
	if raw, err := os.ReadFile(c.path); err != nil || string(raw) != string(before) {
		t.Fatal("invalid edit changed the file", err)
	}
	edit.Command.Path = "/opt/voice-note"
	if _, err := c.editCommand(t.Context(), edit); err != nil {
		t.Fatal(err)
	}
	if _, err := c.editCommand(t.Context(), edit); err == nil || !strings.Contains(err.Error(), "revision changed") {
		t.Fatal("accepted stale revision", err)
	}
	edit.ExpectedRevision = c.engine.RoutingStatus().Revision
	if _, err := c.editCommand(t.Context(), edit); err == nil {
		t.Fatal("create overwrote an existing command")
	}
	if backup, err := os.ReadFile(c.path + ".voice-backup"); err != nil || string(backup) != string(before) {
		t.Fatal("missing original configuration backup", err)
	}
	view, err := c.GetRouting(t.Context())
	data, marshalErr := json.Marshal(view)
	if err != nil || marshalErr != nil || strings.Contains(string(data), "/opt/voice-note") || !strings.Contains(string(data), `"command_ids":["existing","note"]`) || !strings.Contains(string(data), `"pending":false`) {
		t.Fatal("public routing must expose only registered IDs", err, marshalErr)
	}
}
