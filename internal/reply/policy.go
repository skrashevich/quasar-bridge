// Package reply carries delivery policy for the current speaker request.
package reply

import (
	"context"
	"sync/atomic"
)

type policyKey struct{}

// WithPolicy creates an independent policy for one request. Derived tool and
// resolver contexts share it, including when a later step returns an error.
func WithPolicy(ctx context.Context) context.Context {
	return context.WithValue(ctx, policyKey{}, new(atomic.Bool))
}

// SuppressListening retains spoken replies while preventing microphone reopen.
func SuppressListening(ctx context.Context) {
	if suppressed, ok := ctx.Value(policyKey{}).(*atomic.Bool); ok {
		suppressed.Store(true)
	}
}

func ListeningAllowed(ctx context.Context) bool {
	suppressed, ok := ctx.Value(policyKey{}).(*atomic.Bool)
	return !ok || !suppressed.Load()
}
