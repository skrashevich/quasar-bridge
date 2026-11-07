// Package identity carries SDK-reported speaker metadata for one request.
package identity

import (
	"context"
	"sync/atomic"
)

// Snapshot contains no credentials or raw profile identifiers. IDs are
// domain-separated SHA256 values of PersId; names come from that same profile's
// first name. Candidate fields express a hypothesis, never an established ID.
type Snapshot struct {
	Status         string `json:"status,omitempty"`
	ID             string `json:"id,omitempty"`
	Name           string `json:"name,omitempty"`
	CandidateID    string `json:"candidate_id,omitempty"`
	CandidateName  string `json:"candidate_name,omitempty"`
	Source         string `json:"source,omitempty"`
	Confidence     string `json:"confidence,omitempty"`
	Scoring        bool   `json:"scoring,omitzero"`
	Classification bool   `json:"classification,omitzero"`
	CandidateCount int    `json:"candidate_count,omitzero"`
}

type contextKey struct{}
type state struct{ current atomic.Pointer[Snapshot] }

// WithState starts an independent state. Derived contexts share its updates.
func WithState(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextKey{}, new(state))
}

func Update(ctx context.Context, snapshot Snapshot) {
	if s, ok := ctx.Value(contextKey{}).(*state); ok {
		s.current.Store(new(snapshot))
	}
}

// FromContext returns a value copy, so callers cannot mutate the shared state.
func FromContext(ctx context.Context) Snapshot {
	if s, ok := ctx.Value(contextKey{}).(*state); ok {
		if value := s.current.Load(); value != nil {
			return *value
		}
	}
	return Snapshot{}
}
