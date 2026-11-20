package assistant

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type WebResult struct {
	URL       string    `json:"url"`
	Title     string    `json:"title,omitempty"`
	Text      string    `json:"text"`
	Links     []WebLink `json:"links,omitempty"`
	Audio     []WebLink `json:"audio,omitempty"`
	Truncated bool      `json:"truncated,omitzero"`
	music     pageMusic
}
type WebReader struct {
	config Config
	http   *http.Client
	lookup func(context.Context, string) ([]net.IPAddr, error)
}

var (
	errWebURL        = errors.New("web URL must be HTTP/HTTPS without credentials")
	errWebPrivate    = errors.New("web private addresses are blocked")
	errWebPrivateDNS = errors.New("web DNS resolved to a private or reserved address")
	errWebDNS        = errors.New("web DNS lookup failed")
	errWebConnection = errors.New("web connection failed")
	errWebRedirects  = errors.New("web redirect limit exceeded")
)

func NewWebReader(c Config) *WebReader {
	w := &WebReader{config: c, lookup: net.DefaultResolver.LookupIPAddr}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // DNS validation must not be bypassed by a proxy from the environment.
	transport.DialContext = w.dial
	transport.MaxIdleConns = 2
	transport.MaxIdleConnsPerHost = 1
	transport.IdleConnTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = time.Duration(c.WebTimeoutMS) * time.Millisecond
	w.http = &http.Client{Transport: transport, Timeout: time.Duration(c.WebTimeoutMS) * time.Millisecond, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return errWebRedirects
		}
		return validateWebURL(req.URL)
	}}
	return w
}
func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range []string{"100.64.0.0/10", "198.18.0.0/15", "192.0.0.0/24", "192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32", "2002::/16", "2001::/32", "64:ff9b::/96"} {
		if netip.MustParsePrefix(prefix).Contains(ip) {
			return false
		}
	}
	return true
}
func validateWebURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return errWebURL
	}
	if strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "localhost") {
		return errWebPrivate
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicIP(ip) {
		return errWebPrivate
	}
	return nil
}
func (w *WebReader) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid web address")
	}
	ips, err := w.lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errWebDNS
	}
	for _, ip := range ips {
		a, ok := netip.AddrFromSlice(ip.IP)
		if !ok || ip.Zone != "" || !publicIP(a) {
			return nil, errWebPrivateDNS
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, errWebConnection
}
func (w *WebReader) Read(ctx context.Context, raw string) (WebResult, error) {
	body, err := w.readBody(ctx, raw, nil)
	if err != nil {
		return WebResult{}, err
	}
	data, typ := body.data, body.typ
	result := WebResult{URL: body.url, Truncated: body.truncated}
	text := string(data)
	if typ == "text/html" || typ == "application/xhtml+xml" {
		result.Title, text = htmlText(data, w.config.WebMaxChars)
		pageURL, _ := url.Parse(result.URL)
		result.Links, result.Audio, result.music = extractPageMedia(data, pageURL)
	} else if typ == "application/json" {
		pageURL, _ := url.Parse(result.URL)
		media := newMediaCollector(pageURL)
		media.json(data)
		result.Audio = media.audio
	}
	text = strings.Join(strings.Fields(strings.ToValidUTF8(text, "�")), " ")
	if runes := []rune(text); len(runes) > w.config.WebMaxChars {
		text = string(runes[:w.config.WebMaxChars])
		result.Truncated = true
	}
	// HTML extraction reads only enough text to establish truncation.
	if title := []rune(result.Title); len(title) > 300 {
		result.Title = string(title[:300])
	}
	result.Text = text
	if text == "" && len(result.Audio) == 0 && len(result.Links) == 0 && !(result.music.player && len(result.music.scripts) > 0) {
		return WebResult{}, errors.New("web page contains no readable text")
	}
	return result, nil
}
func htmlText(data []byte, maxChars int) (string, string) {
	z := html.NewTokenizer(bytes.NewReader(data))
	var parts []string
	var title string
	hidden := ""
	inTitle := false
	size := 0
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			break
		}
		token := z.Token()
		switch kind {
		case html.StartTagToken:
			name := token.Data
			if hidden == "" && (name == "script" || name == "style" || name == "noscript" || name == "svg") {
				hidden = name
			}
			if name == "title" {
				inTitle = true
			}
		case html.EndTagToken:
			if token.Data == hidden {
				hidden = ""
			}
			if token.Data == "title" {
				inTitle = false
			}
		case html.TextToken:
			if hidden != "" {
				continue
			}
			part := strings.Join(strings.Fields(token.Data), " ")
			if inTitle {
				title += part
				continue
			}
			if part != "" {
				parts = append(parts, part)
				size += len([]rune(part)) + 1
			}
			if size > maxChars {
				return title, strings.Join(parts, " ")
			}
		}
	}
	return title, strings.Join(parts, " ")
}
