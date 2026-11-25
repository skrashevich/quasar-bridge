// Package dialogue keeps bounded, short-lived conversational context in RAM.
package dialogue

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/skrashevich/quasar-bridge/internal/llm"
)

type Config struct {
	Enabled    bool `json:"enabled"`
	TTLSeconds int  `json:"ttl_seconds"`
	MaxTurns   int  `json:"max_turns"`
	MaxChars   int  `json:"max_chars"`
}

func DefaultConfig() Config {
	return Config{Enabled: true, TTLSeconds: 300, MaxTurns: 6, MaxChars: 12000}
}
func (c Config) Validate() error {
	if c.TTLSeconds < 30 || c.TTLSeconds > 86400 || c.MaxTurns < 1 || c.MaxTurns > 20 || c.MaxChars < 512 || c.MaxChars > 65536 {
		return errors.New("invalid dialogue_context limits")
	}
	return nil
}

type speakerKey struct{}

// WithSpeaker scopes context without retaining the raw device identifier.
func WithSpeaker(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, speakerKey{}, fmt.Sprintf("%x", sha256.Sum256([]byte(id))))
}
func key(ctx context.Context) string { s, _ := ctx.Value(speakerKey{}).(string); return s }

// SpeakerKey returns the anonymous key shared by this speaker's requests.
func SpeakerKey(ctx context.Context) string { return key(ctx) }

type turn struct {
	user, answer string
	route        string
	private      bool
}
type homeQuestion struct {
	history []llm.Message
	until   time.Time
}
type entry struct {
	turns         []turn
	home          homeQuestion
	replay        []turn
	replayReady   bool
	replayPartial string
	replayInvalid bool
	retryAnswered bool
	at            time.Time
	generation    uint64
	pending       bool
}
type Snapshot struct {
	Messages       []llm.Message
	PrivateHistory bool
	Route          string
}
type Ticket struct {
	key        string
	generation uint64
	replay     *replayState
}
type requestKey struct{}
type requestState struct {
	store  *Store
	ticket Ticket
}
type Store struct {
	cfg        Config
	mu         sync.Mutex
	entries    map[string]*entry
	generation uint64
	now        func() time.Time
}

func New(cfg Config) *Store { return &Store{cfg: cfg, entries: map[string]*entry{}, now: time.Now} }
func (s *Store) expired(e *entry) bool {
	return s.now().Sub(e.at) >= time.Duration(s.cfg.TTLSeconds)*time.Second
}
func snapshot(e *entry) Snapshot {
	var v Snapshot
	if e != nil {
		for _, t := range e.turns {
			v.Messages = append(v.Messages, llm.Message{Role: "user", Content: t.user}, llm.Message{Role: "assistant", Content: t.answer})
			v.PrivateHistory = v.PrivateHistory || t.private
			v.Route = t.route
		}
	}
	if len(v.Messages) > 0 && v.Route == "" {
		v.Route = "llm"
	}
	return v
}
func (s *Store) View(ctx context.Context) Snapshot {
	if s == nil || !s.cfg.Enabled {
		return Snapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key(ctx)]
	if e != nil && s.expired(e) {
		e.turns = nil
		e.replay = nil
		e.replayReady = false
		if !e.pending && !s.now().Before(e.home.until) {
			delete(s.entries, key(ctx))
			e = nil
		}
	}
	return snapshot(e)
}
func (s *Store) Begin(ctx context.Context) (Snapshot, Ticket) {
	if s == nil {
		return Snapshot{}, Ticket{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.entries {
		if s.expired(e) {
			e.turns = nil
			e.replay = nil
			e.replayReady = false
			if !e.pending && !s.now().Before(e.home.until) {
				delete(s.entries, k)
			}
		}
	}
	k := key(ctx)
	e := s.entries[k]
	if e == nil {
		if len(s.entries) >= 128 {
			var oldest string
			var at time.Time
			for k, e := range s.entries {
				if at.IsZero() || e.at.Before(at) {
					oldest, at = k, e.at
				}
			}
			delete(s.entries, oldest)
		}
		e = new(entry)
		s.entries[k] = e
	}
	if s.expired(e) {
		e.turns = nil
		e.replay = nil
		e.replayReady = false
	}
	var replay *replayState
	if e.replayReady && len(e.replay) > 0 {
		replay = &replayState{turns: slices.Clone(e.replay), at: e.at}
	}
	e.replayReady = false
	e.replayPartial = ""
	e.replayInvalid = false
	e.retryAnswered = false
	e.pending = true
	s.generation++
	e.generation = s.generation
	e.at = s.now()
	return snapshot(e), Ticket{key: k, generation: e.generation, replay: replay}
}
func (s *Store) Commit(ctx context.Context, ticket Ticket, user, answer string, private bool) bool {
	return s.CommitRoute(ctx, ticket, "llm", user, answer, private)
}

// CommitRoute keeps the handler that owns the next elliptical reply with its history.
func (s *Store) CommitRoute(ctx context.Context, ticket Ticket, route, user, answer string, private bool) bool {
	return s.commitRoute(ctx, ticket, route, user, answer, private, nil)
}

func (s *Store) CommitHome(ctx context.Context, ticket Ticket, user, answer string, history []llm.Message) bool {
	return s.commitRoute(ctx, ticket, "home", user, answer, false, history)
}

func (s *Store) commitRoute(ctx context.Context, ticket Ticket, route, user, answer string, private bool, homeHistory []llm.Message) bool {
	if s == nil || (!s.cfg.Enabled && route != "home") {
		return ctx.Err() == nil
	}
	if ticket.generation == 0 || key(ctx) != ticket.key || ctx.Err() != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[ticket.key]
	if e == nil || e.generation != ticket.generation {
		return false
	}
	e.pending = false
	e.at = s.now()
	e.home = homeQuestion{}
	if route == "home" && IsHomeQuestion(answer) {
		history := append(slices.Clone(homeHistory), llm.Message{Role: "user", Content: user}, llm.Message{Role: "assistant", Content: answer})
		// Keep the original request and the most recent answer when the chain grows.
		if max := 2 * max(2, s.cfg.MaxTurns); len(history) > max {
			history = append(history[:2], history[len(history)-max+2:]...)
		}
		chars := 0
		for _, message := range history {
			chars += utf8.RuneCountInString(message.Content)
		}
		if chars <= s.cfg.MaxChars {
			e.home = homeQuestion{history: history, until: e.at.Add(45 * time.Second)}
		}
	}
	if !s.cfg.Enabled {
		return true
	}
	if strings.TrimSpace(answer) == "" {
		return true
	}
	if utf8.RuneCountInString(user)+utf8.RuneCountInString(answer) > s.cfg.MaxChars {
		e.turns = nil
		return true
	}
	e.turns = append(e.turns, turn{user: user, answer: answer, route: route, private: private})
	chars := 0
	for _, t := range e.turns {
		chars += utf8.RuneCountInString(t.user) + utf8.RuneCountInString(t.answer)
	}
	first := 0
	for len(e.turns)-first > s.cfg.MaxTurns || chars > s.cfg.MaxChars {
		chars -= utf8.RuneCountInString(e.turns[first].user) + utf8.RuneCountInString(e.turns[first].answer)
		first++
	}
	if first > 0 {
		e.turns = slices.Clone(e.turns[first:])
	}
	return true
}

// IsHomeQuestion identifies answers that request a Home clarification.
func IsHomeQuestion(answer string) bool {
	return strings.Contains(answer, "?") || strings.Contains(strings.ToLower(answer), "уточни")
}

// TakeHomeQuestion discards a clarification when the speaker changes topics.
func (s *Store) TakeHomeQuestion(ctx context.Context, ticket Ticket) bool {
	return len(s.TakeHomeDialogue(ctx, ticket)) > 0
}

// PeekHomeQuestion leaves a clarification available until the route is executed.
func (s *Store) PeekHomeQuestion(ctx context.Context, ticket Ticket) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.current(ctx, ticket) {
		return ""
	}
	q := s.entries[ticket.key].home
	if !s.now().Before(q.until) {
		return ""
	}
	return q.history[len(q.history)-1].Content
}

// TakeHomeDialogue consumes the clarification and checks its deadline again.
func (s *Store) TakeHomeDialogue(ctx context.Context, ticket Ticket) []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.current(ctx, ticket) {
		return nil
	}
	e := s.entries[ticket.key]
	q := e.home
	e.home = homeQuestion{}
	if !s.now().Before(q.until) {
		return nil
	}
	return q.history
}

