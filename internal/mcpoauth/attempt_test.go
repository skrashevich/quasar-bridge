package mcpoauth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type manualAttemptTransport struct {
	http.RoundTripper
	tokenRequests atomic.Int32
}

func (t *manualAttemptTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost {
		t.tokenRequests.Add(1)
	}
	return t.RoundTripper.RoundTrip(r)
}

func startManualFixture(t *testing.T, ctx context.Context, redirect string) (*Attempt, string, <-chan error, *manualAttemptTransport) {
	t.Helper()
	server, binding := loginFixture(t, loginFixtureOptions{})
	httpTransport := &manualAttemptTransport{RoundTripper: server.Client().Transport}
	links := make(chan string, 1)
	attempt, err := NewAttempt(binding, redirect, func(_ context.Context, link string) error {
		links <- link
		return nil
	}, httpTransport)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, binding.Endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{StatusCode: 401, Body: http.NoBody, Header: make(http.Header)}
	resp.Header.Set("WWW-Authenticate", `Bearer resource_metadata="`+binding.Issuer+`/.well-known/oauth-protected-resource"`)
	done := make(chan error, 1)
	go func() { done <- attempt.Handler().Authorize(ctx, req, resp) }()
	select {
	case link := <-links:
		u, err := url.Parse(link)
		if err != nil || u.Query().Get("redirect_uri") != redirect {
			t.Fatalf("unexpected authorization redirect: %v", err)
		}
		q := url.Values{"state": {u.Query().Get("state")}, "code": {"fixture-code"}, "iss": {binding.Issuer}}
		return attempt, redirect + "?" + q.Encode(), done, httpTransport
	case err := <-done:
		t.Fatalf("authorization URL not produced: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return nil, "", nil, nil
}

func TestManualAttemptDoesNotBindAndAcceptsExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	attempt, callback, done, transport := startManualFixture(t, ctx, "http://"+occupied.Addr().String()+"/callback")
	if _, err := attempt.Record(); err == nil {
		t.Fatal("credential available before completion")
	}
	if outcome, err := attempt.Submit(callback); err != nil || outcome != CallbackAccepted {
		t.Fatalf("first submit: %v, %v", outcome, err)
	}
	if _, err := attempt.Submit(callback); !errors.Is(err, ErrCallbackUsed) {
		t.Fatalf("callback replay: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if record, err := attempt.Record(); err != nil || record.AccessToken != "fixture-access" ||
		transport.tokenRequests.Load() != 1 {
		t.Fatalf("one token should be recorded after one exchange: %v", err)
	}
}

func TestManualAttemptRejectsCallbackWithoutConsuming(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	attempt, callback, done, transport := startManualFixture(t, ctx, ManualRedirect)
	u, _ := url.Parse(callback)
	badIssuer := u.Query()
	badIssuer.Set("iss", "https://unexpected.example.invalid")
	noIssuer := u.Query()
	noIssuer.Del("iss")
	for _, invalid := range []string{
		strings.Replace(callback, "127.0.0.1", "elsewhere.example.invalid", 1),
		strings.Replace(callback, ":19876", ":19877", 1),
		strings.Replace(callback, "/callback?", "/other?", 1),
		callback + "#fragment",
		callback + "&state=duplicate",
		strings.Replace(callback, u.Query().Get("state"), "wrong-state", 1),
		callback + "&code=another",
		callback + "&error=access_denied",
		ManualRedirect + "?" + badIssuer.Encode(),
		ManualRedirect + "?" + noIssuer.Encode(),
		callback + "&extra=" + strings.Repeat("x", 8192),
	} {
		if _, err := attempt.Submit(invalid); !errors.Is(err, ErrCallbackMismatch) {
			t.Fatalf("invalid return did not produce a mismatch: %v", err)
		}
	}
	if transport.tokenRequests.Load() != 0 {
		t.Fatal("an invalid return reached the token endpoint")
	}
	if outcome, err := attempt.Submit(callback); err != nil || outcome != CallbackAccepted {
		t.Fatalf("invalid return consumed the attempt: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestManualAttemptDenialAndCancellation(t *testing.T) {
	for _, denied := range []bool{true, false} {
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
		attempt, callback, done, transport := startManualFixture(t, ctx, ManualRedirect)
		if denied {
			u, _ := url.Parse(callback)
			q := u.Query()
			q.Del("code")
			q.Set("error", "access_denied")
			if outcome, err := attempt.Submit(ManualRedirect + "?" + q.Encode()); err != nil || outcome != CallbackDenied {
				cancel()
				t.Fatalf("matching denial: %v, %v", outcome, err)
			}
			if _, err := attempt.Submit(callback); !errors.Is(err, ErrCallbackUsed) {
				cancel()
				t.Fatalf("denial did not consume attempt: %v", err)
			}
		} else {
			cancel()
		}
		if err := <-done; err == nil || transport.tokenRequests.Load() != 0 {
			cancel()
			t.Fatalf("denied or canceled attempt performed token exchange: %v", err)
		}
		cancel()
	}
}
