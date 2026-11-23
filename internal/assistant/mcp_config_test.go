package assistant

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestMCPValidateOAuth(t *testing.T) {
	const mcpURL = "https://mcp.example.invalid/mcp"
	const client = `"https://client.example.invalid/oauth/client.json"`
	const issuer = `"https://auth.example.invalid"`
	t.Setenv("MCP_OAUTH_TEST_URL", mcpURL)
	t.Setenv("MCP_OAUTH_TEST_HEADER", "private-fixture-header")
	for _, tc := range []struct {
		name, endpoint, client, issuer, scopes string
		wantErr                                bool
	}{
		{"explicit HTTPS", `"url":"` + mcpURL + `","transport":"streamable_http"`, client, issuer, `["read"]`, false},
		{"inferred HTTPS", `"url":"` + mcpURL + `"`, client, issuer, `["read"]`, false},
		{"environment and tenant", `"url_env":"MCP_OAUTH_TEST_URL","headers_env":{"X-Tenant":"MCP_OAUTH_TEST_HEADER"}`, client, issuer, `["read"]`, false},
		{"legacy SSE", `"url":"` + mcpURL + `","transport":"sse"`, client, issuer, `["read"]`, true},
		{"stdio", `"command":"fixture"`, client, issuer, `["read"]`, true},
		{"HTTP", `"url":"http://mcp.example.invalid/mcp"`, client, issuer, `["read"]`, true},
		{"MCP user info", `"url":"https://user:secret@mcp.example.invalid/mcp"`, client, issuer, `["read"]`, true},
		{"MCP fragment", `"url":"` + mcpURL + `#secret"`, client, issuer, `["read"]`, true},
		{"client no path", `"url":"` + mcpURL + `"`, `"https://client.example.invalid"`, issuer, `["read"]`, true},
		{"client root path", `"url":"` + mcpURL + `"`, `"https://client.example.invalid/"`, issuer, `["read"]`, true},
		{"client user info", `"url":"` + mcpURL + `"`, `"https://user:secret@client.example.invalid/client.json"`, issuer, `["read"]`, true},
		{"client fragment", `"url":"` + mcpURL + `"`, `"https://client.example.invalid/client.json#secret"`, issuer, `["read"]`, true},
		{"issuer user info", `"url":"` + mcpURL + `"`, client, `"https://user:secret@auth.example.invalid"`, `["read"]`, true},
		{"issuer fragment", `"url":"` + mcpURL + `"`, client, `"https://auth.example.invalid#secret"`, `["read"]`, true},
		{"issuer query", `"url":"` + mcpURL + `"`, client, `"https://auth.example.invalid?secret=x"`, `["read"]`, true},
		{"missing scope", `"url":"` + mcpURL + `"`, client, issuer, `[]`, true},
		{"duplicate scope", `"url":"` + mcpURL + `"`, client, issuer, `["read","read"]`, true},
		{"scope with whitespace", `"url":"` + mcpURL + `"`, client, issuer, `["read write"]`, true},
		{"static bearer", `"url":"` + mcpURL + `","bearer_token_env":"MCP_OAUTH_TEST_HEADER"`, client, issuer, `["read"]`, true},
		{"mixed-case authorization", `"url":"` + mcpURL + `","headers_env":{"aUtHoRiZaTiOn":"MCP_OAUTH_TEST_HEADER"}`, client, issuer, `["read"]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"name":"catalog",` + tc.endpoint + `,"oauth":{"client_id":` + tc.client + `,"issuer":` + tc.issuer + `,"scopes":` + tc.scopes + `}}`
			var server MCPServerConfig
			if err := json.Unmarshal([]byte(raw), &server); err != nil {
				t.Fatal(err)
			}
			err := validateMCP([]MCPServerConfig{server})
			if (err != nil) != tc.wantErr {
				t.Fatalf("validation = %v; want error: %v", err, tc.wantErr)
			}
			if err != nil && (strings.Contains(err.Error(), "https://") || strings.Contains(err.Error(), "private-fixture-header")) {
				t.Fatalf("validation exposed config values: %v", err)
			}
		})
	}
}

func TestMCPConfigurationValidation(t *testing.T) {
	t.Setenv("MCP_URL_FIXTURE", "https://mcp.example.invalid/mcp/private-capability")
	t.Setenv("MCP_TOKEN_FIXTURE", "fixture-token")
	for _, valid := range []MCPServerConfig{
		{Name: "http", URL: "https://mcp.example.invalid/mcp"},
		{Name: "sse", Transport: "sse", URLEnv: "MCP_URL_FIXTURE", HeadersEnv: map[string]string{"X-API-Key": "MCP_TOKEN_FIXTURE"}},
		{Name: "local", Command: "fixture", Args: []string{"--stdio"}, EnvFrom: map[string]string{"TOKEN": "MCP_TOKEN_FIXTURE"}},
		{Name: "disabled", Disabled: true},
	} {
		if err := validateMCP([]MCPServerConfig{valid}); err != nil {
			t.Fatal(valid.Name, err)
		}
	}
	for _, invalid := range []MCPServerConfig{
		{Name: "UPPER", URL: "https://mcp.example.invalid/mcp"},
		{Name: "x", Transport: "unknown", URL: "https://mcp.example.invalid/mcp"},
		{Name: "x", URL: "https://user:password@mcp.example.invalid/mcp"},
		{Name: "x", URLEnv: "UNSET_MCP_URL_FIXTURE"},
		{Name: "x", URL: "https://mcp.example.invalid/mcp", URLEnv: "MCP_URL_FIXTURE"},
		{Name: "x", Transport: "stdio"},
		{Name: "x", Command: "fixture", URL: "https://mcp.example.invalid/mcp"},
		{Name: "x", URL: "https://mcp.example.invalid/mcp", TimeoutMS: -1},
		{Name: "x", URL: "https://mcp.example.invalid/mcp", BearerTokenEnv: "UNSET_MCP_TOKEN_FIXTURE"},
		{Name: "x", URL: "https://mcp.example.invalid/mcp", HeadersEnv: map[string]string{"MCP-Session-ID": "MCP_TOKEN_FIXTURE"}},
		{Name: "x", Command: "fixture", EnvFrom: map[string]string{"TOKEN": "UNSET_MCP_TOKEN_FIXTURE"}},
	} {
		if err := validateMCP([]MCPServerConfig{invalid}); err == nil {
			t.Fatal("invalid MCP configuration accepted", invalid.Name)
		}
	}
	cfg := DefaultConfig()
	cfg.MCPServers = []MCPServerConfig{{Name: "one", URL: "https://mcp.example.invalid/mcp"}}
	if cfg.Validate() == nil {
		t.Fatal("MCP silently enabled while assistant tools are disabled")
	}
	cfg.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.MCPServers = append(cfg.MCPServers, cfg.MCPServers[0])
	if cfg.Validate() == nil {
		t.Fatal("duplicate server namespace accepted")
	}
}
