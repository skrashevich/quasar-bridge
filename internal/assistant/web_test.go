package assistant

import (
	"context"
	"crypto/tls"
	"errors"
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

func TestWebReaderReportsSafeFailureCauses(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		lookup     func(context.Context, string) ([]net.IPAddr, error)
		transport  http.RoundTripper
	}{
		{name: "DNS", want: "web DNS lookup failed", lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return nil, errors.New("secret resolver details")
		}},
		{name: "private DNS", want: "web DNS resolved to a private or reserved address", lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}},
		{name: "HTTP", want: "web page returned HTTP 403", transport: roundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("secret response body")), Header: make(http.Header)}, nil
		})},
		{name: "TLS", want: "web TLS certificate verification failed", transport: roundTripper(func(*http.Request) (*http.Response, error) {
			return nil, &tls.CertificateVerificationError{Err: errors.New("secret certificate details")}
		})},
		{name: "transport", want: "web request failed or target blocked", transport: roundTripper(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("secret " + req.URL.String())
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := NewWebReader(DefaultConfig())
			if tc.lookup != nil {
				reader.lookup = tc.lookup
			}
			if tc.transport != nil {
				reader.http.Transport = tc.transport
			}
			_, err := reader.Read(t.Context(), "https://audio.example/private-path?token=secret")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error=%v want=%q", err, tc.want)
			}
		})
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
