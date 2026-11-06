// Package chatgpt authorizes a local Bridge installation to use a ChatGPT plan.
package chatgpt

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	issuer             = "https://auth.openai.com"
	resource           = "https://api.openai.com/v1"
	planScope          = "chatgpt.tokens.use.direct"
	dynamicClient      = "dynamic_agent_client"
	maxCredentialBytes = 1 << 20
)

// Store keeps one account registration. Select another file to use a different
// account or workspace. The file is independent of Codex's authentication cache.
type Store struct {
	Path string
	HTTP *http.Client

	// Test endpoints are deliberately not configurable through the application.
	authorizeURL string
	tokenURL     string
	jwksURL      string
	revokeURL    string
}

type credentials struct {
	Version      int       `json:"version"`
	HostID       string    `json:"ext_agent_host_id"`
	Issuer       string    `json:"issuer,omitempty"`
	Subject      string    `json:"subject,omitempty"`
	Email        string    `json:"email,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
}

// Status contains no access, refresh, or ID tokens. Expiry does not imply sign-out:
// an expired access token may still be renewed when the next request is made.
type Status struct {
	SignedIn  bool      `json:"signed_in"`
	Email     string    `json:"email,omitempty"`
	Subject   string    `json:"subject,omitempty"`
	ClientID  string    `json:"client_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

func DefaultPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", errors.New("cannot locate ChatGPT credential directory; set llm_chatgpt_auth_file")
	}
	return filepath.Join(base, "quasar-bridge", "chatgpt.json"), nil
}

func (s *Store) filename() (string, error) {
	if s.Path != "" {
		return s.Path, nil
	}
	return DefaultPath()
}

func (s *Store) load() (credentials, error) {
	path, err := s.filename()
	if err != nil {
		return credentials{}, err
	}
	f, err := openPrivate(path, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credentials{}, os.ErrNotExist
		}
		return credentials{}, errors.New("cannot read protected ChatGPT credentials")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return credentials{}, errors.New("ChatGPT credentials must be a regular owner-only file (chmod 600)")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
	var c credentials
	if err != nil || len(raw) > maxCredentialBytes || json.Unmarshal(raw, &c, json.RejectUnknownMembers(true)) != nil || c.Version != 1 || c.HostID == "" {
		return credentials{}, errors.New("invalid Bridge ChatGPT credentials; sign in with -chatgpt-auth login (Codex auth.json is not supported)")
	}
	return c, nil
}

func (s *Store) save(c credentials) error {
	path, err := s.filename()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if os.MkdirAll(dir, 0700) != nil {
		return errors.New("cannot create ChatGPT credential directory")
	}
	body, err := json.Marshal(c)
	if err != nil {
		return errors.New("cannot encode ChatGPT credentials")
	}
	f, err := os.CreateTemp(dir, ".chatgpt-*")
	if err != nil {
		return errors.New("cannot save ChatGPT credentials")
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil || os.Rename(temporary, path) != nil {
		return errors.New("cannot save ChatGPT credentials")
	}
	return nil
}

func hasPlanScope(scope string) bool {
	for item := range strings.FieldsSeq(scope) {
		if item == planScope {
			return true
		}
	}
	return false
}

func (c credentials) signedIn() bool {
	return c.Issuer == issuer && c.Subject != "" && c.ClientID != "" && c.ClientID != dynamicClient && c.AccessToken != "" && c.RefreshToken != "" && hasPlanScope(c.Scope)
}

func (c *credentials) clearTokens() {
	c.AccessToken, c.RefreshToken, c.IDToken, c.Scope = "", "", "", ""
	c.ExpiresAt = time.Time{}
}

// Status reads local account information without refreshing or making a request.
func (s *Store) Status(ctx context.Context) (Status, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	c, err := s.load()
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	return Status{SignedIn: c.signedIn(), Email: c.Email, Subject: c.Subject, ClientID: c.ClientID, ExpiresAt: c.ExpiresAt}, nil
}

// Token reloads credentials under an interprocess lock and rotates near-expiry
// tokens once, including when Bridge diagnostics and the daemon run together.
func (s *Store) Token(ctx context.Context) (string, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	c, err := s.load()
	if errors.Is(err, os.ErrNotExist) || (err == nil && !c.signedIn()) {
		return "", errors.New("ChatGPT is not signed in; run -chatgpt-auth login")
	}
	if err != nil {
		return "", err
	}
	if time.Until(c.ExpiresAt) > time.Minute {
		return c.AccessToken, nil
	}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ClientID}, "refresh_token": {c.RefreshToken}, "resource": {resource}}
	token, err := s.exchange(ctx, form)
	if err != nil {
		if oauth, ok := errors.AsType[*oauthError](err); ok && oauth.terminalRefresh() {
			c.clearTokens()
			if saveErr := s.save(c); saveErr != nil {
				return "", saveErr
			}
			return "", errors.New("ChatGPT session expired or was revoked; run -chatgpt-auth login")
		}
		return "", err
	}
	if token.Scope == "" {
		token.Scope = c.Scope
	}
	c.AccessToken, c.RefreshToken, c.Scope = token.AccessToken, token.RefreshToken, token.Scope
	c.ExpiresAt = time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second)
	// A refreshed ID token is not used as identity until validated during sign-in.
	if err := s.save(c); err != nil {
		return "", err
	}
	if !hasPlanScope(token.Scope) {
		return "", errors.New("ChatGPT plan permission is missing; run -chatgpt-auth login")
	}
	return c.AccessToken, nil
}

