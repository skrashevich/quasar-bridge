//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/skrashevich/quasar-bridge/internal/mcpoauth"
)

func TestMCPAuthChild(t *testing.T) {
	if os.Getenv("QUASAR_MCP_AUTH_TEST_CHILD") != "1" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
	os.Args = append([]string{"quasar-bridge"}, os.Args[slices.Index(os.Args, "--")+1:]...)
	main()
}

func mcpAuthChild(t *testing.T, config string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	child := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestMCPAuthChild$", "--", "-config", config}, args...)...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "QUASAR_") {
			child.Env = append(child.Env, entry)
		}
	}
	child.Env = append(child.Env, "QUASAR_MCP_AUTH_TEST_CHILD=1",
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	return child
}

func mcpAuthConfig(t *testing.T, origin string, disabled bool) (string, mcpoauth.Binding) {
	t.Helper()
	b := mcpoauth.Binding{Name: "catalog", Endpoint: origin + "/mcp", Issuer: origin,
		ClientID: "https://client.example.invalid/client.json", Scopes: []string{"catalog:read"}}
	body, err := json.Marshal(map[string]any{"assistant_tools": map[string]any{"enabled": true, "mcp_servers": []any{
		map[string]any{"name": b.Name, "url": b.Endpoint, "disabled": disabled, "oauth": map[string]any{"client_id": b.ClientID, "issuer": b.Issuer, "scopes": b.Scopes}},
		map[string]any{"name": "anonymous", "url": b.Endpoint},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return configFile(t, string(body)), b
}

func TestMCPAuthCLIStatusLogout(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Error("local authentication command unexpectedly used the network")
	}))
	defer server.Close()
	config, binding := mcpAuthConfig(t, server.URL, true)
	store, err := mcpoauth.NewStore(config)
	if err != nil {
		t.Fatal(err)
	}
	cmd := func(action, want string) {
		t.Helper()
		out, err := mcpAuthChild(t, config, "-mcp-auth", action, "-mcp-server", "catalog").Output()
		if err != nil || strings.SplitN(string(out), "\n", 2)[0] != want {
			t.Fatalf("%s: %q, %v", action, out, err)
		}
	}
	cmd("status", "not signed in")
	for _, v := range []struct {
		expiry  time.Time
		refresh string
		status  string
	}{
		{time.Now().Add(time.Hour), "", "access token valid"},
		{time.Now().Add(-time.Hour), "fixture-refresh", "refresh needed"},
		{time.Now().Add(-time.Hour), "", "sign-in required"},
	} {
		if err := store.Save(t.Context(), mcpoauth.Record{Binding: binding,
			AccessToken: "never-print-token", Expiry: v.expiry, RefreshToken: v.refresh}); err != nil {
			t.Fatal(err)
		}
		cmd("status", v.status)
	}
	cmd("logout", "logged out locally")
	cmd("status", "not signed in")
	if requests.Load() != 0 {
		t.Fatal("status or logout contacted the MCP server")
	}
}

func TestMCPAuthCLIConflicts(t *testing.T) {
	active, _ := mcpAuthConfig(t, "https://mcp.example.invalid", false)
	disabled, _ := mcpAuthConfig(t, "https://mcp.example.invalid", true)
	for _, tc := range []struct {
		config string
		args   []string
		want   string
	}{
		{active, []string{"-mcp-auth", "login", "-mcp-server", "catalog", "-check-mcp"}, "mutually exclusive"},
		{active, []string{"-mcp-auth", "status", "-mcp-server", "unknown"}, "MCP OAuth server is not configured"},
		{active, []string{"-mcp-auth", "status", "-mcp-server", "anonymous"}, "MCP OAuth server is not configured"},
		{disabled, []string{"-mcp-auth", "login", "-mcp-server", "catalog"}, "MCP OAuth server is disabled"},
		{active, []string{"-mcp-auth", "login", "-mcp-server", "catalog", "-mcp-auth-listen", "0.0.0.0:19876"}, "IPv4 loopback"},
		{active, []string{"-mcp-auth", "login", "-mcp-server", "catalog", "-mcp-auth-listen", "127.0.0.1:80"}, "IPv4 loopback"},
	} {
		out, err := mcpAuthChild(t, tc.config, tc.args...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), tc.want) {
			t.Errorf("%v: exit %v, output %s", tc.args, err, out)
		}
	}
}

