package soundcloud

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxResponse = 8 << 20
const officialBase = "https://api.soundcloud.com"
const webBase = "https://api-v2.soundcloud.com"

type Client struct {
	cfg      Config
	http     *http.Client
	mode     string
	mu       sync.Mutex
	token    string
	refresh  string
	expires  time.Time
	userAuth bool
	clientID string
	webToken string
}

func NewClient(cfg Config, hc *http.Client) (*Client, error) {
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if hc == nil {
		var err error
		hc, err = NewHTTPClient(cfg.ProxyURL)
		if err != nil {
			return nil, err
		}
	}
	copyClient := *hc
	if copyClient.Timeout <= 0 || copyClient.Timeout > 30*time.Second {
		copyClient.Timeout = 20 * time.Second
	}
	// A redirect is a new trust decision; credentials never leave their original host.
	prior := copyClient.CheckRedirect
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("soundcloud: too many redirects")
		}
		if err := ValidateAssetURL(req.URL); err != nil {
			return err
		}
		if len(via) > 0 && req.URL.Hostname() != via[0].URL.Hostname() {
			req.Header.Del("Authorization")
		}
		if prior != nil {
			return prior(req, via)
		}
		return nil
	}
	c := &Client{cfg: cfg, http: &copyClient, token: cfg.AccessToken, refresh: cfg.RefreshToken, userAuth: cfg.AccessToken != "" || cfg.RefreshToken != "", clientID: cfg.ClientID}
	if cfg.API != "official" {
		if err := c.loadWebToken(); err != nil {
			return nil, err
		}
	}
	if cfg.API != "web" && c.webToken == "" {
		if err := c.loadTokens(); err != nil {
			return nil, err
		}
	}
	c.mode = cfg.API
	if c.mode == "auto" {
		c.mode = "web"
		if c.webToken == "" && (c.token != "" || c.refresh != "" || (cfg.ClientID != "" && cfg.ClientSecret != "")) {
			c.mode = "official"
		}
	}
	// An official application's client ID is not the browser application's ID.
	// Keep the legacy public-web override only when no official credentials are
	// configured; retaining those credentials across web login must not mix APIs.
	if c.mode == "web" && (cfg.ClientSecret != "" || cfg.AccessToken != "" || cfg.RefreshToken != "" || cfg.TokenFile != "") {
		c.clientID = ""
	}
	return c, nil
}
func ValidateAssetURL(u *url.URL) error {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Host == "" || (u.Port() != "" && u.Port() != "443") {
		return errors.New("soundcloud: forbidden upstream URL")
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil {
		return errors.New("soundcloud: forbidden upstream host")
	}
	for _, domain := range []string{"soundcloud.com", "sndcdn.com", "soundcloud.cloud"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return nil
		}
	}
	return errors.New("soundcloud: forbidden upstream host")
}
func (c *Client) base() string {
	if c.mode == "web" {
		return webBase
	}
	return officialBase
}
func (c *Client) limit(n int) int {
	if n <= 0 {
		return c.cfg.QueueLimit
	}
	return min(n, c.cfg.QueueLimit)
}
func (c *Client) request(ctx context.Context, method, raw string, values url.Values, auth bool) ([]byte, int, error) {
	u, err := url.Parse(raw)
	if err != nil || ValidateAssetURL(u) != nil {
		return nil, 0, errors.New("soundcloud: invalid API URL")
	}
	// Authenticated requests are exclusively to the catalog origin.
	if auth && u.Hostname() != strings.TrimPrefix(c.base(), "https://") {
		return nil, 0, errors.New("soundcloud: invalid API origin")
	}
	q := u.Query()
	for k, v := range values {
		q[k] = v
	}
	u.RawQuery = q.Encode()
	var body io.Reader
	if auth && c.mode == "web" && c.webToken != "" && method == http.MethodPut {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, 0, errors.New("soundcloud: invalid request")
	}
	if auth {
		if c.mode == "web" {
			id, e := c.webID(ctx, false)
			if e != nil {
				return nil, 0, e
			}
			q.Set("client_id", id)
			req.URL.RawQuery = q.Encode()
			if c.webToken != "" {
				req.Header.Set("Authorization", "OAuth "+c.webToken)
				req.Header.Set("Origin", "https://soundcloud.com")
				req.Header.Set("Referer", "https://soundcloud.com/")
				req.Header.Set("Accept", "application/json")
				q.Set("app_locale", "en")
				req.URL.RawQuery = q.Encode()
				if method == http.MethodPut {
					req.Header.Set("Content-Type", "application/json")
				}
			}
		} else {
			token, e := c.accessToken(ctx, false)
			if e != nil {
				return nil, 0, e
			}
			req.Header.Set("Authorization", "OAuth "+token)
		}
	}
	requestClient := c.http
	if auth {
		catalogClient := *c.http
		prior := catalogClient.CheckRedirect
		catalogClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
			// A mutation's redirect must never turn an unsuccessful write into
			// a successful login page GET. Catalog reads stay on their origin.
			if method != http.MethodGet && method != http.MethodHead || next.URL.Scheme != u.Scheme || next.URL.Host != u.Host {
				return http.ErrUseLastResponse
			}
			if prior != nil {
				return prior(next, via)
			}
			return nil
		}
		requestClient = &catalogClient
	}
	resp, err := requestClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, 0, context.DeadlineExceeded
		}
		return nil, 0, errors.New("soundcloud: HTTP request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, resp.StatusCode, errors.New("soundcloud: response read failed")
	}
	if len(data) > maxResponse {
		return nil, resp.StatusCode, errors.New("soundcloud: response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, &httpStatusError{Code: resp.StatusCode}
	}
	return data, resp.StatusCode, nil
}
func (c *Client) api(ctx context.Context, method, raw string, q url.Values) ([]byte, error) {
	b, status, e := c.request(ctx, method, raw, q, true)
	if c.mode == "web" && c.webToken != "" {
		switch status {
		case http.StatusUnauthorized:
			return nil, ErrAuthorization
		case http.StatusForbidden:
			return nil, ErrWebChallenge
		}
		if e == nil && method != http.MethodGet && method != http.MethodHead && !confirmedWebMutation(status, b) {
			return nil, ErrWebChallenge
		}
	}
	if status == 401 || status == 403 {
		if c.mode == "web" {
			_, e = c.webID(ctx, true)
		} else {
			_, e = c.accessToken(ctx, true)
		}
		if e == nil {
			b, _, e = c.request(ctx, method, raw, q, true)
		}
	}
	return b, e
}
func (c *Client) pages(ctx context.Context, raw string, q url.Values, limit int, playableOnly bool) ([]catalogItem, error) {
	var accept func(catalogItem) bool
	if playableOnly {
		accept = func(item catalogItem) bool { _, ok := item.track(); return ok }
	}
	return c.pagesMatching(ctx, raw, q, limit, accept)
}

