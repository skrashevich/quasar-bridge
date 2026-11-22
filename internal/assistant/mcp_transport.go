package assistant

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Configured MCP origins can be private LAN services. Credentials and legacy
// SSE message posts must stay on the configured origin, including redirects.
type mcpHTTP struct {
	origin  *url.URL
	headers http.Header
}

func (t *mcpHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != t.origin.Scheme || r.URL.Host != t.origin.Host {
		return nil, errors.New("MCP cross-origin request blocked")
	}
	r = r.Clone(r.Context())
	for key, values := range t.headers {
		r.Header[key] = values
	}
	// Teardown must not hold up an otherwise complete spoken answer.
	if r.Method == http.MethodDelete {
		ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
		defer cancel()
		r = r.Clone(ctx)
	}
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, errors.New("MCP HTTP transport failed")
	}
	resp.Body = &mcpBody{Reader: io.LimitReader(resp.Body, 2<<20), Closer: resp.Body}
	return resp, nil
}

type mcpBody struct {
	io.Reader
	io.Closer
}

func openMCPSession(ctx context.Context, c MCPServerConfig) (*mcp.ClientSession, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(ctx)
	var transport mcp.Transport
	if c.transport() == "stdio" {
		// Only the local deployment config can select the executable and argv.
		// Tool-call arguments travel over MCP after the child starts.
		command := exec.CommandContext(ctx, c.Command, c.Args...) // nosemgrep: go_subproc_rule-subproc, go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
		command.WaitDelay = 200 * time.Millisecond
		// Server stderr can contain credentials; do not copy it to runtime logs.
		command.Stderr = io.Discard
		for _, name := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR", "TMP", "TEMP", "SYSTEMROOT", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
			if value, ok := os.LookupEnv(name); ok {
				command.Env = append(command.Env, name+"="+value)
			}
		}
		for name, value := range c.Env {
			command.Env = append(command.Env, name+"="+value)
		}
		for name, source := range c.EnvFrom {
			command.Env = append(command.Env, name+"="+os.Getenv(source))
		}
		transport = &mcp.CommandTransport{Command: command, TerminateDuration: 100 * time.Millisecond}
	} else {
		endpoint, _ := url.Parse(c.endpoint())
		headers := make(http.Header)
		if c.BearerTokenEnv != "" {
			headers.Set("Authorization", "Bearer "+os.Getenv(c.BearerTokenEnv))
		}
		for name, source := range c.HeadersEnv {
			headers.Set(name, os.Getenv(source))
		}
		client := &http.Client{Transport: &mcpHTTP{origin: endpoint, headers: headers}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if c.transport() == "sse" {
			transport = &mcp.SSEClientTransport{Endpoint: c.endpoint(), HTTPClient: client, MaxEventSize: 1 << 20}
		} else {
			transport = &mcp.StreamableClientTransport{Endpoint: c.endpoint(), HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: 1 << 20}
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "quasar-bridge", Version: "0.3.6"}, &mcp.ClientOptions{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Capabilities:   &mcp.ClientCapabilities{},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		cancel()
		return nil, func() {}, errors.New("MCP connection or initialization failed")
	}
	return session, cancel, nil
}
