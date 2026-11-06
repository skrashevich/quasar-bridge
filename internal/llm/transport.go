package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"
	"syscall"
)

// TransportError preserves the underlying error for classification while keeping
// ordinary logs free of endpoint URLs and credentials. Explicit diagnostics may
// call Diagnostic to display the useful network cause.
type TransportError struct {
	summary string
	cause   error
	detail  string
}

func (e *TransportError) Error() string      { return e.summary }
func (e *TransportError) Unwrap() error      { return e.cause }
func (e *TransportError) Diagnostic() string { return e.detail }

var diagnosticURL = regexp.MustCompile(`https?://[^\s"'<>]+`)

func newTransportError(summary string, cause error, endpoint, credential string) *TransportError {
	inner := cause
	for {
		failure, ok := errors.AsType[*url.Error](inner)
		if !ok {
			break
		}
		inner = failure.Err
	}
	detail := inner.Error()
	if endpoint != "" {
		detail = strings.ReplaceAll(detail, endpoint, "[endpoint]")
	}
	detail = diagnosticURL.ReplaceAllString(detail, "[endpoint]")
	if credential != "" {
		for _, value := range []string{credential, url.QueryEscape(credential), url.PathEscape(credential)} {
			detail = strings.ReplaceAll(detail, value, "[credential]")
		}
	}
	return &TransportError{summary: summary, cause: cause, detail: detail}
}

func transientTransportError(err error) bool {
	if _, ok := errors.AsType[*TransportError](err); !ok || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	if failure, ok := errors.AsType[net.Error](err); ok {
		return failure.Temporary() && !failure.Timeout()
	}
	return false
}
