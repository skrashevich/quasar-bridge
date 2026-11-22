package mcpoauth

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

const maxOAuthResponseBytes = 1 << 20

type oauthHTTP struct {
	b              Binding
	mcp            *url.URL
	issuer         *url.URL
	upstream       http.RoundTripper
	resource       atomic.Bool
	metadata       atomic.Bool
	callbackIssuer atomic.Bool
}

func newOAuthHTTP(b Binding, upstream http.RoundTripper) *http.Client {
	mcp, _ := url.Parse(b.Endpoint)
	issuer, _ := url.Parse(b.Issuer)
	if upstream == nil {
		upstream = http.DefaultTransport
	}
	return &http.Client{
		Transport: &oauthHTTP{b: b, mcp: mcp, issuer: issuer, upstream: upstream},
		Timeout:   15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func onOrigin(u, origin *url.URL) bool {
	return u != nil && origin != nil && u.Scheme == "https" && u.Host != "" &&
		strings.EqualFold(u.Host, origin.Host) && u.User == nil && u.Fragment == ""
}

func (h *oauthHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		defer r.Body.Close()
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	switch r.Method {
	case http.MethodGet:
		if !onOrigin(r.URL, h.mcp) && !onOrigin(r.URL, h.issuer) {
			return nil, errors.New("MCP OAuth metadata request left the configured origins")
		}
	case http.MethodPost:
		if !onOrigin(r.URL, h.issuer) {
			return nil, errors.New("MCP OAuth token request left the issuer origin")
		}
	default:
		return nil, errors.New("unsupported MCP OAuth request method")
	}
	req := r.Clone(r.Context())
	req.Host = ""
	req.Header = make(http.Header)
	for _, key := range []string{"Accept", "Content-Type"} {
		if value := r.Header.Get(key); value != "" {
			req.Header.Set(key, value)
		}
	}
	if r.Method == http.MethodPost {
		if err := h.publicTokenForm(req); err != nil {
			return nil, err
		}
	}
	resp, err := h.upstream.RoundTrip(req)
	if err != nil {
		return nil, errors.New("MCP OAuth HTTP request failed")
	}
	if r.Method != http.MethodGet || resp.StatusCode != http.StatusOK {
		resp.Body = &boundedOAuthBody{Reader: io.LimitReader(resp.Body, maxOAuthResponseBytes+1), Closer: resp.Body}
		return resp, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes+1))
	_ = resp.Body.Close()
	if err != nil || len(body) > maxOAuthResponseBytes || h.checkMetadata(r.URL, body) != nil {
		return nil, errors.New("MCP OAuth metadata did not match the configured resource or issuer")
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

func (h *oauthHTTP) publicTokenForm(req *http.Request) error {
	invalid := errors.New("invalid MCP OAuth token request")
	if req.Body == nil || req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		return invalid
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxOAuthResponseBytes+1))
	form, parseErr := url.ParseQuery(string(body))
	if err != nil || len(body) > maxOAuthResponseBytes || parseErr != nil || form.Has("client_secret") ||
		len(form["resource"]) != 1 || form.Get("resource") != h.b.Endpoint ||
		len(form["client_id"]) > 1 || (form.Has("client_id") && form.Get("client_id") != h.b.ClientID) {
		return invalid
	}
	// SDK v1.8.0 lets oauth2 probe Basic auth for CIMD. Never forward that
	// header; a public client sends the configured ID in its first form.
	form.Set("client_id", h.b.ClientID)
	encoded := form.Encode()
	req.Body = io.NopCloser(strings.NewReader(encoded))
	req.GetBody = nil
	req.ContentLength = int64(len(encoded))
	return nil
}

type boundedOAuthBody struct {
	io.Reader
	io.Closer
}

func (h *oauthHTTP) checkMetadata(source *url.URL, body []byte) error {
	var doc struct {
		Resource                    string   `json:"resource"`
		Authorization               []string `json:"authorization_servers"`
		Issuer                      string   `json:"issuer"`
		AuthorizeURL                string   `json:"authorization_endpoint"`
		TokenURL                    string   `json:"token_endpoint"`
		ChallengeMethods            []string `json:"code_challenge_methods_supported"`
		AuthorizationResponseIssuer bool     `json:"authorization_response_iss_parameter_supported"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	mismatch := errors.New("MCP OAuth metadata mismatch")
	switch {
	case doc.Resource != "" && doc.Issuer == "":
		if !onOrigin(source, h.mcp) || doc.Resource != h.b.Endpoint ||
			len(doc.Authorization) == 0 || doc.Authorization[0] != h.b.Issuer {
			return mismatch
		}
		h.resource.Store(true)
	case doc.Issuer != "" && doc.Resource == "":
		authorize, _ := url.Parse(doc.AuthorizeURL)
		token, _ := url.Parse(doc.TokenURL)
		if !onOrigin(source, h.issuer) || doc.Issuer != h.b.Issuer ||
			!onOrigin(authorize, h.issuer) || !onOrigin(token, h.issuer) ||
			!slices.Contains(doc.ChallengeMethods, "S256") {
			return mismatch
		}
		h.callbackIssuer.Store(doc.AuthorizationResponseIssuer)
		h.metadata.Store(true)
	default:
		return mismatch
	}
	return nil
}