func newCLIOAuthServer(t *testing.T, authenticated http.Handler, allowAnonymous ...func(*http.Request) bool) (*httptest.Server, string) {
	t.Helper()
	var origin string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": origin + "/mcp", "authorization_servers": []string{origin}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": origin, "authorization_endpoint": origin + "/authorize", "token_endpoint": origin + "/token",
			"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"}, "client_id_metadata_document_supported": true,
			"authorization_response_iss_parameter_supported": true,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("code") != "never-print-code" || r.FormValue("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"never-print-new-token","refresh_token":"never-print-refresh","token_type":"Bearer","expires_in":120}`))
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer never-print-new-token" &&
			(len(allowAnonymous) == 0 || !allowAnonymous[0](r)) {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+origin+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authenticated.ServeHTTP(w, r)
	})
	server := httptest.NewUnstartedServer(mux)
	origin = "https://" + server.Listener.Addr().String()
	server.StartTLS()
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "fixture-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return server, ca
}

func runCLILogin(t *testing.T, config, ca string, denied bool) (string, error) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	child := mcpAuthChild(t, config, "-mcp-auth", "login", "-mcp-server", "catalog", "-mcp-auth-listen", addr)
	child.Env = append(child.Env, "SSL_CERT_FILE="+ca)
	var stderr bytes.Buffer
	child.Stderr = &stderr
	pipe, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	var printed strings.Builder
	s := bufio.NewScanner(pipe)
	sent := false
	for s.Scan() {
		line := s.Text()
		printed.WriteString(line + "\n")
		if !strings.HasPrefix(line, "https://") || sent {
			continue
		}
		authURL, err := url.Parse(line)
		if err != nil {
			t.Fatal(err)
		}
		q := authURL.Query()
		callback := url.Values{"state": {q.Get("state")}, "iss": {authURL.Scheme + "://" + authURL.Host}}
		if denied {
			callback.Set("error", "access_denied")
		} else {
			callback.Set("code", "never-print-code")
		}
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(q.Get("redirect_uri") + "?" + callback.Encode())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		sent = true
	}
	err = child.Wait()
	waited = true
	if !sent {
		t.Fatalf("CLI did not print a sign-in URL: %v %s", err, stderr.String())
	}
	return printed.String() + stderr.String(), err
}

func TestMCPAuthLoginKeepsPreviousCredential(t *testing.T) {
	server, ca := newCLIOAuthServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "never-print-response-body", http.StatusBadGateway)
	}))
	config, binding := mcpAuthConfig(t, server.URL, false)
	store, err := mcpoauth.NewStore(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), mcpoauth.Record{Binding: binding, AccessToken: "seeded-token"}); err != nil {
		t.Fatal(err)
	}
	for _, denied := range []bool{true, false} {
		out, err := runCLILogin(t, config, ca, denied)
		if err == nil {
			t.Fatal("failed sign-in succeeded")
		}
		if strings.Contains(out, "never-print-") || strings.Contains(out, "seeded-token") {
			t.Fatal("CLI reported OAuth credentials or an upstream response body")
		}
		if got, err := store.Load(binding); err != nil || got.AccessToken != "seeded-token" {
			t.Fatalf("failed reauthorization changed the prior record: %v", err)
		}
	}
}

