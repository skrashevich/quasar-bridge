//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"quasarbridge/internal/assistant"
	"quasarbridge/internal/mcpoauth"
)

type panelOAuthTransport struct {
	http.RoundTripper
	tokens   atomic.Int32
	requests atomic.Int32
	verifier atomic.Pointer[string]
	before   func(*http.Request)
}

func (p *panelOAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.requests.Add(1)
	if r.URL.Path == "/token" {
		p.tokens.Add(1)
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			return nil, err
		}
		r = r.Clone(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(body))
		form, _ := url.ParseQuery(string(body))
		verifier := form.Get("code_verifier")
		p.verifier.Store(&verifier)
	}
	if p.before != nil {
		p.before(r)
	}
	return p.RoundTripper.RoundTrip(r)
}

type panelOAuthFixture struct {
	mux       *http.ServeMux
	panel     *panelMCPAuth
	issuer    *httptest.Server
	store     *mcpoauth.Store
	binding   mcpoauth.Binding
	config    string
	ca        string
	transport *panelOAuthTransport
	logs      *diagnosticLog
	network   atomic.Bool
	lists     atomic.Int32
	calls     atomic.Int32
	deletes   atomic.Int32
}

func newPanelOAuthFixture(t *testing.T, before func(*http.Request), intercept func(http.ResponseWriter, *http.Request) bool, allowAnonymous ...func(*http.Request) bool) *panelOAuthFixture {
	t.Helper()
	f := &panelOAuthFixture{mux: http.NewServeMux(), logs: newDiagnosticLog(65536)}
	api := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	api.AddTool(&mcp.Tool{Name: "catalog_read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		f.calls.Add(1)
		return &mcp.CallToolResult{}, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return api }, nil)
	f.issuer, f.ca = newCLIOAuthServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			f.deletes.Add(1)
		}
		if intercept != nil && intercept(w, r) {
			return
		}
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var msg struct{ Method string }
			if json.Unmarshal(raw, &msg) == nil && msg.Method == "tools/list" {
				f.lists.Add(1)
			}
		}
		mcpHandler.ServeHTTP(w, r)
	}), allowAnonymous...)
	f.binding = mcpoauth.Binding{
		Name: "catalog", Endpoint: f.issuer.URL + "/mcp", Issuer: f.issuer.URL,
		ClientID: "https://client.example.invalid/client.json", Scopes: []string{"catalog:read"},
	}
	b := f.binding
	adminConfig, _ := json.Marshal(map[string]any{"assistant_tools": map[string]any{"enabled": true, "mcp_servers": []any{
		map[string]any{"name": b.Name, "url": b.Endpoint, "oauth": map[string]any{"client_id": b.ClientID, "issuer": b.Issuer, "scopes": b.Scopes}},
	}}})
	f.config = configFile(t, string(adminConfig))
	var err error
	f.store, err = mcpoauth.NewStore(f.config)
	if err != nil {
		t.Fatal(err)
	}
	config, err := readConfigForCommand(f.config, true)
	if err != nil {
		t.Fatal(err)
	}
	servers := config.AssistantTools.MCPServers
	second := servers[0]
	second.Name = "other"
	disabled := servers[0]
	disabled.Name, disabled.Disabled = "disabled", true
	servers = append(servers, second, disabled, assistant.MCPServerConfig{Name: "no-oauth"})
	for i := range 7 {
		extra := second
		extra.Name = "extra" + strconv.Itoa(i)
		servers = append(servers, extra)
	}
	ctx, cancel := context.WithCancel(t.Context())
	f.panel = newPanelMCPAuth(ctx, servers, f.store)
	guard := f.network.Load
	registerDiagnostics(f.mux, f.logs, func() map[string]any { return nil }, guard)
	registerDiagnosticsAPI(f.mux, diagnosticControl{MCPOAuth: f.panel, Network: guard})
	oldTransport, oldLogger := http.DefaultTransport, slog.Default()
	f.transport = &panelOAuthTransport{RoundTripper: f.issuer.Client().Transport, before: before}
	http.DefaultTransport = f.transport
	slog.SetDefault(slog.New(slog.NewTextHandler(f.logs, nil)))
	t.Cleanup(func() {
		cancel()
		f.panel.close()
		http.DefaultTransport = oldTransport
		slog.SetDefault(oldLogger)
	})
	return f
}

