package mcpoauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

const (
	DefaultListen  = "127.0.0.1:19876"
	callbackPath   = "/callback"
	ManualRedirect = "http://" + DefaultListen + callbackPath
	maxCallback    = 8192
)

var (
	ErrCallbackMismatch = errors.New("MCP OAuth return address does not match the current attempt")
	ErrCallbackUsed     = errors.New("MCP OAuth return address is no longer usable")
	ErrLoginDenied      = errors.New("MCP OAuth sign-in was declined")
)

type CallbackOutcome uint8

const (
	CallbackAccepted CallbackOutcome = iota
	CallbackDenied
)

type Attempt struct {
	b          Binding
	redirect   string
	publish    func(context.Context, string) error
	handler    *auth.AuthorizationCodeHandler
	httpPolicy *oauthHTTP
	started    atomic.Bool
	results    chan callbackResult
	mu         sync.Mutex
	state      string
	wait       context.Context
	completed  bool
	record     *Record
}

type callbackResult struct {
	auth.AuthorizationResult
	err error
}

func loopbackListen(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	n, numErr := strconv.ParseUint(port, 10, 16)
	if err != nil || numErr != nil || host != "127.0.0.1" || n < 1024 {
		return "", errors.New("MCP OAuth callback requires IPv4 loopback 127.0.0.1 and a port from 1024 to 65535")
	}
	return net.JoinHostPort(host, strconv.FormatUint(n, 10)), nil
}

func NewAttempt(b Binding, redirect string, publish func(context.Context, string) error, upstream http.RoundTripper) (*Attempt, error) {
	u, err := url.Parse(redirect)
	if err != nil || u == nil || publish == nil {
		return nil, errors.New("invalid MCP OAuth attempt")
	}
	address, err := loopbackListen(u.Host)
	if err != nil || redirect != "http://"+address+callbackPath {
		return nil, errors.New("invalid MCP OAuth return address")
	}
	client := newOAuthHTTP(b, upstream)
	l := &Attempt{b: b, redirect: redirect, publish: publish, httpPolicy: client.Transport.(*oauthHTTP), results: make(chan callbackResult, 1)}
	l.handler, err = auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &auth.ClientIDMetadataDocumentConfig{URL: b.ClientID},
		RedirectURL:                    redirect,
		AuthorizationCodeFetcher:       l.fetchCode,
		ScopeFilter:                    func([]string) []string { return slices.Clone(b.Scopes) },
		Client:                         client,
		NewTokenSource:                 l.captureToken,
	})
	if err != nil {
		return nil, errors.New("cannot configure MCP OAuth client")
	}
	return l, nil
}

func (l *Attempt) Handler() auth.OAuthHandler { return l }

func (l *Attempt) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if resp.StatusCode != http.StatusUnauthorized || !l.started.CompareAndSwap(false, true) {
		_ = resp.Body.Close()
		return errors.New("MCP OAuth initialization was not authorized")
	}
	if err := l.handler.Authorize(ctx, req, resp); err != nil {
		if errors.Is(err, ErrLoginDenied) {
			return ErrLoginDenied
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("MCP OAuth sign-in was declined or the server did not accept this client")
	}
	return nil
}

func (l *Attempt) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return l.handler.TokenSource(ctx)
}

func (l *Attempt) Record() (Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.record == nil {
		return Record{}, errors.New("MCP OAuth login did not issue a token")
	}
	return *l.record, nil
}

func (l *Attempt) captureToken(_ context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
	tokenURL, _ := url.Parse(cfg.Endpoint.TokenURL)
	if tok == nil || !strings.EqualFold(tok.Type(), "Bearer") || !validBearer(tok.AccessToken) ||
		!onOrigin(tokenURL, l.httpPolicy.issuer) || !grantedScopesAllowed(tok.Extra("scope"), l.b.Scopes) {
		return nil, errors.New("invalid MCP OAuth token response")
	}
	l.mu.Lock()
	l.record = &Record{Binding: l.b, TokenEndpoint: cfg.Endpoint.TokenURL, AccessToken: tok.AccessToken,
		RefreshToken: tok.RefreshToken, Expiry: tok.Expiry}
	l.mu.Unlock()
	return oauth2.StaticTokenSource(tok), nil
}

