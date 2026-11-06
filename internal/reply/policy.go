// Package reply carries delivery policy for the current speaker request.
package reply

import (
	"context"
	"sync/atomic"
)

type policyKey struct{}

type policy struct {
	suppressed atomic.Bool
	delivery   context.Context
	delivered  context.CancelFunc
}

// WithPolicy creates an independent policy for one request. Derived tool and
// resolver contexts share it, including when a later step returns an error.
func WithPolicy(ctx context.Context) context.Context {
	delivery, delivered := context.WithCancel(ctx)
	return context.WithValue(ctx, policyKey{}, &policy{delivery: delivery, delivered: delivered})
}

// SuppressListening retains spoken replies while preventing microphone reopen.
func SuppressListening(ctx context.Context) {
	if p, ok := ctx.Value(policyKey{}).(*policy); ok {
		p.suppressed.Store(true)
	}
}

func ListeningAllowed(ctx context.Context) bool {
	p, ok := ctx.Value(policyKey{}).(*policy)
	return !ok || !p.suppressed.Load()
}

// DeliveryContext outlives the model's tool context. It ends when the final
// speech stream has been forwarded, or the original speaker request ends (the
// native request has a 90-second timeout). CLI requests use their own context.
func DeliveryContext(ctx context.Context) context.Context {
	if p, ok := ctx.Value(policyKey{}).(*policy); ok {
		return p.delivery
	}
	return ctx
}

func Delivered(ctx context.Context) {
	if p, ok := ctx.Value(policyKey{}).(*policy); ok {
		p.delivered()
	}
}
