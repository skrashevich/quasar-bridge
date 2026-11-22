package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"uuid"
)

var (
	ErrLoginDenied    = errors.New("ChatGPT authorization was denied; no credentials were changed")
	ErrPlanPermission = errors.New("ChatGPT plan permission was not granted; authorize plan usage in ChatGPT settings")
)

// LoginAttempt keeps the PKCE verifier and identity nonce on the Bridge.
// The caller owns the attempt lifetime and must consume a matching callback once.
type LoginAttempt struct {
	store        *Store
	initial      credentials
	redirect     string
	state        string
	nonce        string
	verifier     string
	clientID     string
	authorizeURL string
}

// BeginLogin uses the same loopback redirect for the CLI and the panel. The
// public port can be the local end of an SSH forward to the panel.
func (s *Store) BeginLogin(ctx context.Context, port uint16) (*LoginAttempt, error) {
	if port == 0 {
		return nil, errors.New("invalid ChatGPT callback port")
	}
	initial, err := s.prepareHost(ctx)
	if err != nil {
		return nil, err
	}
	host := "127.0.0.1"
	if port != 80 {
		host = net.JoinHostPort(host, strconv.Itoa(int(port)))
	}
	redirect := "http://" + host + "/auth/callback"
	state, nonce, verifier := rand.Text(), rand.Text(), rand.Text()+rand.Text()
	digest := sha256.Sum256([]byte(verifier))
	clientID := initial.ClientID
	if clientID == "" {
		clientID = dynamicClient
	}
	query := url.Values{
		"client_id": {clientID}, "ext_agent_host_id": {initial.HostID},
		"response_type": {"code"}, "redirect_uri": {redirect},
		"scope":    {"openid profile email offline_access resource.invoke " + planScope},
		"resource": {resource}, "state": {state}, "nonce": {nonce},
		"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])},
	}
	if clientID == dynamicClient {
		query.Set("agent_name_hint", "Quasar Bridge")
	}
	endpoint := s.authorizeURL
	if endpoint == "" {
		endpoint = issuer + "/api/accounts/authorize"
	}
	initial.clearTokens()
	return &LoginAttempt{
		store: s, initial: initial, redirect: redirect, state: state, nonce: nonce,
		verifier: verifier, clientID: clientID, authorizeURL: endpoint + "?" + query.Encode(),
	}, nil
}

func (a *LoginAttempt) AuthorizationURL() string { return a.authorizeURL }

// Matches binds a browser return (including a pasted link) to the exact
// redirect URI and state issued for this attempt. The URL is never fetched.
func (a *LoginAttempt) Matches(raw string) bool {
	base, query, ok := strings.Cut(raw, "?")
	if !ok || base != a.redirect || strings.Contains(query, "#") {
		return false
	}
	values, err := url.ParseQuery(query)
	return err == nil && len(values["state"]) == 1 && subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(a.state)) == 1
}

