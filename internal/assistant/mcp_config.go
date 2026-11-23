package assistant

import (
	"errors"
	"fmt"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// MCPServerConfig contains deployment settings, never credentials read from env.
type MCPServerConfig struct {
	Name                string            `json:"name"`
	Disabled            bool              `json:"disabled,omitzero"`
	Transport           string            `json:"transport,omitempty"`
	URL                 string            `json:"url,omitempty"`
	URLEnv              string            `json:"url_env,omitempty"`
	BearerTokenEnv      string            `json:"bearer_token_env,omitempty"`
	HeadersEnv          map[string]string `json:"headers_env,omitempty"`
	Command             string            `json:"command,omitempty"`
	Args                []string          `json:"args,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
	EnvFrom             map[string]string `json:"env_from,omitempty"`
	AllowedTools        []string          `json:"allowed_tools,omitempty"`
	DeniedTools         []string          `json:"denied_tools,omitempty"`
	AllowPrivateHistory bool              `json:"allow_private_history,omitzero"`
	TimeoutMS           int               `json:"timeout_ms,omitzero"`
	MaxResultChars      int               `json:"max_result_chars,omitzero"`
	OAuth               *MCPOAuthConfig   `json:"oauth,omitempty"`
}

type MCPOAuthConfig struct {
	ClientID string   `json:"client_id"`
	Issuer   string   `json:"issuer"`
	Scopes   []string `json:"scopes"`
}

var mcpServerName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,19}$`)
var mcpEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var mcpHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func (c MCPServerConfig) transport() string {
	if c.Transport != "" {
		return c.Transport
	}
	if c.Command != "" {
		return "stdio"
	}
	return "streamable_http"
}
func (c MCPServerConfig) timeout() time.Duration {
	if c.TimeoutMS == 0 {
		return 10 * time.Second
	}
	return time.Duration(c.TimeoutMS) * time.Millisecond
}
func (c MCPServerConfig) resultLimit() int {
	if c.MaxResultChars == 0 {
		return 12000
	}
	return c.MaxResultChars
}
func (c MCPServerConfig) endpoint() string {
	if c.URLEnv != "" {
		return os.Getenv(c.URLEnv)
	}
	return c.URL
}
func validateMCP(servers []MCPServerConfig) error {
	if len(servers) > 8 {
		return errors.New("at most 8 MCP servers are supported")
	}
	names := map[string]bool{}
	for i, c := range servers {
		fail := func(field string) error { return fmt.Errorf("invalid assistant_tools.mcp_servers[%d]: %s", i, field) }
		if !mcpServerName.MatchString(c.Name) || names[c.Name] {
			return fail("name must be unique and match [a-z][a-z0-9_-]{0,19}")
		}
		names[c.Name] = true
		if c.OAuth != nil {
			if err := validateMCPOAuth(c); err != nil {
				return fail(err.Error())
			}
		}
		if c.Disabled {
			continue
		}
		if c.TimeoutMS != 0 && (c.TimeoutMS < 100 || c.TimeoutMS > 60000) || c.MaxResultChars != 0 && (c.MaxResultChars < 100 || c.MaxResultChars > 20000) {
			return fail("invalid timeout_ms or max_result_chars")
		}
		if c.transport() == "stdio" {
			if c.Command == "" || c.URL != "" || c.URLEnv != "" || c.BearerTokenEnv != "" || len(c.HeadersEnv) > 0 || len(c.Args) > 64 || strings.ContainsRune(c.Command, 0) {
				return fail("stdio requires command/args and no HTTP settings")
			}
			for _, arg := range c.Args {
				if len(arg) > 16000 || strings.ContainsRune(arg, 0) {
					return fail("invalid command argument")
				}
			}
			for key, value := range c.Env {
				if !mcpEnvName.MatchString(key) || strings.ContainsRune(value, 0) {
					return fail("invalid env setting")
				}
			}
			for key, name := range c.EnvFrom {
				if !mcpEnvName.MatchString(key) || !mcpEnvName.MatchString(name) {
					return fail("invalid env_from setting")
				}
				if _, ok := os.LookupEnv(name); !ok {
					return fail("env_from variable is not set")
				}
			}
		} else {
			if c.transport() != "streamable_http" && c.transport() != "sse" || (c.URL == "") == (c.URLEnv == "") || c.Command != "" || len(c.Args)+len(c.Env)+len(c.EnvFrom) > 0 {
				return fail("HTTP transport requires exactly one of url/url_env and no stdio settings")
			}
			if c.URLEnv != "" && !mcpEnvName.MatchString(c.URLEnv) {
				return fail("invalid url_env name")
			}
			u, err := url.Parse(c.endpoint())
			if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
				return fail("invalid HTTP endpoint or unset url_env")
			}
			if c.BearerTokenEnv != "" && (!mcpEnvName.MatchString(c.BearerTokenEnv) || os.Getenv(c.BearerTokenEnv) == "" || strings.ContainsAny(os.Getenv(c.BearerTokenEnv), "\r\n")) {
				return fail("bearer_token_env is invalid or not set")
			}
			seenHeaders := map[string]bool{}
			for header, name := range c.HeadersEnv {
				h := textproto.CanonicalMIMEHeaderKey(header)
				if seenHeaders[h] || !mcpHeaderName.MatchString(header) || h == "Host" || h == "Content-Type" || h == "Accept" || strings.HasPrefix(strings.ToLower(h), "mcp-") || !mcpEnvName.MatchString(name) || os.Getenv(name) == "" || strings.ContainsAny(os.Getenv(name), "\r\n") || h == "Authorization" && c.BearerTokenEnv != "" {
					return fail("invalid or unset headers_env setting")
				}
				seenHeaders[h] = true
			}
		}
		for _, list := range [][]string{c.AllowedTools, c.DeniedTools} {
			seen := map[string]bool{}
			for _, name := range list {
				if name == "" || len(name) > 128 || seen[name] {
					return fail("invalid or repeated tool filter")
				}
				seen[name] = true
			}
		}
	}
	return nil
}

