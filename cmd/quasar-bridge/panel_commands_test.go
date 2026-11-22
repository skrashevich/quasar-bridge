package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
	var heard int
	c.approvals.speak = func(_ context.Context, n int) error { heard = n; return nil }
	example := bridge.RoutingCommand{Path: "/usr/bin/date", Args: []string{"+%F"}}
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
	if heard != 0 {
		t.Fatal("CSRF rejection played a code")
	}
	w := request("POST", "192.0.2.9:9221", origin, h)
	var ticket commandTicket
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &ticket) != nil || ticket.ID == "" || heard < 100 || heard > 999 {
		t.Fatalf("expected spoken confirmation on LAN, got %d", w.Code)
	}
	if after, err := os.ReadFile(c.path); err != nil || !bytes.Equal(after, before) || c.engine.RoutingStatus().Revision != revision {
		t.Fatal("draft took effect before confirmation", err)
	}
	confirm := map[string]string{"action": "confirm", "challenge_id": ticket.ID, "code": strconv.Itoa(heard)}
	body, _ = json.Marshal(confirm)
	otherHeaders := h.Clone()
	otherHeaders.Set("Cookie", otherCookie)
	otherHeaders.Set("X-Quasar-CSRF", otherToken)
	if w := request("POST", client, origin, otherHeaders); w.Code != http.StatusGone {
		t.Fatal("another session confirmed the command")
	}
	confirm["id"] = "substitution"
	body, _ = json.Marshal(confirm)
	if w := request("POST", client, origin, h); w.Code != http.StatusBadRequest {
		t.Fatal("confirmation accepted a modified draft")
	}
	delete(confirm, "id")
	body, _ = json.Marshal(confirm)
	network = false
	if w := request("POST", client, origin, h); w.Code != http.StatusForbidden {
		t.Fatal("confirmation available with network disabled")
	}
	network = true
	if w := request("POST", "192.0.2.9:2222", origin, h); w.Code != http.StatusOK || c.engine.RoutingStatus().Revision == revision {
		t.Fatalf("could not confirm from the same browser after changing address: %d", w.Code)
	}
	got := c.active.Routing.Commands["example"]
	if got.Path != example.Path || !slices.Equal(got.Args, example.Args) {
		t.Fatal("authorized invocation was not applied")
	}
	if w := request("POST", client, origin, h); w.Code != http.StatusGone {
		t.Fatal("accepted a used challenge")
	}
	localOrigin := "http://127.0.0.1:2704"
	localToken, localCookie := openEditor("127.0.0.1:4444", localOrigin)
	h.Set("Origin", localOrigin)
	h.Set("Cookie", localCookie)
	h.Set("X-Quasar-CSRF", localToken)
	example.Args = []string{"+%Z"}
	body, _ = json.Marshal(commandEdit{Action: "update", ID: "example", OriginalID: "example", Command: &example, ExpectedRevision: c.engine.RoutingStatus().Revision})
	if w := request("POST", "127.0.0.1:4444", localOrigin, h); w.Code != http.StatusAccepted || slices.Equal(c.active.Routing.Commands["example"].Args, example.Args) {
		t.Fatal("changing only arguments on loopback also requires confirmation")
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
	edit.Command.Path = "/opt/helpers/voice-note"
	if _, err := c.editCommand(t.Context(), edit, false); !errors.Is(err, errCommandApprovalRequired) {
		t.Fatal("valid but unconfirmed program", err)
	}
	if _, err := c.editCommand(t.Context(), edit, true); err != nil {
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

func TestSpokenCodeLimitsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	now := time.Now()
	var heard, plays int
	speaker := func(context.Context, int) error { return errors.New("speaker unavailable") }
	a := &commandApprovals{now: func() time.Time { return now }, speak: speaker}
	edit := commandEdit{ID: "date"}
	if _, err := a.begin(t.Context(), path, "owner", edit); !errors.Is(err, errAudioUnavailable) || a.pending != nil {
		t.Fatal("failed playback left an approvable edit")
	}
	now = now.Add(issueInterval)
	a.speak = func(_ context.Context, n int) error { heard = n; plays++; return nil }
	ticket, err := a.begin(t.Context(), path, "owner", edit)
	if err != nil {
		t.Fatal(err)
	}
	if dup, err := a.begin(t.Context(), path, "owner", edit); err != nil || dup.ID != ticket.ID || plays != 1 {
		t.Fatal("a retried draft replayed the number", err)
	}
	if _, err := a.begin(t.Context(), path, "new-cookie", edit); err == nil || plays != 1 {
		t.Fatal("another browser could replace a pending challenge")
	}
	if _, err := a.begin(t.Context(), path, "owner", commandEdit{ID: "other"}); err == nil {
		t.Fatal("another draft could reuse an approval")
	}
	if _, err := a.finish(path, "owner", ticket.ID, strconv.Itoa(heard), true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.finish(path, "owner", ticket.ID, strconv.Itoa(heard), false); !errors.Is(err, errExpiredChallenge) {
		t.Fatal("cancelled code accepted")
	}
	if _, err := a.begin(t.Context(), path, "other", edit); err == nil {
		t.Fatal("cancel reset audio rate limit")
	}
	now = now.Add(issueInterval)
	ticket, err = a.begin(t.Context(), path, "owner", edit)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(codeLifetime)
	if _, err := a.finish(path, "owner", ticket.ID, strconv.Itoa(heard), false); !errors.Is(err, errExpiredChallenge) {
		t.Fatal("expired code accepted")
	}
	for i := 0; i < 3; i++ {
		// A restart loses the number but keeps the station-wide guess budget.
		a = &commandApprovals{now: a.now, speak: a.speak}
		now = now.Add(issueInterval)
		ticket, err = a.begin(t.Context(), path, strconv.Itoa(i), edit)
		if err != nil {
			t.Fatal(err)
		}
		_, err = a.finish(path, strconv.Itoa(i), ticket.ID, strconv.Itoa(100+((heard-100+1)%900)), false)
		if i < 2 && !errors.Is(err, errWrongCode) {
			t.Fatal("wrong code accepted", err)
		}
		if i == 2 {
			if wait, ok := errors.AsType[approvalWait](err); !ok || wait.after != 24*time.Hour {
				t.Fatal("missing daily lockout")
			}
		}
	}
	a = &commandApprovals{now: a.now, speak: a.speak}
	now = now.Add(23 * time.Hour)
	if _, err := a.begin(t.Context(), path, "new-session", edit); err == nil {
		t.Fatal("restart or new session bypassed the limit")
	}
	now = now.Add(time.Hour)
	if _, err := a.begin(t.Context(), path, "new-session", edit); err != nil {
		t.Fatal("lockout failed to expire", err)
	}
	info, err := os.Stat(path + ".command-limits")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("attempt counter is not private")
	}
}

func TestCommandConfirmationRechecksRevisionAndIsSingleUse(t *testing.T) {
	c := configurationFixture(t, `{"routing":{"commands":{"note":{"path":"/opt/note"}}}}`)
	var heard string
	c.approvals.speak = func(_ context.Context, n int) error { heard = strconv.Itoa(n); return nil }
	before := c.engine.RoutingStatus().Revision
	edit := commandEdit{Action: "create", ID: "report", Command: &bridge.RoutingCommand{Path: "/opt/report"}, ExpectedRevision: before}
	ticket, err := c.approvals.begin(t.Context(), c.path, "browser", edit)
	if err != nil {
		t.Fatal(err)
	}
	description := "Записать заметку"
	nameOnly := commandEdit{Action: "update", ID: "notes", OriginalID: "note", Command: &bridge.RoutingCommand{Path: "/opt/note"}, Description: &description, ExpectedRevision: before}
	if _, err := c.editCommand(t.Context(), nameOnly, false); err != nil {
		t.Fatal("metadata-only edit required a code", err)
	}
	check := make(chan error, 2)
	for range 2 {
		go func() {
			saved, err := c.approvals.finish(c.path, "browser", ticket.ID, heard, false)
			if err == nil {
				_, err = c.editCommand(t.Context(), saved, true)
			}
			check <- err
		}()
	}
	var expired, stale bool
	for range 2 {
		err := <-check
		if err == nil {
			t.Fatal("draft replaced a newer routing configuration")
		}
		expired = expired || errors.Is(err, errExpiredChallenge)
		stale = stale || strings.Contains(err.Error(), "routing revision changed")
	}
	if !expired || !stale || len(c.active.Routing.Commands) != 1 || c.active.Routing.Commands["notes"].Path != "/opt/note" {
		t.Fatal("concurrent confirmations must consume once and preserve the new revision")
	}
}