// Login starts a protected loopback callback and supplies an authorization URL
// to the caller. It never opens a browser automatically or prints retained ID
// tokens. The caller controls cancellation and how the URL is presented.
func (s *Store) Login(ctx context.Context, address string, showURL func(string) error) error {
	if address == "" {
		address = "127.0.0.1:1455"
	}
	host, port, err := net.SplitHostPort(address)
	n, numberErr := strconv.Atoi(port)
	if err != nil || host != "127.0.0.1" || numberErr != nil || n < 0 || n > 65535 {
		return errors.New("ChatGPT callback must listen on 127.0.0.1 with a valid port")
	}
	if showURL == nil {
		return errors.New("ChatGPT login requires an authorization URL handler")
	}
	if ctx.Err() != nil {
		return fmt.Errorf("ChatGPT login: %w", ctx.Err())
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return errors.New("cannot start ChatGPT callback listener on 127.0.0.1; choose another port")
	}
	defer listener.Close()
	attempt, err := s.BeginLogin(ctx, uint16(listener.Addr().(*net.TCPAddr).Port))
	if err != nil {
		return err
	}
	results := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		raw := attempt.redirect + "?" + r.URL.RawQuery
		if !attempt.Matches(raw) {
			http.Error(w, "Invalid sign-in state.", http.StatusBadRequest)
			return
		}
		select {
		case results <- raw:
			fmt.Fprintln(w, "Sign-in response received. Return to the terminal to check the result.")
		default:
			http.Error(w, "Sign-in callback already received.", http.StatusConflict)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	defer server.Close()
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	if err := showURL(attempt.AuthorizationURL()); err != nil {
		return errors.New("cannot present ChatGPT authorization URL")
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("ChatGPT login: %w", ctx.Err())
	case <-serveErrors:
		return errors.New("ChatGPT callback listener stopped")
	case raw := <-results:
		return attempt.Complete(ctx, raw)
	}
}

func (a *LoginAttempt) Complete(ctx context.Context, raw string) error {
	if !a.Matches(raw) {
		return errors.New("invalid ChatGPT callback address or state")
	}
	_, query, _ := strings.Cut(raw, "?")
	values, _ := url.ParseQuery(query)
	code, clientID := values.Get("code"), values.Get("client_id")
	switch {
	case values.Has("error"):
		return ErrLoginDenied
	case code == "" || len(values["code"]) != 1:
		return errors.New("ChatGPT callback did not contain an authorization code")
	case a.clientID == dynamicClient && (clientID == "" || clientID == dynamicClient):
		return errors.New("ChatGPT registration did not return an issued client ID")
	case a.clientID != dynamicClient && clientID != "" && clientID != a.clientID:
		return errors.New("ChatGPT callback changed the selected account registration")
	case a.clientID != dynamicClient:
		clientID = a.clientID
	}
	s, initial := a.store, a.initial
	if initial.ClientID == "" {
		// Registration survives a failed code exchange or missing plan grant.
		// No token or unverified account identity is made active here.
		if err := s.retainRegistration(ctx, initial, clientID); err != nil {
			return err
		}
		initial.ClientID = clientID
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {a.verifier}, "redirect_uri": {a.redirect}, "resource": {resource}}
	token, err := s.exchange(ctx, form)
	if err != nil {
		return err
	}
	identity, err := s.validateIDToken(ctx, token.IDToken, clientID, a.nonce)
	if err != nil {
		return err
	}
	if !hasPlanScope(token.Scope) {
		return ErrPlanPermission
	}
	if initial.Subject != "" && identity.Subject != initial.Subject {
		return errors.New("ChatGPT sign-in identity differs from the selected account; choose a different credential file")
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := s.load()
	if err != nil {
		return err
	}
	if current.HostID != initial.HostID || current.ClientID != initial.ClientID || current.Subject != initial.Subject {
		return errors.New("ChatGPT registration changed during sign-in; try again")
	}
	current.Issuer, current.Subject, current.Email = issuer, identity.Subject, identity.Email
	current.ClientID = clientID
	current.AccessToken, current.RefreshToken, current.IDToken = token.AccessToken, token.RefreshToken, token.IDToken
	current.Scope = token.Scope
	current.ExpiresAt = time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second)
	return s.save(current)
}

func (s *Store) retainRegistration(ctx context.Context, initial credentials, clientID string) error {
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := s.load()
	if err != nil {
		return err
	}
	if current.HostID != initial.HostID || current.ClientID != initial.ClientID || current.Subject != initial.Subject {
		return errors.New("ChatGPT registration changed during sign-in; try again")
	}
	current.ClientID = clientID
	return s.save(current)
}

func (s *Store) prepareHost(ctx context.Context) (credentials, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return credentials{}, err
	}
	defer unlock()
	c, err := s.load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return credentials{}, err
	}
	if err == nil {
		if !strings.HasPrefix(c.HostID, "quasar-bridge-") {
			return c, nil
		}
		// The initial implementation generated a format OpenAI rejects. Repair
		// only an untouched pre-registration record; an issued registration or
		// account identity must never acquire a new host lifecycle implicitly.
		if c.ClientID != "" || c.Subject != "" || c.Issuer != "" || c.Email != "" || c.AccessToken != "" || c.RefreshToken != "" || c.IDToken != "" || c.Scope != "" || !c.ExpiresAt.IsZero() {
			return credentials{}, errors.New("unsupported legacy ChatGPT host ID belongs to an existing registration; use a separate credential file to register this host again")
		}
	} else {
		c = credentials{Version: 1}
	}
	c.HostID = "urn:uuid:" + uuid.NewV4().String()
	if err := s.save(c); err != nil {
		return credentials{}, err
	}
	return c, nil
}
