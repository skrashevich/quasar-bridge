package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrRefreshUnavailable = errors.New("MCP OAuth token refresh temporarily unavailable")

type Source struct {
	Store *Store
	HTTP  http.RoundTripper
}

func (s *Source) AccessToken(ctx context.Context, b Binding) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s == nil || s.Store == nil {
		return "", ErrSignInRequired
	}
	r, err := s.Store.Load(b)
	if errors.Is(err, ErrNotSignedIn) {
		return "", ErrSignInRequired
	}
	if err != nil {
		return "", err
	}
	if usableAccessToken(r, time.Now()) {
		return r.AccessToken, nil
	}
	if r.NeedsLogin || r.RefreshToken == "" {
		return "", ErrSignInRequired
	}
	r, err = s.Store.Update(ctx, b, func(latest Record) (Record, error) {
		if usableAccessToken(latest, time.Now()) {
			return latest, nil
		}
		if latest.NeedsLogin || latest.RefreshToken == "" {
			return Record{}, ErrSignInRequired
		}
		return s.refresh(ctx, b, latest)
	})
	if errors.Is(err, ErrNotSignedIn) {
		return "", ErrSignInRequired
	}
	if err != nil {
		return "", err
	}
	if !usableAccessToken(r, time.Now()) {
		return "", ErrSignInRequired
	}
	return r.AccessToken, nil
}

func usableAccessToken(r Record, now time.Time) bool {
	return !r.NeedsLogin && validBearer(r.AccessToken) && (r.Expiry.IsZero() || now.Before(r.Expiry))
}

func (s *Source) refresh(ctx context.Context, b Binding, r Record) (Record, error) {
	endpoint, _ := url.Parse(r.TokenEndpoint)
	issuer, _ := url.Parse(b.Issuer)
	if !onOrigin(endpoint, issuer) {
		return Record{}, ErrSignInRequired
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {b.ClientID},
		"refresh_token": {r.RefreshToken},
		"resource":      {b.Endpoint},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Record{}, ErrRefreshUnavailable
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := newOAuthHTTP(b, s.HTTP).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Record{}, ctx.Err()
		}
		return Record{}, ErrRefreshUnavailable
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes+1))
	var reply struct {
		Error        string      `json:"error"`
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		TokenType    string      `json:"token_type"`
		ExpiresIn    json.Number `json:"expires_in"`
		Scope        any         `json:"scope"`
	}
	if err != nil || len(body) > maxOAuthResponseBytes || json.Unmarshal(body, &reply) != nil {
		return Record{}, ErrRefreshUnavailable
	}
	if resp.StatusCode == http.StatusBadRequest && reply.Error == "invalid_grant" {
		return Record{Binding: b, NeedsLogin: true}, nil
	}
	if resp.StatusCode != http.StatusOK || reply.Error != "" || !validBearer(reply.AccessToken) ||
		(reply.TokenType != "" && !strings.EqualFold(reply.TokenType, "Bearer")) ||
		!grantedScopesAllowed(reply.Scope, b.Scopes) {
		return Record{}, ErrRefreshUnavailable
	}
	r.AccessToken = reply.AccessToken
	r.Expiry = time.Time{}
	if reply.ExpiresIn != "" {
		seconds, err := reply.ExpiresIn.Int64()
		if err != nil || seconds <= 0 || seconds > int64((1<<63-1)/time.Second) {
			return Record{}, ErrRefreshUnavailable
		}
		r.Expiry = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	if reply.RefreshToken != "" {
		r.RefreshToken = reply.RefreshToken
	}
	return r, nil
}
