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

type callbackResult struct {
	code     string
	clientID string
	err      error
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
	initial, err := s.prepareHost(ctx)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return errors.New("cannot start ChatGPT callback listener on 127.0.0.1; choose another port")
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/auth/callback"
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
	results := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		values := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid sign-in state.", http.StatusBadRequest)
			return
		}
		result := callbackResult{code: values.Get("code"), clientID: values.Get("client_id")}
		if values.Has("error") {
			result.err = errors.New("ChatGPT authorization was denied; no credentials were changed")
		} else if result.code == "" {
			result.err = errors.New("ChatGPT callback did not contain an authorization code")
		} else if clientID == dynamicClient {
			if result.clientID == "" || result.clientID == dynamicClient {
				result.err = errors.New("ChatGPT registration did not return an issued client ID")
			}
		} else if result.clientID != "" && result.clientID != clientID {
			result.err = errors.New("ChatGPT callback changed the selected account registration")
		} else {
			result.clientID = clientID
		}
		select {
		case results <- result:
			fmt.Fprintln(w, "Sign-in response received. Return to the terminal to check the result.")
		default:
			http.Error(w, "Sign-in callback already received.", http.StatusConflict)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	defer server.Close()
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	if err := showURL(endpoint + "?" + query.Encode()); err != nil {
		return errors.New("cannot present ChatGPT authorization URL")
	}
	var result callbackResult
	select {
	case <-ctx.Done():
		return fmt.Errorf("ChatGPT login: %w", ctx.Err())
	case <-serveErrors:
		return errors.New("ChatGPT callback listener stopped")
	case result = <-results:
	}
	if result.err != nil {
		return result.err
	}
	if initial.ClientID == "" {
		// Registration survives a failed code exchange or missing plan grant.
		// No token or unverified account identity is made active here.
		if err := s.retainRegistration(ctx, initial, result.clientID); err != nil {
			return err
		}
		initial.ClientID = result.clientID
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {result.clientID}, "code": {result.code}, "code_verifier": {verifier}, "redirect_uri": {redirect}, "resource": {resource}}
	token, err := s.exchange(ctx, form)
	if err != nil {
		return err
	}
	identity, err := s.validateIDToken(ctx, token.IDToken, result.clientID, nonce)
	if err != nil {
		return err
	}
	if !hasPlanScope(token.Scope) {
		return errors.New("ChatGPT plan permission was not granted; authorize plan usage in ChatGPT settings")
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
	current.ClientID = result.clientID
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
