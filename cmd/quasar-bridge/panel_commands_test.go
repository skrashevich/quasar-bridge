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
	network := false
	registerDiagnosticsAPI(mux, diagnosticControl{Configuration: c, Network: func() bool { return network }})
	const origin = "http://192.0.2.1:2704"
	const client = "192.0.2.2:3456"
	revision := c.engine.RoutingStatus().Revision
	example := c.active.Routing.Commands["note"]
	body, err := json.Marshal(commandEdit{Action: "create", ID: "example", Command: &example, ExpectedRevision: revision})
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
	openEditor := func(peer, target string) (string, string) {
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
		cookies := w.Result().Cookies()
		if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Secure != strings.HasPrefix(target, "https:") || cookies[0].Domain != "" {
			t.Fatal("missing host-only session cookie with CSRF protections")
		}
		return view.Token, cookies[0].Name + "=" + cookies[0].Value
	}
	openEditor("[::1]:4110", "https://localhost:2704")
	if w := request("GET", client, origin, http.Header{"X-Forwarded-For": {"127.0.0.1"}}); w.Code != http.StatusForbidden {
		t.Fatalf("disabled network access allowed editor read: %d", w.Code)
	}
	// An old installation may still have the previous version's list.
	if err := os.WriteFile(filepath.Join(filepath.Dir(c.path), "command-editor.clients"), []byte("192.0.2.77\n"), 0600); err != nil {
		t.Fatal(err)
	}
	network = true
	token, cookie := openEditor(client, origin)
	otherToken, otherCookie := openEditor("[2001:db8::2]:4110", origin)
	if otherToken == token || otherCookie == cookie {
		t.Fatal("independent browser sessions shared credentials")
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
		{"foreign DNS host", client, "http://example.invalid", "http://example.invalid", token, "", "application/json", 403},
		{"form body", client, origin, origin, token, "same-origin", "application/x-www-form-urlencoded", 415},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{"Cookie": {cookie}}
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
	if w := request("POST", client, origin, h); w.Code != http.StatusForbidden {
		t.Fatalf("accepted token without session: %d", w.Code)
	}
	h.Set("Cookie", otherCookie)
	if w := request("POST", client, origin, h); w.Code != http.StatusForbidden {
		t.Fatalf("accepted token from another session: %d", w.Code)
	}
	h.Set("Cookie", cookie)
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
	if w := request("POST", "192.0.2.9:9221", origin, h); w.Code != http.StatusOK || c.engine.RoutingStatus().Revision == revision {
		t.Fatalf("same browser could not save after an address change: %d", w.Code)
	}
	network = false
	if w := request("POST", client, origin, h); w.Code != http.StatusForbidden {
		t.Fatalf("disabled network access allowed editor write: %d", w.Code)
	}
	// A new system invocation stays available through the local operator panel.
	localOrigin := "http://127.0.0.1:2704"
	localToken, localCookie := openEditor("127.0.0.1:4444", localOrigin)
	edit := commandEdit{Action: "create", ID: "local_report", Command: &bridge.RoutingCommand{Path: "/opt/report"},
		ExpectedRevision: c.engine.RoutingStatus().Revision}
	body, err = json.Marshal(edit)
	if err != nil {
		t.Fatal(err)
	}
	network = true
	if w := request("POST", client, origin, h); w.Code != http.StatusBadRequest || c.engine.RoutingStatus().Revision != edit.ExpectedRevision {
		t.Fatalf("LAN client defined a new system invocation: %d", w.Code)
	}
	h.Set("Origin", localOrigin)
	h.Set("Cookie", localCookie)
	h.Set("X-Quasar-CSRF", localToken)
	if w := request("POST", "127.0.0.1:4444", localOrigin, h); w.Code != http.StatusOK {
		t.Fatalf("local operator could not register an invocation: %d", w.Code)
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
	if _, err := c.editCommand(t.Context(), edit, false); err == nil {
		t.Fatal("accepted relative program path")
	}
	if raw, err := os.ReadFile(c.path); err != nil || string(raw) != string(before) {
		t.Fatal("invalid edit changed the file", err)
	}
	dir := filepath.Join(filepath.Dir(c.path), "commands")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	edit.Command.Path = filepath.Join(dir, "voice-note")
	if err := os.WriteFile(edit.Command.Path, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := c.editCommand(t.Context(), edit, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.editCommand(t.Context(), edit, false); err == nil || !strings.Contains(err.Error(), "revision changed") {
		t.Fatal("accepted stale revision", err)
	}
	edit.ExpectedRevision = c.engine.RoutingStatus().Revision
	if _, err := c.editCommand(t.Context(), edit, false); err == nil {
		t.Fatal("create overwrote an existing command")
	}
	if backup, err := os.ReadFile(c.path + ".voice-backup"); err != nil || string(backup) != string(before) {
		t.Fatal("missing original configuration backup", err)
	}
	view, err := c.GetRouting(t.Context())
	data, marshalErr := json.Marshal(view)
	if err != nil || marshalErr != nil || strings.Contains(string(data), edit.Command.Path) || !strings.Contains(string(data), `"command_ids":["existing","note"]`) || !strings.Contains(string(data), `"pending":false`) {
		t.Fatal("public routing must expose only registered IDs", err, marshalErr)
	}
}

func TestPanelCommandPrograms(t *testing.T) {
	c := configurationFixture(t, `{"routing":{"commands":{"existing":{"path":"/opt/report","args":["daily"]}}}}`)
	dir := filepath.Join(filepath.Dir(c.path), "commands")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "note")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	check := func(path string, args []string, allow bool) {
		t.Helper()
		err := c.validatePanelProgram(bridge.RoutingCommand{Path: path, Args: args})
		if (err == nil) != allow {
			t.Fatalf("path %s with args %q: allow=%v, err=%v", path, args, allow, err)
		}
	}
	check(program, []string{"argument with spaces"}, true)
	check("/opt/report", []string{"daily"}, true)
	check("/opt/report", []string{"different"}, false)
	check("/bin/cat", nil, false)
	check(filepath.Join(dir, "missing"), nil, false)
	check(dir+"/../commands/note", nil, false)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(program, link); err != nil {
		t.Fatal(err)
	}
	check(link, nil, false)
	for _, mode := range []os.FileMode{0600, 0777} {
		if err := os.Chmod(program, mode); err != nil {
			t.Fatal(err)
		}
		check(program, nil, false)
	}
}