func validateMCPOAuth(c MCPServerConfig) error {
	if c.transport() != "streamable_http" {
		return errors.New("oauth requires streamable_http")
	}
	if c.BearerTokenEnv != "" {
		return errors.New("oauth cannot be combined with bearer_token_env")
	}
	for header := range c.HeadersEnv {
		if strings.EqualFold(header, "Authorization") {
			return errors.New("oauth cannot be combined with an Authorization header")
		}
	}
	httpsURL := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.Contains(raw, "#") {
			return nil
		}
		return u
	}
	endpoint := c.endpoint()
	// An operator may disable a server and remove its endpoint before logging out.
	disabledWithoutEnv := c.Disabled && c.URL == "" && mcpEnvName.MatchString(c.URLEnv) && endpoint == ""
	if !disabledWithoutEnv && httpsURL(endpoint) == nil {
		return errors.New("oauth requires a valid HTTPS MCP endpoint")
	}
	if u := httpsURL(c.OAuth.ClientID); u == nil || u.Path == "" || u.Path == "/" {
		return errors.New("oauth client_id must be a HTTPS URL with a document path")
	}
	if u := httpsURL(c.OAuth.Issuer); u == nil || u.RawQuery != "" || u.ForceQuery {
		return errors.New("oauth issuer must be a HTTPS URL without a query")
	}
	if len(c.OAuth.Scopes) == 0 {
		return errors.New("oauth requires explicit scopes")
	}
	scopes := make(map[string]bool, len(c.OAuth.Scopes))
	for _, scope := range c.OAuth.Scopes {
		if scope == "" || scopes[scope] {
			return errors.New("invalid or repeated oauth scope")
		}
		for _, r := range scope {
			if r < '!' || r > '~' || r == '"' || r == '\\' {
				return errors.New("invalid or repeated oauth scope")
			}
		}
		scopes[scope] = true
	}
	return nil
}