func (f *panelOAuthFixture) request(method, action string, value any) *httptest.ResponseRecorder {
	var body io.Reader
	if value != nil {
		raw, _ := json.Marshal(value)
		body = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:2704/diagnostics/api/mcp-auth"+action, body)
	req.RemoteAddr = "127.0.0.1:12345"
	if value != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1:2704")
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

type panelFixtureLogin struct {
	ID       string `json:"attempt_id"`
	URL      string `json:"authorization_url"`
	Redirect string `json:"redirect_uri"`
}

func (f *panelOAuthFixture) start(t *testing.T, server string) panelFixtureLogin {
	t.Helper()
	r := f.request("POST", "/start", map[string]string{"server": server})
	var login panelFixtureLogin
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &login) != nil ||
		login.ID == "" || login.Redirect != "http://127.0.0.1:19876/callback" {
		t.Fatalf("start login: HTTP %d", r.Code)
	}
	u, err := url.Parse(login.URL)
	if err != nil || u.Query().Get("redirect_uri") != login.Redirect || u.Query().Get("state") == "" {
		t.Fatal("authorization URL did not have the expected return and state")
	}
	return login
}

func (f *panelOAuthFixture) returned(login panelFixtureLogin, denied bool) string {
	u, _ := url.Parse(login.URL)
	q := url.Values{"state": {u.Query().Get("state")}, "iss": {f.issuer.URL}}
	if denied {
		q.Set("error", "access_denied")
	} else {
		q.Set("code", "never-print-code")
	}
	return login.Redirect + "?" + q.Encode()
}

func TestPanelMCPOAuthManualLogin(t *testing.T) {
	var failInit atomic.Bool
	f := newPanelOAuthFixture(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		if !failInit.Load() {
			return false
		}
		http.Error(w, "never-print-upstream-body", http.StatusBadGateway)
		return true
	})
	status := f.request("GET", "", nil)
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"status":"not signed in"`) ||
		strings.Contains(status.Body.String(), f.issuer.URL) || strings.Contains(status.Body.String(), "no-oauth") {
		t.Fatalf("bad initial account status: HTTP %d", status.Code)
	}
	login := f.start(t, "catalog")
	payload := map[string]string{"server": "catalog", "attempt_id": login.ID, "callback": f.returned(login, false)}
	r := f.request("POST", "/complete", payload)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"access token valid"`) ||
		f.transport.tokens.Load() != 1 || f.lists.Load() != 0 || f.calls.Load() != 0 {
		t.Fatalf("manual login: HTTP %d, token calls=%d, tool lists=%d, tool calls=%d", r.Code, f.transport.tokens.Load(), f.lists.Load(), f.calls.Load())
	}
	if f.deletes.Load() != 1 {
		t.Fatal("interactive login did not close the authenticated MCP session")
	}
	if replay := f.request("POST", "/complete", payload); replay.Code != 409 || f.transport.tokens.Load() != 1 {
		t.Fatalf("callback replay: HTTP %d", replay.Code)
	}
	if got := f.request("GET", "", nil); !strings.Contains(got.Body.String(), `"status":"access token valid"`) {
		t.Fatal("signed-in state missing in status")
	}
	c := mcpAuthChild(t, f.config, "-check-mcp")
	c.Env = append(c.Env, "SSL_CERT_FILE="+f.ca)
	if out, err := c.CombinedOutput(); err != nil || !strings.Contains(string(out), `"tool_names":["catalog_read"]`) ||
		f.lists.Load() == 0 || f.calls.Load() != 0 || strings.Contains(string(out), "never-print-") {
		t.Fatalf("shared CLI discovery failed: %v %s", err, out)
	}
	if err := f.store.Save(t.Context(), mcpoauth.Record{Binding: f.binding, AccessToken: "older-token"}); err != nil {
		t.Fatal(err)
	}
	for _, denied := range []bool{true, false} {
		failInit.Store(!denied)
		login := f.start(t, "catalog")
		failed := f.request("POST", "/complete", map[string]string{"server": "catalog", "attempt_id": login.ID, "callback": f.returned(login, denied)})
		if failed.Code != 422 || strings.Contains(failed.Body.String(), "never-print-") {
			t.Fatalf("failed login: HTTP %d", failed.Code)
		}
		if record, err := f.store.Load(f.binding); err != nil || record.AccessToken != "older-token" {
			t.Fatalf("failed reauthorization changed the stored token: %v", err)
		}
	}
	logs := f.logs.snapshot(0).Text
	verifier := f.transport.verifier.Load()
	if strings.Contains(r.Body.String()+logs, "never-print-") || verifier == nil ||
		strings.Contains(r.Body.String()+logs, *verifier) {
		t.Fatal("a callback, token or verifier leaked to panel output or logs")
	}
}