func TestMCPOAuthCheckProcess(t *testing.T) {
	var lists, calls, middlewareRequests atomic.Int32
	api := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	api.AddTool(&mcp.Tool{Name: "catalog_read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{}, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return api }, &mcp.StreamableHTTPOptions{Stateless: true})
	server, ca := newCLIOAuthServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		middlewareRequests.Add(1)
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var msg struct{ Method string }
			if json.Unmarshal(raw, &msg) == nil && msg.Method == "tools/list" {
				lists.Add(1)
			}
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	body, err := json.Marshal(map[string]any{"assistant_tools": map[string]any{"enabled": true, "mcp_servers": []any{
		map[string]any{"name": "catalog", "url": server.URL + "/mcp", "oauth": map[string]any{
			"client_id": "https://client.example.invalid/client.json", "issuer": server.URL, "scopes": []string{"catalog:read"},
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	config := configFile(t, string(body))
	c := mcpAuthChild(t, config, "-check-config")
	if out, err := c.CombinedOutput(); err != nil || !strings.Contains(string(out), "configuration is valid") ||
		lists.Load() != 0 || middlewareRequests.Load() != 0 {
		t.Fatalf("offline validation failed: %s, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(config), ".mcp-oauth")); !os.IsNotExist(err) {
		t.Fatalf("offline validation created OAuth storage: %v", err)
	}
	if out, err := runCLILogin(t, config, ca, false); err != nil || !strings.Contains(out, "MCP OAuth sign-in complete") || lists.Load() != 0 {
		t.Fatalf("sign-in or discovery isolation failed: %s, %v", out, err)
	}
	c = mcpAuthChild(t, config, "-check-mcp")
	c.Env = append(c.Env, "SSL_CERT_FILE="+ca)
	out, err := c.CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"tool_names":["catalog_read"]`) ||
		lists.Load() == 0 || calls.Load() != 0 || strings.Contains(string(out), "never-print-") {
		t.Fatalf("authenticated discovery failed or executed a tool: %s, %v", out, err)
	}
}

func TestSystemdOAuthWritableException(t *testing.T) {
	unit, err := os.ReadFile("../../scripts/quasar-bridge.service")
	if err != nil {
		t.Fatal(err)
	}
	const configDir = "/etc/quasar-bridge"
	paths := []string{}
	for line := range strings.SplitSeq(string(unit), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "ReadWritePaths="); ok {
			for _, p := range strings.Fields(value) {
				paths = append(paths, strings.TrimPrefix(p, "-"))
			}
		}
	}
	if !strings.Contains(string(unit), "ProtectSystem=strict\n") ||
		!strings.Contains(string(unit), "-config "+configDir+"/config.json\n") ||
		!slices.Equal(paths, []string{configDir + "/.mcp-oauth"}) {
		t.Fatalf("systemd must keep strict protection and allow writing only the OAuth state path, got %v", paths)
	}
}

func TestMCPAuthDisabledServerWithoutEndpointCanLogOut(t *testing.T) {
	path := configFile(t, `{"assistant_tools":{"enabled":true,"mcp_servers":[{"name":"catalog","disabled":true,"url_env":"QUASAR_OAUTH_TEST_REMOVED_URL","oauth":{"client_id":"https://client.example.invalid/client.json","issuer":"https://auth.example.invalid","scopes":["catalog:read"]}}]}}`)
	store, err := mcpoauth.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b := mcpoauth.Binding{Name: "catalog", Endpoint: "https://mcp.example.invalid/mcp",
		ClientID: "https://client.example.invalid/client.json", Issuer: "https://auth.example.invalid", Scopes: []string{"catalog:read"}}
	if err := store.Save(t.Context(), mcpoauth.Record{Binding: b, AccessToken: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if out, err := mcpAuthChild(t, path, "-mcp-auth", "logout", "-mcp-server", "catalog").CombinedOutput(); err != nil {
		t.Fatalf("disabled OAuth server could not log out after losing url_env: %s, %v", out, err)
	}
	if _, err := store.Load(b); !errors.Is(err, mcpoauth.ErrNotSignedIn) {
		t.Fatalf("logout did not remove the disabled server's credential: %v", err)
	}
}
