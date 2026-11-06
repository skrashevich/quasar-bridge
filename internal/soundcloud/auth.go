package soundcloud

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type tokenData struct {
	Access    string    `json:"access_token"`
	Refresh   string    `json:"refresh_token,omitempty"`
	ExpiresIn int64     `json:"expires_in,omitzero"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	User      bool      `json:"user_authorized,omitzero"`
}

func (c *Client) loadTokens() error {
	if c.cfg.TokenFile == "" {
		return nil
	}
	f, e := os.Open(c.cfg.TokenFile)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return errors.New("soundcloud: cannot read token_file")
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if e != nil {
		return errors.New("soundcloud: cannot read token_file")
	}
	if len(b) > 64<<10 {
		return errors.New("soundcloud: token_file too large")
	}
	var t tokenData
	if json.Unmarshal(b, &t) != nil {
		return errors.New("soundcloud: invalid token_file")
	}
	if c.token == "" && c.refresh == "" {
		c.token = t.Access
		c.refresh = t.Refresh
		c.expires = t.ExpiresAt
		c.userAuth = t.User
	}
	return nil
}
func (c *Client) saveTokens(t tokenData) error {
	if c.cfg.TokenFile == "" {
		return nil
	}
	b, e := json.Marshal(t)
	if e != nil {
		return errors.New("soundcloud: cannot encode tokens")
	}
	f, e := os.CreateTemp(filepath.Dir(c.cfg.TokenFile), ".soundcloud-token-*")
	if e != nil {
		return errors.New("soundcloud: cannot create token_file")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e == nil {
		e = os.Rename(name, c.cfg.TokenFile)
	}
	if e != nil {
		return errors.New("soundcloud: cannot save token_file")
	}
	return nil
}
func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && (c.expires.IsZero() || time.Until(c.expires) > 30*time.Second) {
		return c.token, nil
	}
	values := url.Values{}
	if c.refresh != "" {
		values.Set("grant_type", "refresh_token")
		values.Set("refresh_token", c.refresh)
	} else if c.userAuth {
		// A user principal cannot be renewed with an application grant. In
		// particular, personal operations must never retry using app credentials.
		return "", ErrAuthorization
	} else if c.cfg.ClientID != "" && c.cfg.ClientSecret != "" {
		values.Set("grant_type", "client_credentials")
		c.userAuth = false
	} else {
		return "", errors.New("SoundCloud: нужны OAuth token или client_id и client_secret")
	}
	values.Set("client_id", c.cfg.ClientID)
	values.Set("client_secret", c.cfg.ClientSecret)
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, "https://secure.soundcloud.com/oauth/token", strings.NewReader(values.Encode()))
	if e != nil {
		return "", errors.New("soundcloud: invalid authorization request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.cfg.ClientID != "" && c.cfg.ClientSecret != "" {
		req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	}
	tokenClient := *c.http
	tokenClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := tokenClient.Do(req)
	if e != nil {
		return "", errors.New("soundcloud: authorization request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New("soundcloud: authorization rejected")
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var t tokenData
	if e != nil || json.Unmarshal(b, &t) != nil || t.Access == "" {
		return "", errors.New("soundcloud: invalid authorization response")
	}
	if t.Refresh == "" {
		t.Refresh = c.refresh
	}
	if t.ExpiresIn <= 0 {
		t.ExpiresIn = 3600
	}
	t.ExpiresAt = time.Now().Add(time.Duration(min(t.ExpiresIn, 86400)) * time.Second)
	t.User = c.userAuth
	if e = c.saveTokens(t); e != nil {
		return "", e
	}
	c.token = t.Access
	c.refresh = t.Refresh
	c.expires = t.ExpiresAt
	return c.token, nil
}
func (c *Client) authorizedUser() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode == "official" && c.userAuth
}
func (c *Client) Likes(ctx context.Context, limit int) ([]Track, error) {
	if !c.authorizedUser() {
		return nil, ErrAuthorization
	}
	limit = c.limit(limit)
	items, e := c.pages(ctx, c.base()+"/me/likes/tracks", url.Values{"limit": {itoa(limit)}, "linked_partitioning": {"1"}}, limit, true)
	return tracksFrom(items, limit), e
}
func (c *Client) Like(ctx context.Context, t Track, like bool) error {
	if !c.authorizedUser() {
		return ErrAuthorization
	}
	id, e := c.resourcePath(t.URN, "tracks")
	if e != nil {
		return e
	}
	method := http.MethodPost
	if !like {
		method = http.MethodDelete
	}
	_, e = c.api(ctx, method, c.base()+"/likes/tracks/"+id, nil)
	return e
}
