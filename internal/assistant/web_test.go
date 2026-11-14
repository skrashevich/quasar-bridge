package assistant

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestWebReaderExtractsBoundedTextAndBlocksPrivateTargets(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WebMaxChars = 100
	reader := NewWebReader(cfg)
	reader.http = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(`<html><title>Пример</title><script>private script</script><style>hidden style</style><p>Полезный &amp; видимый текст ` + strings.Repeat("hello ", 100) + `</p></html>`))}, nil
	})}
	got, err := reader.Read(t.Context(), "https://example.org/article")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "Полезный & видимый") || strings.Contains(got.Text, "private script") || !got.Truncated || len([]rune(got.Text)) > 100 {
		t.Fatalf("bad text: %+v", got)
	}
	for _, target := range []string{"file:///etc/passwd", "http://127.0.0.1/", "http://[::1]/", "http://169.254.169.254/", "https://user:password@example.org/"} {
		if _, err := reader.Read(t.Context(), target); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
}
func TestWebDialRejectsDNSRebindingAndRedirects(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	if _, err := reader.Read(t.Context(), "http://example.org/"); err == nil {
		t.Fatal("private DNS accepted")
	}
	req, _ := http.NewRequest("GET", "http://127.0.0.1/", nil)
	if reader.http.CheckRedirect(req, []*http.Request{{}}) == nil {
		t.Fatal("private redirect accepted")
	}
}

func TestWebReaderDecodesRussianCharset(t *testing.T) {
	for _, contentType := range []string{"text/html; charset=windows-1251", "text/html"} {
		t.Run(contentType, func(t *testing.T) {
			reader := NewWebReader(DefaultConfig())
			data := append([]byte(`<html><meta charset="windows-1251"><p>`), []byte{0xcf, 0xf0, 0xe8, 0xe2, 0xe5, 0xf2}...)
			data = append(data, []byte(`</p></html>`)...)
			reader.http = &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			})}
			result, err := reader.Read(t.Context(), "https://example.org/")
			if err != nil || result.Text != "Привет" {
				t.Fatalf("charset lost: %+v %v", result, err)
			}
		})
	}
}

func TestWebReaderRejectsPrivateURLsBeforeSending(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.http.Transport = roundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("blocked URL reached the HTTP transport")
		return nil, nil
	})
	targets := []string{
		"http://localhost/", "http://LOCALHOST./", "http://127.0.0.1/",
		"http://169.254.169.254/latest/meta-data/", "http://100.64.0.1/",
		"http://0.0.0.0/", "http://198.18.0.1/", "http://192.0.2.1/",
		"http://[::1]/", "http://[::ffff:127.0.0.1]/", "http://[::ffff:a00:1]/",
		"http://[fe80::1]/", "http://[fc00::1]/", "http://[2001:db8::1]/",
		"http://[64:ff9b::a00:1]/", "http://[2002:7f00:1::]/",
		"file:///etc/passwd", "ftp://example.org/", "https://user:password@example.org/",
	}
	// Construct synthetic RFC1918 fixtures, keeping deployment address literals
	// out of the public tree without exempting this file from its scanner.
	for _, ip := range []net.IP{net.IPv4(10, 0, 0, 1), net.IPv4(172, 16, 0, 1), net.IPv4(192, 168, 0, 1)} {
		targets = append(targets, "http://"+ip.String()+"/")
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			if _, err := reader.Read(t.Context(), target); err == nil {
				t.Fatal("private, reserved or non-HTTP target accepted")
			}
		})
	}
}

func TestWebDialRejectsEveryDNSAddressBeforeConnecting(t *testing.T) {
	public := net.IPAddr{IP: net.ParseIP("93.184.215.14")}
	for _, tc := range []struct {
		name string
		ips  []net.IPAddr
	}{
		{"loopback", []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}},
		{"metadata", []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}},
		{"mapped private", []net.IPAddr{{IP: net.ParseIP("::ffff:c0a8:1")}}},
		{"mixed public then private", []net.IPAddr{public, {IP: net.IPv4(10, 0, 0, 1)}}},
		{"mixed public then reserved", []net.IPAddr{public, {IP: net.ParseIP("198.18.0.1")}}},
		{"scoped IPv6", []net.IPAddr{{IP: net.ParseIP("2606:4700:4700::1111"), Zone: "eth0"}}},
		{"invalid address", []net.IPAddr{{IP: net.IP{1, 2, 3}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := NewWebReader(DefaultConfig())
			reader.lookup = func(context.Context, string) ([]net.IPAddr, error) { return tc.ips, nil }
			// An invalid port also prevents external traffic if this guard regresses.
			_, err := reader.dial(t.Context(), "tcp", "example.org:invalid-port")
			if err == nil || err.Error() != "web DNS resolved to a private or reserved address" {
				t.Fatalf("DNS answer was not rejected before dial: %v", err)
			}
		})
	}
}

func TestWebReaderBlocksPrivateRedirectsBeforeFollowing(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1/", "http://169.254.169.254/", "http://[::ffff:a00:1]/", "http://localhost./", "file:///etc/passwd"} {
		t.Run(target, func(t *testing.T) {
			reader := NewWebReader(DefaultConfig())
			calls := 0
			reader.http.Transport = roundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			if _, err := reader.Read(t.Context(), "https://example.org/"); err == nil || calls != 1 {
				t.Fatalf("private redirect followed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestWebReaderRedirectToHostnameStillUsesDNSGuard(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	transport := reader.http.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.DialTLSContext != nil || transport.DialContext == nil {
		t.Fatal("web transport can bypass the validated DNS dialer")
	}
	calls := 0
	reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://private.example/"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		_, err := transport.DialContext(r.Context(), "tcp", r.URL.Hostname()+":443")
		return nil, err
	})
	reader.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.IPv4(192, 168, 0, 1)}}, nil
	}
	if _, err := reader.Read(t.Context(), "https://example.org/"); err == nil || calls != 2 {
		t.Fatalf("redirect bypassed private DNS guard: calls=%d err=%v", calls, err)
	}
}
