package chatgpt

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"syscall"
	"testing"
	"time"
)

type authorizationTransportFunc func(*http.Request) (*http.Response, error)

func (f authorizationTransportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestExchangeReportsSafeTransportCauses(t *testing.T) {
	const prefix = "ChatGPT authorization transport failed"
	for _, tc := range []struct {
		name, want string
		cause      error
	}{
		{"DNS", prefix + ": DNS lookup failed", &net.DNSError{Name: "resolver-secret", Server: "server-secret", Err: "lookup-secret"}},
		{"certificate", prefix + ": TLS certificate verification failed", &tls.CertificateVerificationError{Err: errors.New("certificate-secret")}},
		{"timeout", prefix + ": request timed out", context.DeadlineExceeded},
		{"EOF", prefix + ": connection closed", io.EOF},
		{"unexpected EOF", prefix + ": connection closed", io.ErrUnexpectedEOF},
		{"closed socket", prefix + ": connection closed", net.ErrClosed},
		{"broken pipe", prefix + ": connection closed", syscall.EPIPE},
		{"refused", prefix + ": connection refused", syscall.ECONNREFUSED},
		{"reset", prefix + ": connection reset", syscall.ECONNRESET},
		{"network unreachable", prefix + ": network unreachable", syscall.ENETUNREACH},
		{"host unreachable", prefix + ": network unreachable", syscall.EHOSTUNREACH},
		{"unknown", prefix, errors.New("unknown-secret")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Store{tokenURL: "https://token.example.invalid/private?capability=endpoint-secret", HTTP: &http.Client{
				Transport: authorizationTransportFunc(func(*http.Request) (*http.Response, error) {
					return nil, &url.Error{Op: "private-operation-secret", URL: "https://proxy-user:proxy-password@proxy.invalid/path?code=callback-secret", Err: fmt.Errorf("form-secret: %w", tc.cause)}
				}),
			}}
			token, err := s.exchange(t.Context(), url.Values{"grant_type": {"authorization_code"}, "code": {"authorization-code-secret"}, "code_verifier": {"verifier-secret"}})
			if err == nil || err.Error() != tc.want || token != (tokenResponse{}) {
				t.Fatalf("transport failure lost its safe category: token=%+v, err=%v, want=%q", token, err, tc.want)
			}
		})
	}
}

func TestExchangeClassifiesClientTimeoutWithLiveParentContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		close(entered)
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer func() { close(release); server.Close() }()
	s := &Store{tokenURL: server.URL, HTTP: &http.Client{Timeout: 100 * time.Millisecond}}
	_, err := s.exchange(ctx, url.Values{"code": {"authorization-code-secret"}})
	if ctx.Err() != nil {
		t.Fatalf("parent context expired instead of the client: %v", ctx.Err())
	}
	select {
	case <-entered:
	default:
		t.Fatal("request did not reach the response-header wait")
	}
	if err == nil || err.Error() != "ChatGPT authorization transport failed: request timed out" {
		t.Fatalf("client timeout was not classified: %v", err)
	}
}