func (c *Client) pagesMatching(ctx context.Context, raw string, q url.Values, limit int, accept func(catalogItem) bool) ([]catalogItem, error) {
	var out []catalogItem
	err := c.visitPages(ctx, raw, q, func(item catalogItem) (bool, error) {
		if accept != nil && !accept(item) {
			return false, nil
		}
		out = append(out, item)
		return len(out) >= limit, nil
	})
	return out, err
}

// visitPages keeps collection expansion inside the bounded page traversal so
// empty collections cannot hide a later usable collection.
func (c *Client) visitPages(ctx context.Context, raw string, q url.Values, visit func(catalogItem) (bool, error)) error {
	seen := map[string]bool{}
	for range 10 {
		if seen[raw] {
			break
		}
		seen[raw] = true
		b, err := c.api(ctx, http.MethodGet, raw, q)
		if err != nil {
			return err
		}
		items, next, err := decodeItems(b)
		if err != nil {
			return errors.New("soundcloud: invalid catalog response")
		}
		for _, item := range items {
			done, err := visit(item)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
		if next == "" {
			break
		}
		u, err := url.Parse(next)
		if err != nil {
			return errors.New("soundcloud: invalid pagination URL")
		}
		base, _ := url.Parse(c.base())
		current, _ := url.Parse(raw)
		u = current.ResolveReference(u)
		if u.Scheme != "https" || u.Host != base.Host || u.User != nil {
			return errors.New("soundcloud: invalid pagination origin")
		}
		raw, q = u.String(), nil
	}
	return nil
}

func tracksFrom(items []catalogItem, limit int) []Track {
	out := make([]Track, 0, min(len(items), limit))
	for _, i := range items {
		if t, ok := i.track(); ok {
			out = append(out, t)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}
func (c *Client) Track(ctx context.Context, urn string) (Track, error) {
	id, e := c.resourcePath(urn, "tracks")
	if e != nil {
		return Track{}, e
	}
	b, e := c.api(ctx, "GET", c.base()+"/tracks/"+id, nil)
	if e != nil {
		if status, ok := errors.AsType[*httpStatusError](e); ok && (status.Code == http.StatusNotFound || status.Code == http.StatusGone) {
			return Track{}, ErrUnavailable
		}
		return Track{}, e
	}
	var i catalogItem
	if json.Unmarshal(b, &i) != nil {
		return Track{}, errors.New("soundcloud: invalid track")
	}
	t, ok := i.track()
	if !ok {
		return Track{}, ErrUnavailable
	}
	return t, nil
}
func resourceID(urn, kind string) (string, error) {
	id := strings.TrimPrefix(urn, "soundcloud:"+kind+":")
	if id == "" {
		return "", errors.New("soundcloud: missing resource ID")
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return "", errors.New("soundcloud: invalid resource ID")
		}
	}
	return id, nil
}

func (c *Client) resourcePath(urn, kind string) (string, error) {
	id, err := resourceID(urn, kind)
	if err != nil {
		return "", err
	}
	if c.mode == "official" {
		return "soundcloud:" + kind + ":" + id, nil
	}
	return id, nil
}

// httpStatusError deliberately carries no URL or upstream response body.
type httpStatusError struct{ Code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("soundcloud: HTTP status %d", e.Code) }
