package mcpoauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func writeOAuthJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func TestOAuthHTTPGuards(t *testing.T) {
	var escaped atomic.Int32
	third := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped.Add(1)
	}))
	defer third.Close()
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Tenant") != "" {
			t.Error("MCP headers were forwarded to the authorization server")
		}
		http.Redirect(w, r, third.URL, http.StatusTemporaryRedirect)
	}))
	defer issuer.Close()
	resource := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, third.URL, http.StatusFound)
	}))
	defer resource.Close()
	client := newOAuthHTTP(Binding{Endpoint: resource.URL + "/mcp", Issuer: issuer.URL}, resource.Client().Transport)
	if _, err := client.Get(third.URL + "/.well-known/oauth-protected-resource"); err == nil {
		t.Fatal("metadata request to third origin was not rejected")
	}
	for _, target := range []string{resource.URL + "/.well-known/oauth-protected-resource", issuer.URL + "/.well-known/oauth-authorization-server"} {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer fixture-private")
		r.Header.Set("X-Tenant", "fixture-private")
		resp, err := client.Do(r)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 300 || resp.StatusCode >= 400 {
				t.Errorf("expected redirect was not rejected: %d", resp.StatusCode)
			}
		}
	}
	if n := escaped.Load(); n != 0 {
		t.Fatalf("OAuth requests escaped the trusted origins: %d", n)
	}
}