func (s *Store) httpClient() *http.Client {
	var client http.Client
	if s.HTTP != nil {
		client = *s.HTTP
	}
	if client.Timeout == 0 {
		client.Timeout = 30 * time.Second
	}
	// Token-bearing POSTs must never be redirected to another origin.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

type oauthError struct {
	code   string
	status int
}

func (e *oauthError) Error() string {
	if e.code == "invalid_client" {
		return "ChatGPT client registration was rejected"
	}
	return fmt.Sprintf("ChatGPT authorization returned HTTP %d", e.status)
}

func (e *oauthError) terminalRefresh() bool {
	switch e.code {
	case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
		return true
	}
	return false
}

func (s *Store) exchange(ctx context.Context, form url.Values) (tokenResponse, error) {
	endpoint := s.tokenURL
	if endpoint == "" {
		endpoint = issuer + "/api/accounts/oauth/token"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, errors.New("invalid ChatGPT authorization endpoint")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return tokenResponse{}, fmt.Errorf("ChatGPT authorization: %w", ctx.Err())
		}
		return tokenResponse{}, errors.New("ChatGPT authorization transport failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCredentialBytes+1))
	if err != nil || len(body) > maxCredentialBytes {
		return tokenResponse{}, errors.New("cannot read ChatGPT authorization response")
	}
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &failure)
		return tokenResponse{}, &oauthError{code: failure.Error, status: resp.StatusCode}
	}
	var token tokenResponse
	if json.Unmarshal(body, &token) != nil || token.AccessToken == "" || token.RefreshToken == "" || !strings.EqualFold(token.TokenType, "Bearer") || token.ExpiresIn <= 0 || token.ExpiresIn > 30*24*60*60 || strings.ContainsAny(token.AccessToken+token.RefreshToken, "\r\n") {
		return tokenResponse{}, errors.New("invalid ChatGPT authorization response")
	}
	return token, nil
}

// Logout revokes the renewable session, clears local tokens, and keeps the
// account registration and host identity for a later sign-in. A revocation
// failure is reported even though local sign-out still completes.
func (s *Store) Logout(ctx context.Context) error {
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := s.load()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var revokeErr error
	if c.RefreshToken != "" {
		endpoint := s.revokeURL
		if endpoint == "" {
			endpoint = issuer + "/api/accounts/oauth/revoke"
		}
		form := url.Values{"token": {c.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {c.ClientID}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			revokeErr = errors.New("ChatGPT signed out locally; remote revocation was not confirmed; disconnect Bridge in ChatGPT settings")
		} else {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := s.httpClient().Do(req)
			if err != nil {
				revokeErr = errors.New("ChatGPT signed out locally; remote revocation was not confirmed; disconnect Bridge in ChatGPT settings")
			} else {
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					revokeErr = errors.New("ChatGPT signed out locally; remote revocation was not confirmed; disconnect Bridge in ChatGPT settings")
				}
			}
		}
	}
	c.clearTokens()
	if err := s.save(c); err != nil {
		return err
	}
	return revokeErr
}