func TestPanelMCPOAuthNeverAuthorizesOnNotification(t *testing.T) {
	method := func(r *http.Request) string {
		if r.Method != http.MethodPost {
			return ""
		}
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct{ Method string }
		_ = json.Unmarshal(body, &msg)
		return msg.Method
	}
	f := newPanelOAuthFixture(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		if method(r) == "server/discover" {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		return false
	}, func(r *http.Request) bool {
		switch method(r) {
		case "server/discover", "initialize":
			return true
		}
		return false
	})
	got := f.request("POST", "/start", map[string]string{"server": "catalog"})
	if got.Code != 422 || f.transport.tokens.Load() != 0 {
		t.Fatalf("an MCP notification must not prompt login: HTTP %d", got.Code)
	}
	if _, err := f.store.Load(f.binding); !errors.Is(err, mcpoauth.ErrNotSignedIn) {
		t.Fatalf("anonymous initialization left a credential: %v", err)
	}
}

type failedMCPLinkWriter struct{ *httptest.ResponseRecorder }

func (failedMCPLinkWriter) FlushError() error { return io.ErrClosedPipe }

func TestPanelMCPOAuthFailedLinkDeliveryCancelsAttempt(t *testing.T) {
	f := newPanelOAuthFixture(t, nil, nil)
	req := httptest.NewRequest("POST", "http://127.0.0.1:2704/diagnostics/api/mcp-auth/start", strings.NewReader(`{"server":"catalog"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	f.mux.ServeHTTP(failedMCPLinkWriter{httptest.NewRecorder()}, req)
	if f.current("catalog") != nil || f.transport.tokens.Load() != 0 {
		t.Fatal("failed link delivery left a live attempt")
	}
}

func (f *panelOAuthFixture) current(server string) *panelMCPAttempt {
	s := f.panel.servers[server]
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func TestPanelMCPOAuthAttemptLifecycle(t *testing.T) {
	t.Run("start canceled before URL", func(t *testing.T) {
		reached := make(chan struct{}, 1)
		var blocked atomic.Bool
		f := newPanelOAuthFixture(t, func(r *http.Request) {
			// Only block the initial request, not the SDK's best-effort
			// cancellation notification with its own context.
			if r.URL.Path == "/mcp" && blocked.CompareAndSwap(false, true) {
				reached <- struct{}{}
				<-r.Context().Done()
			}
		}, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		req := httptest.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:2704/diagnostics/api/mcp-auth/start", strings.NewReader(`{"server":"catalog"}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		done := make(chan struct{})
		go func() { f.mux.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
		select {
		case <-reached:
		case <-time.After(2 * time.Second):
			t.Fatal("start did not begin discovery")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("canceled start did not return")
		}
		if f.current("catalog") != nil || f.transport.tokens.Load() != 0 {
			t.Fatal("canceled start left an attempt or issued a token")
		}
	})
	t.Run("expiry", func(t *testing.T) {
		f := newPanelOAuthFixture(t, nil, nil)
		f.panel.ttl = 250 * time.Millisecond
		login := f.start(t, "catalog")
		attempt := f.current("catalog")
		if attempt == nil {
			t.Fatal("login never became current")
		}
		<-attempt.done
		got := f.request("POST", "/complete", map[string]string{"server": "catalog", "attempt_id": login.ID, "callback": f.returned(login, false)})
		if got.Code != 409 || f.transport.tokens.Load() != 0 {
			t.Fatalf("expired callback: HTTP %d", got.Code)
		}
	})
	for _, action := range []string{"logout", "replace"} {
		t.Run(action, func(t *testing.T) {
			reached := make(chan struct{}, 1)
			release := make(chan struct{})
			defer close(release)
			f := newPanelOAuthFixture(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodDelete {
					return false
				}
				select {
				case reached <- struct{}{}:
				default:
				}
				<-release
				return false
			})
			login := f.start(t, "catalog")
			done := make(chan int, 1)
			go func() {
				done <- f.request("POST", "/complete", map[string]string{"server": "catalog", "attempt_id": login.ID, "callback": f.returned(login, false)}).Code
			}()
			select {
			case <-reached:
			case <-time.After(2 * time.Second):
				t.Fatal("did not finish authenticated initialization")
			}
			if r := f.request("GET", "", nil); r.Code != 200 {
				t.Fatal("status blocked by in-flight exchange")
			}
			other := f.start(t, "other")
			if action == "logout" {
				if r := f.request("POST", "/logout", map[string]string{"server": "catalog"}); r.Code != 200 {
					t.Fatalf("logout while completing: HTTP %d", r.Code)
				}
			} else {
				next := f.start(t, "catalog")
				if r := f.request("POST", "/cancel", map[string]string{"server": "catalog", "attempt_id": login.ID}); r.Code != 409 {
					t.Fatalf("cancel old attempt: HTTP %d", r.Code)
				}
				if current := f.current("catalog"); current == nil || current.id != next.ID {
					t.Fatal("old cancellation replaced the new attempt")
				}
			}
			release <- struct{}{}
			select {
			case code := <-done:
				if code != 409 {
					t.Fatalf("obsolete completion: HTTP %d", code)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("obsolete exchange did not unwind")
			}
			if _, err := f.store.Load(f.binding); !errors.Is(err, mcpoauth.ErrNotSignedIn) {
				t.Fatalf("obsolete attempt restored credentials: %v", err)
			}
			if r := f.request("POST", "/cancel", map[string]string{"server": "other", "attempt_id": other.ID}); r.Code != 200 {
				t.Fatalf("cancel other server: HTTP %d", r.Code)
			}
		})
	}
}

func TestPanelMCPOAuthAttemptLimit(t *testing.T) {
	f := newPanelOAuthFixture(t, nil, nil)
	first := f.start(t, "catalog")
	for i := range 7 {
		f.start(t, "extra"+strconv.Itoa(i))
	}
	before := f.transport.requests.Load()
	if r := f.request("POST", "/start", map[string]string{"server": "other"}); r.Code != 429 || f.transport.requests.Load() != before {
		t.Fatalf("ninth attempt was not blocked before discovery: HTTP %d", r.Code)
	}
	replacement := f.start(t, "catalog")
	if replacement.ID == first.ID {
		t.Fatal("new start did not replace the current attempt at capacity")
	}
	if r := f.request("POST", "/cancel", map[string]string{"server": "catalog", "attempt_id": replacement.ID}); r.Code != 200 {
		t.Fatalf("cancel did not release slot: HTTP %d", r.Code)
	}
	f.start(t, "other")
}

func TestPanelMCPOAuthUsesDiagnosticsAccess(t *testing.T) {
	f := newPanelOAuthFixture(t, nil, nil)
	for _, tc := range []struct {
		lan, enabled, cross bool
		origin, content     string
		body                string
		want                int
	}{
		{false, false, false, "http://elsewhere.example.invalid", "application/json", `{"server":"catalog"}`, 403},
		{false, false, true, "", "application/json", `{"server":"catalog"}`, 403},
		{true, false, false, "", "application/json", `{"server":"catalog"}`, 403},
		{false, false, false, "", "text/plain", `{"server":"catalog"}`, 415},
		{true, true, false, "http://192.0.2.2:2704", "application/json", `{"server":"missing"}`, 404},
		{true, true, false, "http://192.0.2.2:2704", "application/json", `{"server":"disabled"}`, 409},
		{true, true, false, "http://192.0.2.2:2704", "application/json", `{"server":"catalog","url":"https://elsewhere.example.invalid"}`, 400},
	} {
		f.network.Store(tc.enabled)
		host, peer := "127.0.0.1:2704", "127.0.0.1:12345"
		if tc.lan {
			host, peer = "192.0.2.2:2704", "192.0.2.3:12345"
		}
		req := httptest.NewRequest("POST", "http://"+host+"/diagnostics/api/mcp-auth/start", strings.NewReader(tc.body))
		req.RemoteAddr = peer
		req.Header.Set("Content-Type", tc.content)
		req.Header.Set("Origin", tc.origin)
		if tc.cross {
			req.Header.Set("Sec-Fetch-Site", "cross-site")
		}
		r := httptest.NewRecorder()
		f.mux.ServeHTTP(r, req)
		if r.Code != tc.want || f.transport.requests.Load() != 0 {
			t.Fatalf("guard allowed an unauthorized or malformed request: HTTP %d, want %d", r.Code, tc.want)
		}
	}
}
