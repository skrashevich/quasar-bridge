package llm

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestTransportErrorPreservesCauseWithoutRuntimeDisclosure(t *testing.T) {
	const endpoint = "https://private.example.invalid/private-path?access_token=secret-query"
	c := Client{BaseURL: endpoint, APIKey: "secret-key", HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: "https://user:secret-password@private.example.invalid/?key=secret-query", Err: fmt.Errorf("read failure for secret-key: %w", io.ErrUnexpectedEOF)}
	})}}
	_, err := c.Complete(t.Context(), nil, false)
	failure, ok := errors.AsType[*TransportError](err)
	if !ok || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("transport cause lost: %v", err)
	}
	for _, private := range []string{"secret", "private-path", "private.example.invalid", "unexpected EOF"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("runtime disclosure: %v", err)
		}
	}
	if detail := failure.Diagnostic(); !strings.Contains(detail, "unexpected EOF") || strings.Contains(detail, "secret") || strings.Contains(detail, "https://") {
		t.Fatalf("unsafe or unhelpful diagnostic: %q", detail)
	}
}

func probeTransportResponse(t *testing.T, request *http.Request) (*http.Response, error) {
	t.Helper()
	body := readChatRequest(t, request)
	recorder := httptest.NewRecorder()
	writeCompatibilityAnswer(recorder, body)
	return recorder.Result(), nil
}

func TestProbeCompatibilityRetriesSameProfileAfterParameterRemoval(t *testing.T) {
	var requests []map[string]any
	reports := []error{}
	c := Client{BaseURL: "https://provider.example.invalid/v1", HTTP: &http.Client{Transport: transportFunc(func(request *http.Request) (*http.Response, error) {
		body := readChatRequest(t, request)
		requests = append(requests, body)
		if len(requests) == 1 {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`[{"error":{"message":"Unknown name chat_template_kwargs: Cannot find field.","status":"INVALID_ARGUMENT"}}]`))}, nil
		}
		if len(requests) == 2 {
			return nil, io.EOF
		}
		recorder := httptest.NewRecorder()
		writeCompatibilityAnswer(recorder, body)
		return recorder.Result(), nil
	})}}
	parameters, err := c.ProbeCompatibility(t.Context(), func(err error) { reports = append(reports, err) })
	if err != nil || !reflect.DeepEqual(parameters, map[string]any{"chat_template_kwargs": nil}) || len(requests) != 4 {
		t.Fatalf("parameters=%v error=%v requests=%d", parameters, err, len(requests))
	}
	if !reflect.DeepEqual(requests[1], requests[2]) {
		t.Fatalf("transport failure changed profile: %v -> %v", requests[1], requests[2])
	}
	if len(reports) != 2 {
		t.Fatalf("reports=%v", reports)
	}
	phase, ok := errors.AsType[*ProbeError](reports[1])
	if !ok || phase.Phase != "plain" || !phase.Retrying {
		t.Fatalf("retry phase absent: %v", reports[1])
	}
}

func TestProbeCompatibilityPersistentTransportFailureIsBounded(t *testing.T) {
	calls := 0
	reports := 0
	c := Client{BaseURL: "https://provider.example.invalid/v1", HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, io.EOF
	})}}
	parameters, err := c.ProbeCompatibility(t.Context(), func(error) { reports++ })
	if parameters != nil || !errors.Is(err, io.EOF) || calls != 2 || reports != 2 {
		t.Fatalf("parameters=%v error=%v calls=%d reports=%d", parameters, err, calls, reports)
	}
}

func TestProbeCompatibilityRetriesOnlyJSONPhase(t *testing.T) {
	var requests []map[string]any
	c := Client{BaseURL: "https://provider.example.invalid/v1", HTTP: &http.Client{Transport: transportFunc(func(request *http.Request) (*http.Response, error) {
		body := readChatRequest(t, request)
		requests = append(requests, body)
		if len(requests) == 2 {
			return nil, syscall.ECONNRESET
		}
		recorder := httptest.NewRecorder()
		writeCompatibilityAnswer(recorder, body)
		return recorder.Result(), nil
	})}}
	reports := 0
	parameters, err := c.ProbeCompatibility(t.Context(), func(err error) {
		reports++
		phase, ok := errors.AsType[*ProbeError](err)
		if !ok || phase.Phase != "json" || !phase.Retrying {
			t.Errorf("JSON retry phase missing: %v", err)
		}
	})
	if err != nil || len(parameters) != 0 || len(requests) != 3 || reports != 1 || !reflect.DeepEqual(requests[1], requests[2]) {
		t.Fatalf("parameters=%v error=%v requests=%d reports=%d", parameters, err, len(requests), reports)
	}
}

type temporaryProbeError struct{}

func (temporaryProbeError) Error() string   { return "temporary network failure" }
func (temporaryProbeError) Temporary() bool { return true }
func (temporaryProbeError) Timeout() bool   { return false }

func TestProbeCompatibilityTransientTransportPolicy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"reset", syscall.ECONNRESET, true},
		{"temporary", temporaryProbeError{}, true},
		{"certificate", x509.UnknownAuthorityError{}, false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"other", errors.New("permanent transport failure"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := Client{BaseURL: "https://provider.example.invalid/v1", HTTP: &http.Client{Transport: transportFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return nil, tc.err
				}
				return probeTransportResponse(t, request)
			})}}
			_, err := c.ProbeCompatibility(t.Context(), nil)
			if tc.retry {
				if err != nil || calls != 3 {
					t.Fatalf("error=%v calls=%d", err, calls)
				}
			} else if err == nil || calls != 1 {
				t.Fatalf("non-transient failure retried: error=%v calls=%d", err, calls)
			}
		})
	}
}

func TestProbeCompatibilityCancellationStopsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	c := Client{BaseURL: "https://provider.example.invalid/v1", HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, io.EOF
	})}}
	_, err := c.ProbeCompatibility(ctx, func(error) { cancel() })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

type failedProbeBody struct{}

func (failedProbeBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failedProbeBody) Close() error             { return nil }

func TestProbeCompatibilityRetriesInterruptedResponseBody(t *testing.T) {
	calls := 0
	c := Client{BaseURL: "https://provider.example.invalid/v1", HTTP: &http.Client{Transport: transportFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 200, Body: failedProbeBody{}}, nil
		}
		return probeTransportResponse(t, request)
	})}}
	parameters, err := c.ProbeCompatibility(t.Context(), nil)
	if err != nil || len(parameters) != 0 || calls != 3 {
		t.Fatalf("parameters=%v error=%v calls=%d", parameters, err, calls)
	}
}
