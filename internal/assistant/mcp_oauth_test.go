//go:build linux || darwin

package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"quasarbridge/internal/mcpoauth"
)

func TestMCPOAuthRequestSafety(t *testing.T) {
	t.Setenv("MCP_OAUTH_TENANT_TEST", "fixture-tenant")
	var mcpRequests, toolCalls, refreshCalls atomic.Int32
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcpServer.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			t.Error("fixture tool should have been stopped by HTTP 401")
			return &mcp.CallToolResult{}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, &mcp.StreamableHTTPOptions{Stateless: true})
	mcpAPI := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mcpRequests.Add(1)
		if r.Header.Get("Authorization") != "Bearer refreshed-fixture" || r.Header.Get("X-Tenant") != "fixture-tenant" {
			t.Error("MCP request missed OAuth or configured tenant header")
		}
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var request struct{ Method string }
			if json.Unmarshal(raw, &request) == nil && request.Method == "tools/call" {
				toolCalls.Add(1)
				http.Error(w, "never-print-upstream-body", http.StatusUnauthorized)
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer mcpAPI.Close()
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Tenant") != "" ||
			r.FormValue("resource") != mcpAPI.URL+"/mcp" {
			t.Error("issuer received MCP headers or wrong resource")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"refreshed-fixture","token_type":"Bearer","expires_in":120}`))
	}))
	defer issuer.Close()
	// The assistant tests are serial; use the httptest CA instead of disabling HTTPS verification.
	oldTransport := http.DefaultTransport
	http.DefaultTransport = mcpAPI.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()

	anonymous := httptest.NewServer(handler)
	defer anonymous.Close()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := mcpoauth.NewStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	oauthConfig := MCPServerConfig{Name: "catalog", URL: mcpAPI.URL + "/mcp",
		OAuth:      &MCPOAuthConfig{ClientID: "https://client.example.invalid/client.json", Issuer: issuer.URL, Scopes: []string{"catalog:read"}},
		HeadersEnv: map[string]string{"X-Tenant": "MCP_OAUTH_TENANT_TEST"}}
	source := &mcpoauth.Source{Store: store, HTTP: issuer.Client().Transport}
	cfg := []MCPServerConfig{oauthConfig, {Name: "public", URL: anonymous.URL}}
	status, err := CheckMCP(t.Context(), cfg, source)
	if err == nil || len(status) != 2 || !strings.Contains(status[0].Error, "sign-in required; run -mcp-auth login") ||
		!slices.Equal(status[1].ToolNames, []string{"echo"}) || mcpRequests.Load() != 0 {
		t.Fatalf("missing credentials affected the wrong server or triggered a request: %+v, %v", status, err)
	}
	if err := store.Save(t.Context(), mcpoauth.Record{Binding: oauthConfig.oauthBinding(), AccessToken: "expired-fixture",
		RefreshToken: "refresh-fixture", Expiry: time.Now().Add(-time.Hour), TokenEndpoint: issuer.URL + "/token"}); err != nil {
		t.Fatal(err)
	}
	tools, closeTools := (&Tools{Config: Config{MCPServers: cfg}, MCPOAuth: source}).ForRequest(t.Context())
	defer closeTools()
	_, err = tools.Call(t.Context(), mcpAlias("catalog", "echo"), "{}")
	if err == nil || !strings.Contains(err.Error(), "sign-in required") ||
		strings.Contains(err.Error(), "never-print-upstream-body") || toolCalls.Load() != 1 || refreshCalls.Load() != 1 {
		t.Fatalf("MCP tool 401 replayed or mishandled: tool calls=%d, refreshes=%d, err=%v", toolCalls.Load(), refreshCalls.Load(), err)
	}
}
