package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/bridge"
)

func TestCommandEditorBrowserAccess(t *testing.T) {
	c := configurationFixture(t, `{"routing":{"commands":{"note":{"path":"/opt/voice-note","args":["private argument"]}}}}`)
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Configuration: c, Network: func() bool { return true }})
	const origin = "http://192.0.2.1:2704"
	const client = "192.0.2.2:3456"
	revision := c.engine.RoutingStatus().Revision
	body, err := json.Marshal(commandEdit{Action: "create", ID: "example", Command: &bridge.RoutingCommand{Path: "/bin/cat"}, ExpectedRevision: revision})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, peer, target string, headers http.Header) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target+"/diagnostics/api/routing/command?id=note", bytes.NewReader(body))
		r.RemoteAddr = peer
		if headers != nil {
			r.Header = headers.Clone()
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	editorToken := func(peer, target string) string {
		t.Helper()
		w := request("GET", peer, target, nil)
		var view struct {
			Token string `json:"csrf_token"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.Token == "" || !strings.Contains(w.Body.String(), "private argument") {
			t.Fatalf("editor read failed: %d", w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("editor response can be cached or shared across origins")
		}
		return view.Token
	}
	localToken := editorToken("[::1]:4110", "http://localhost:2704")
	path := filepath.Join(filepath.Dir(c.path), "command-editor.clients")
	if w := request("GET", client, origin, nil); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "private argument") {
		t.Fatalf("unlisted client could read editor: %d", w.Code)
	}
	if err := os.WriteFile(path, []byte("192.0.2.2\n2001:db8::2\n# 192.0.2.3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	token := editorToken(client, origin)
	otherToken := editorToken("[2001:db8::2]:4110", origin)
	if otherToken == token || localToken == token {
		t.Fatal("different clients received the same token")
	}
	if w := request("GET", "192.0.2.3:1234", origin, http.Header{"X-Forwarded-For": {"192.0.2.2"}}); w.Code != http.StatusForbidden {
		t.Fatal("trusted a forwarded client address")
	}
	if w := request("GET", client, origin, http.Header{"Sec-Fetch-Site": {"same-site"}}); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), token) {
		t.Fatal("token available to another origin")
	}
	before, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, peer, target, reqOrigin, csrf, site, contentType string
		status                                                 int
	}{
		{"no origin", client, origin, "", token, "", "application/json", 403},
		{"null origin", client, origin, "null", token, "", "application/json", 403},
		{"foreign origin", client, origin, "https://example.invalid", token, "", "application/json", 403},
		{"another port", client, origin, origin + "0", token, "", "application/json", 403},
		{"different scheme", client, origin, "https://192.0.2.1:2704", token, "", "application/json", 403},
		{"cross site", client, origin, origin, token, "cross-site", "application/json", 403},
		{"same site", client, origin, origin, token, "same-site", "application/json", 403},
		{"no token", client, origin, origin, "", "same-origin", "application/json", 403},
		{"incorrect token", client, origin, origin, "wrong", "same-origin", "application/json", 403},
		{"other client token", client, origin, origin, otherToken, "same-origin", "application/json", 403},
		{"other panel token", client, origin + "0", origin + "0", token, "", "application/json", 403},
		{"untrusted client", "192.0.2.3:1234", origin, origin, token, "", "application/json", 403},
		{"foreign DNS host", client, "http://example.invalid", "http://example.invalid", token, "", "application/json", 403},
		{"form body", client, origin, origin, token, "same-origin", "application/x-www-form-urlencoded", 415},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for name, value := range map[string]string{"Origin": tc.reqOrigin, "X-Quasar-CSRF": tc.csrf, "Sec-Fetch-Site": tc.site, "Content-Type": tc.contentType} {
				if value != "" {
					h.Set(name, value)
				}
			}
			if w := request("POST", tc.peer, tc.target, h); w.Code != tc.status {
				t.Fatalf("got %d, want %d", w.Code, tc.status)
			}
		})
	}
	h := http.Header{"Origin": {origin}, "Content-Type": {"application/json"}}
	h.Set("X-Quasar-CSRF", token)
	for _, field := range []string{"Origin", "X-Quasar-CSRF", "Sec-Fetch-Site"} {
		ambiguous := h.Clone()
		if field == "Sec-Fetch-Site" {
			ambiguous.Set(field, "same-origin")
		}
		ambiguous.Add(field, "duplicate")
		if w := request("POST", client, origin, ambiguous); w.Code != http.StatusForbidden {
			t.Fatalf("accepted duplicate %s: %d", field, w.Code)
		}
	}
	if after, err := os.ReadFile(c.path); err != nil || !bytes.Equal(after, before) || c.engine.RoutingStatus().Revision != revision {
		t.Fatal("a rejected request modified the configuration", err)
	}
	if w := request("POST", client, origin, h); w.Code != http.StatusOK || c.engine.RoutingStatus().Revision == revision {
		t.Fatalf("same-origin save failed: %d", w.Code)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if w := request("POST", client, origin, h); w.Code != http.StatusForbidden {
		t.Fatalf("revoked client retained write access: %d", w.Code)
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