func validBearer(token string) bool {
	if token == "" {
		return false
	}
	for _, r := range token {
		if r < '!' || r > '~' {
			return false
		}
	}
	return true
}

func grantedScopesAllowed(value any, permitted []string) bool {
	if value == nil {
		return true // An omitted scope means the requested scope in OAuth 2.0.
	}
	scope, ok := value.(string)
	if !ok {
		return false
	}
	for _, token := range strings.Fields(scope) {
		if !slices.Contains(permitted, token) {
			return false
		}
	}
	return true
}

func (l *Attempt) fetchCode(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	u, err := url.Parse(args.URL)
	if err != nil || !onOrigin(u, l.httpPolicy.issuer) || !l.httpPolicy.resource.Load() ||
		!l.httpPolicy.metadata.Load() {
		return nil, errors.New("MCP OAuth authorization URL did not match the configured issuer")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || !l.validAuthorizationQuery(q) {
		return nil, errors.New("MCP OAuth authorization URL has invalid parameters")
	}
	l.mu.Lock()
	l.state = q.Get("state")
	l.wait = ctx
	l.mu.Unlock()
	if err := l.publish(ctx, args.URL); err != nil {
		return nil, errors.New("cannot display MCP OAuth sign-in link")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-l.results:
		if result.err != nil {
			return nil, result.err
		}
		return &result.AuthorizationResult, nil
	}
}

func (l *Attempt) validAuthorizationQuery(q url.Values) bool {
	for name, value := range map[string]string{
		"client_id": l.b.ClientID, "redirect_uri": l.redirect,
		"resource": l.b.Endpoint, "response_type": "code", "code_challenge_method": "S256",
	} {
		if len(q[name]) != 1 || q.Get(name) != value {
			return false
		}
	}
	for _, name := range []string{"state", "code_challenge", "scope"} {
		if len(q[name]) != 1 || q.Get(name) == "" {
			return false
		}
	}
	// A request object can override parameters in the URL; this client does not use one.
	return !q.Has("request") && !q.Has("request_uri") &&
		slices.Equal(sortedScopes(strings.Fields(q.Get("scope"))), sortedScopes(l.b.Scopes))
}

func (l *Attempt) Submit(raw string) (CallbackOutcome, error) {
	base, query, ok := strings.Cut(raw, "?")
	if !ok || len(raw) > maxCallback || base != l.redirect || strings.Contains(query, "#") {
		return 0, ErrCallbackMismatch
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return 0, ErrCallbackMismatch
	}
	for _, name := range []string{"state", "code", "iss", "error", "error_description", "error_uri"} {
		if len(q[name]) > 1 {
			return 0, ErrCallbackMismatch
		}
	}
	deny := q.Get("error") != ""
	if (q.Get("code") != "") == deny || (q.Has("error") && !deny) || (deny && q.Has("code")) {
		return 0, ErrCallbackMismatch
	}
	issuerRequired := l.httpPolicy.callbackIssuer.Load()
	if (issuerRequired && q.Get("iss") != l.b.Issuer) || (!issuerRequired && q.Has("iss")) {
		return 0, ErrCallbackMismatch
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == "" ||
		subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(l.state)) != 1 {
		return 0, ErrCallbackMismatch
	}
	if l.completed || l.wait.Err() != nil {
		return 0, ErrCallbackUsed
	}
	l.completed = true
	if deny {
		l.results <- callbackResult{err: ErrLoginDenied}
		return CallbackDenied, nil
	}
	l.results <- callbackResult{AuthorizationResult: auth.AuthorizationResult{
		Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss"),
	}}
	return CallbackAccepted, nil
}
