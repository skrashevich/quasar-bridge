package soundcloud

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func parseProxyURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	invalid := errors.New("soundcloud: proxy_url must be a SOCKS5 URL with host and port")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "socks5" && u.Scheme != "socks5h") || u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return nil, invalid
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || host == "" || port == "" {
		return nil, invalid
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return nil, invalid
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, invalid
	}
	if u.User != nil {
		password, _ := u.User.Password()
		if len(u.User.Username()) == 0 || len(u.User.Username()) > 255 || len(password) > 255 {
			return nil, invalid
		}
	}
	return u, nil
}

// NewHTTPClient creates an isolated SoundCloud transport. A configured SOCKS5
// proxy handles origin DNS resolution and replaces environment proxy selection.
// The caller sets its own timeout for catalog or media requests.
func NewHTTPClient(proxyURL string) (*http.Client, error) {
	u, err := parseProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("soundcloud: cannot create HTTP transport")
	}
	transport := base.Clone()
	if u != nil {
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: transport}, nil
}
