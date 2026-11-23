package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/skrashevich/quasar-bridge/internal/mcpoauth"
)

// Configured MCP origins can be private LAN services. Credentials and legacy
// SSE message posts must stay on the configured origin, including redirects.
type mcpHTTP struct {
	origin  *url.URL
	headers http.Header
	token   func(context.Context) (string, error)
	login   context.Context
}

func (t *mcpHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	// The SDK can send a best-effort cancellation notification with a detached
	// context. Do not start another HTTP request after interactive login ends.
	if t.login != nil && t.login.Err() != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, t.login.Err()
	}
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
	if t.token != nil {
		token, err := t.token(r.Context())
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, errors.New("MCP HTTP transport failed")
	}
	if t.token != nil && resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		return nil, mcpoauth.ErrSignInRequired
	}
	resp.Body = &mcpBody{Reader: io.LimitReader(resp.Body, 2<<20), Closer: resp.Body}
	return resp, nil
}

type mcpBody struct {
	io.Reader
	io.Closer
}

type handshakeOAuth struct{ auth.OAuthHandler }

func (h handshakeOAuth) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	// A token obtained in response to notifications/initialized cannot
	// authenticate the already completed handshake.
	if req.Method == http.MethodPost && req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			defer body.Close()
			var msg struct{ Method string }
			if json.NewDecoder(io.LimitReader(body, 64<<10)).Decode(&msg) == nil &&
				(msg.Method == "initialize" || msg.Method == "server/discover") {
				return h.OAuthHandler.Authorize(ctx, req, resp)
			}
		}
	}
	_ = resp.Body.Close()
	return errors.New("MCP OAuth requires an authenticated handshake")
}

func openMCPSession(ctx context.Context, c MCPServerConfig) (*mcp.ClientSession, context.CancelFunc, error) {
	return openMCPSessionWithOAuth(ctx, c, nil, nil)
}

func openMCPSessionWithOAuth(ctx context.Context, c MCPServerConfig, handler auth.OAuthHandler, token func(context.Context) (string, error)) (*mcp.ClientSession, context.CancelFunc, error) {
	if handler != nil && token != nil {
		return nil, func() {}, errors.New("conflicting MCP OAuth handlers")
	}
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
		httpTransport := &mcpHTTP{origin: endpoint, headers: headers, token: token}
		if handler != nil {
			httpTransport.login = ctx
			handler = handshakeOAuth{handler}
		}
		client := &http.Client{Transport: httpTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if c.transport() == "sse" {
			transport = &mcp.SSEClientTransport{Endpoint: c.endpoint(), HTTPClient: client, MaxEventSize: 1 << 20}
		} else {
			transport = &mcp.StreamableClientTransport{Endpoint: c.endpoint(), HTTPClient: client, OAuthHandler: handler, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: 1 << 20}
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
		return nil, func() {}, mcpFailure(err, "MCP connection or initialization failed")
	}
	return session, cancel, nil
}

func mcpFailure(err error, fallback string) error {
	for _, safe := range []error{mcpoauth.ErrSignInRequired, mcpoauth.ErrRefreshUnavailable, mcpoauth.ErrLoginDenied} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return errors.New(fallback)
}