func (s *Store) Reset(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key(ctx))
}
func Normalize(text string) string {
	return strings.Trim(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(text)), "ё", "е"), " .,!?:;«»\"")
}
func IsReset(text string) bool {
	switch Normalize(text) {
	case "начни новый разговор", "начнем новый разговор", "забудь этот разговор", "сбрось контекст", "новый разговор":
		return true
	}
	return false
}

// IsFollowup recognizes elliptical turns; explicit routing rules take priority.
func IsFollowup(text string) bool {
	text = Normalize(text)
	switch text {
	case "да", "нет", "почему", "зачем", "повтори", "подробнее", "продолжай", "вчера", "сегодня", "позавчера":
		return true
	}
	for _, p := range []string{"а ", "и ", "еще ", "почему ", "зачем ", "повтори ", "расскажи подробнее", "объясни подробнее", "что из этого", "кто из них", "что ты имеешь в виду"} {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// Start reserves ordering before classification and is idempotent for one request.
func (s *Store) Start(ctx context.Context) (context.Context, Snapshot, Ticket) {
	if request, ok := ctx.Value(requestKey{}).(requestState); ok && request.store == s {
		return ctx, s.View(ctx), request.ticket
	}
	view, ticket := s.Begin(ctx)
	return s.Use(ctx, ticket), view, ticket
}
func (s *Store) Use(ctx context.Context, ticket Ticket) context.Context {
	return context.WithValue(ctx, requestKey{}, requestState{s, ticket})
}
func (s *Store) current(ctx context.Context, ticket Ticket) bool {
	e := s.entries[ticket.key]
	return ctx.Err() == nil && key(ctx) == ticket.key && e != nil && e.generation == ticket.generation
}
func (s *Store) Current(ctx context.Context, ticket Ticket) bool {
	if ctx.Err() != nil {
		return false
	}
	if !s.cfg.Enabled {
		return true
	}
	return s.CurrentTurn(ctx, ticket)
}

// CurrentTurn checks ordering even when ordinary dialogue history is disabled.
func (s *Store) CurrentTurn(ctx context.Context, ticket Ticket) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current(ctx, ticket)
}

// Clear resets the topic only if this request is still the latest one.
func (s *Store) Clear(ctx context.Context, ticket Ticket) bool {
	if !s.cfg.Enabled {
		return ctx.Err() == nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.current(ctx, ticket) {
		return false
	}
	s.entries[ticket.key].turns = nil
	s.entries[ticket.key].home = homeQuestion{}
	return true
}
