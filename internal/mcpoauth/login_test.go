package mcpoauth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func freeLoginAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

type loginOutput chan string

func (w loginOutput) Write(p []byte) (int, error) {
	for line := range strings.SplitSeq(string(p), "\n") {
		if strings.HasPrefix(line, "https://") {
			w <- line
		}
	}
	return len(p), nil
}

type loginFixtureOptions struct {
	authorizeQuery string
	methods        []string
	grantedScope   string
}

func loginFixture(t *testing.T, options loginFixtureOptions) (*httptest.Server, Binding) {
	t.Helper()
	methods := options.methods
	if methods == nil {
		methods = []string{"S256"}
	}
	var origin string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeOAuthJSON(w, map[string]any{"resource": origin + "/mcp", "authorization_servers": []string{origin}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeOAuthJSON(w, map[string]any{
			"issuer": origin, "authorization_endpoint": origin + "/authorize" + options.authorizeQuery, "token_endpoint": origin + "/token",
			"response_types_supported": []string{"code"}, "code_challenge_methods_supported": methods,
			"token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": []string{"catalog:read", "catalog:write", "offline_access"},
			"client_id_metadata_document_supported": true, "authorization_response_iss_parameter_supported": true,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.FormValue("code") != "fixture-code" ||
			r.FormValue("client_id") != "https://client.example.invalid/client.json" ||
			r.FormValue("resource") != origin+"/mcp" || r.FormValue("code_verifier") == "" ||
			r.FormValue("client_secret") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("invalid public token exchange: client ID present=%t, resource present=%t, PKCE verifier present=%t, authorization header present=%t",
				r.FormValue("client_id") != "", r.FormValue("resource") != "", r.FormValue("code_verifier") != "", r.Header.Get("Authorization") != "")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply := map[string]any{
			"access_token": "fixture-access", "refresh_token": "fixture-refresh", "token_type": "Bearer", "expires_in": 60,
		}
		if options.grantedScope != "" {
			reply["scope"] = options.grantedScope
		}
		writeOAuthJSON(w, reply)
	})
	server := httptest.NewUnstartedServer(mux)
	origin = "https://" + server.Listener.Addr().String()
	server.StartTLS()
	t.Cleanup(server.Close)
	binding := Binding{Name: "catalog", Endpoint: origin + "/mcp", Issuer: origin,
		ClientID: "https://client.example.invalid/client.json", Scopes: []string{"catalog:read"}}
	return server, binding
}

func prepareFixtureLogin(t *testing.T, ctx context.Context, options loginFixtureOptions) (*Login, Binding, loginOutput, <-chan error, string) {
	t.Helper()
	server, binding := loginFixture(t, options)
	addr := freeLoginAddress(t)
	out := make(loginOutput, 1)
	l, err := NewLogin(binding, addr, out, server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	done := make(chan error, 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, binding.Endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{StatusCode: 401, Body: http.NoBody, Header: make(http.Header)}
	resp.Header.Set("WWW-Authenticate", `Bearer resource_metadata="`+binding.Issuer+`/.well-known/oauth-protected-resource", scope="catalog:read catalog:write"`)
	go func() { done <- l.Handler().Authorize(ctx, req, resp) }()
	return l, binding, out, done, addr
}

func startFixtureLogin(t *testing.T, ctx context.Context, options loginFixtureOptions) (*Login, Binding, *url.URL, <-chan error, string) {
	t.Helper()
	l, binding, out, done, addr := prepareFixtureLogin(t, ctx, options)
	select {
	case line := <-out:
		u, err := url.Parse(line)
		if err != nil {
			t.Fatal(err)
		}
		return l, binding, u, done, addr
	case err := <-done:
		t.Fatalf("login did not start: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return nil, Binding{}, nil, nil, ""
}

func sendFixtureCallback(t *testing.T, address string, params url.Values) int {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(address + "?" + params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestOAuthLoginCIMD(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	l, binding, authURL, done, addr := startFixtureLogin(t, ctx, loginFixtureOptions{})
	if _, err := l.Record(); err == nil {
		t.Fatal("credential available before OAuth exchange")
	}
	q := authURL.Query()
	for key, want := range map[string]string{
		"client_id": binding.ClientID, "code_challenge_method": "S256", "resource": binding.Endpoint,
		"scope": "catalog:read", "redirect_uri": "http://" + addr + "/callback",
	} {
		if q.Get(key) != want {
			t.Errorf("authorize %s = %q, want %q", key, q.Get(key), want)
		}
	}
	if status := sendFixtureCallback(t, q.Get("redirect_uri"), url.Values{
		"state": {q.Get("state")}, "code": {"fixture-code"}, "iss": {binding.Issuer},
	}); status != http.StatusOK {
		t.Fatalf("callback status = %d", status)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r, err := l.Record()
	if err != nil || r.AccessToken != "fixture-access" || r.RefreshToken != "fixture-refresh" || r.TokenEndpoint != binding.Issuer+"/token" {
		t.Fatalf("wrong credential record: %v", err)
	}
}

func TestOAuthLoginRejectsInvalidCallback(t *testing.T) {
	for _, kind := range []string{"state", "issuer", "route"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			l, binding, authURL, done, addr := startFixtureLogin(t, ctx, loginFixtureOptions{})
			q := authURL.Query()
			values := url.Values{"state": {q.Get("state")}, "code": {"fixture-code"}, "iss": {binding.Issuer}}
			target := q.Get("redirect_uri")
			switch kind {
			case "state":
				values.Set("state", "not-the-issued-state")
			case "issuer":
				values.Set("iss", "https://other.example.invalid")
			case "route":
				target = "http://" + addr + "/elsewhere"
			}
			status := sendFixtureCallback(t, target, values)
			if status < 400 {
				t.Fatalf("invalid callback accepted: HTTP %d", status)
			}
			cancel()
			if err := <-done; err == nil {
				t.Fatal("invalid callback authorized the client")
			}
			if _, err := l.Record(); err == nil {
				t.Fatal("invalid callback produced credentials")
			}
		})
	}
}

func TestOAuthLoginCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	l, _, _, done, _ := startFixtureLogin(t, ctx, loginFixtureOptions{})
	if err := <-done; err == nil || ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("expired login did not stop: %v", err)
	}
	if _, err := l.Record(); err == nil {
		t.Fatal("expired login produced credentials")
	}
}

func TestOAuthLoginNoLinkForUnsafeIssuerMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts loginFixtureOptions
	}{
		{"pre-existing scope", loginFixtureOptions{authorizeQuery: "?scope=catalog%3Awrite"}},
		{"pre-existing redirect", loginFixtureOptions{authorizeQuery: "?redirect_uri=https%3A%2F%2Felsewhere.example.invalid"}},
		{"request object reference", loginFixtureOptions{authorizeQuery: "?request_uri=https%3A%2F%2Felsewhere.example.invalid"}},
		{"no S256", loginFixtureOptions{methods: []string{"plain"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			l, _, out, done, _ := prepareFixtureLogin(t, ctx, tc.opts)
			select {
			case <-out:
				t.Fatal("printed an authorization URL from unusable issuer metadata")
			case err := <-done:
				if err == nil {
					t.Fatal("unsafe authorization options were accepted")
				}
			case <-ctx.Done():
				t.Fatal("invalid issuer metadata was not rejected promptly")
			}
			if _, err := l.Record(); err == nil {
				t.Fatal("created credentials after invalid issuer metadata")
			}
		})
	}
}

func TestOAuthLoginRejectsBroaderGrantedScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	l, binding, authURL, done, _ := startFixtureLogin(t, ctx, loginFixtureOptions{grantedScope: "catalog:read catalog:write"})
	q := authURL.Query()
	sendFixtureCallback(t, q.Get("redirect_uri"), url.Values{
		"state": {q.Get("state")}, "code": {"fixture-code"}, "iss": {binding.Issuer},
	})
	if err := <-done; err == nil {
		t.Fatal("accepted a token with an unrequested write scope")
	}
	if _, err := l.Record(); err == nil {
		t.Fatal("retained a credential with an unrequested write scope")
	}
}
