// Package speaker turns the station's local voice classification into a short
// hint about who is talking. Only age group, gender, their confidence and the
// guest flag are retained: voiceprint ids and guest credentials are never read.
package speaker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	"github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/guest"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
)

const (
	// TTL bounds how long a classification may be matched to a request.
	TTL = 120 * time.Second
	// FallbackAge bounds the "latest MatchedUser" fallback used when the
	// request id of the voice request does not match.
	FallbackAge = 30 * time.Second
	// MaxEntries bounds the memory held for one connection.
	MaxEntries = 64

	// highConfidence is the score assumed for a classifier verdict that carries
	// only the discrete "high" level and no probability.
	highConfidence = 0.9

	tagAge    = "children"
	tagGender = "gender"
)

// Context is the classification of one voice request.
type Context struct {
	Age              string // "adult", "child" or empty.
	AgeConfidence    float64
	Gender           string // "male", "female" or empty.
	GenderConfidence float64
	Guest            bool // The station did not report NoMatch.
	RequestID        string
}

// Parse extracts the classification from a MatchedUser event. It reports false
// when the frame has no usable age or gender.
func Parse(msg *client.TClientMessage) (Context, bool) {
	mu := msg.GetEvent().GetMatchedUser()
	if mu == nil {
		return Context{}, false
	}
	bc := mu.GetRequest().GetEvent().GetBiometryClassification()
	c := Context{RequestID: mu.GetHeader().GetRequestId()}
	c.Age, c.AgeConfidence = pick(bc, tagAge)
	c.Gender, c.GenderConfidence = pick(bc, tagGender)
	if c.Age == "" && c.Gender == "" {
		return Context{}, false
	}
	// Only the status enum is read; tokens and ids next to it stay untouched.
	status := mu.GetRequest().GetAdditionalOptions().GetGuestUserOptions().GetStatus()
	c.Guest = status != guest.TGuestOptions_NoMatch
	return c, true
}

// pick prefers the classifier's own verdict (Simple) and falls back to the
// highest score of the group. The probability always comes from Scores when the
// verdict class is listed there.
func pick(bc *common.TBiometryClassification, tag string) (string, float64) {
	score := func(class string) (float64, bool) {
		for _, s := range bc.GetScores() {
			if s.GetTag() == tag && s.GetClassName() == class {
				return s.GetConfidence(), true
			}
		}
		return 0, false
	}
	for _, s := range bc.GetSimple() {
		if s.GetTag() != tag || s.GetClassName() == "" {
			continue
		}
		if p, ok := score(s.GetClassName()); ok {
			return s.GetClassName(), p
		}
		if s.GetConfidence() == common.TBiometryClassification_high {
			return s.GetClassName(), highConfidence
		}
		return s.GetClassName(), 0
	}
	var class string
	var best float64
	for _, s := range bc.GetScores() {
		if s.GetTag() == tag && s.GetClassName() != "" && (class == "" || s.GetConfidence() > best) {
			class, best = s.GetClassName(), s.GetConfidence()
		}
	}
	return class, best
}

type entry struct {
	value Context
	at    time.Time
}

// Store keeps recent classifications of one connection by request id.
type Store struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]entry
	latest  string
	warned  bool
}

func NewStore() *Store {
	return &Store{now: time.Now, entries: map[string]entry{}}
}

// Record remembers the classification, evicting expired and then oldest entries.
func (s *Store) Record(c Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, e := range s.entries {
		if now.Sub(e.at) > TTL {
			delete(s.entries, id)
		}
	}
	for len(s.entries) >= MaxEntries {
		oldest, first := "", true
		for id, e := range s.entries {
			if first || e.at.Before(s.entries[oldest].at) {
				oldest, first = id, false
			}
		}
		delete(s.entries, oldest)
	}
	s.entries[c.RequestID] = entry{value: c, at: now}
	s.latest = c.RequestID
}

// Lookup finds the classification of a request, falling back to the latest
// recent one of this connection.
func (s *Store) Lookup(requestID string) (Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if e, ok := s.entries[requestID]; ok && requestID != "" && now.Sub(e.at) <= TTL {
		return e.value, true
	}
	if e, ok := s.entries[s.latest]; ok && now.Sub(e.at) <= FallbackAge {
		return e.value, true
	}
	return Context{}, false
}

// WarnOnce logs at debug level the first time a request has no classification.
func (s *Store) WarnOnce() {
	s.mu.Lock()
	first := !s.warned
	s.warned = true
	s.mu.Unlock()
	if first {
		slog.Debug("speaker_context_unavailable")
	}
}

type lookupKey struct{}

// WithLookup attaches a lazy lookup to a request context. The classification
// may arrive after the voice request, so it is resolved when the prompt is built.
func WithLookup(ctx context.Context, lookup func() (Context, bool)) context.Context {
	return context.WithValue(ctx, lookupKey{}, lookup)
}

// FromContext returns the classification attached to the request, if any.
func FromContext(ctx context.Context) (Context, bool) {
	lookup, _ := ctx.Value(lookupKey{}).(func() (Context, bool))
	if lookup == nil {
		return Context{}, false
	}
	return lookup()
}
