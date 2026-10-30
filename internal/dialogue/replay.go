package dialogue

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"quasarbridge/internal/llm"
)

type replayState struct {
	turns []turn
	at    time.Time
}

// ReplaySnapshot keeps the previous answer for eligibility checks only. Generation
// receives Messages followed by Question, as if that question first chose the LLM.
type ReplaySnapshot struct {
	Messages       []llm.Message `json:"context"`
	Question       string        `json:"question"`
	PreviousAnswer string        `json:"previous_answer"`
	PrivateHistory bool          `json:"private_history"`
}

// SkipEmpty preserves a completed target when the recognizer confirms silence.
// An unfinished question still invalidates replay unless this hook is called.
func (s *Store) SkipEmpty(ctx context.Context, ticket Ticket) {
	if s == nil || !s.cfg.Enabled || ticket.replay == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.current(ctx, ticket) || s.now().Sub(ticket.replay.at) >= time.Duration(s.cfg.TTLSeconds)*time.Second {
		return
	}
	e := s.entries[ticket.key]
	if e.replayInvalid || e.replayPartial != "" {
		return
	}
	e.replay = slices.Clone(ticket.replay.turns)
	e.replayReady = true
	e.pending = false
	e.at = ticket.replay.at
}

func (s *Store) Replay(ctx context.Context, ticket Ticket) (ReplaySnapshot, bool) {
	if s == nil || !s.cfg.Enabled || ticket.replay == nil {
		return ReplaySnapshot{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.current(ctx, ticket) || s.now().Sub(ticket.replay.at) >= time.Duration(s.cfg.TTLSeconds)*time.Second {
		return ReplaySnapshot{}, false
	}
	turns := ticket.replay.turns
	var out ReplaySnapshot
	for i, t := range turns {
		out.PrivateHistory = out.PrivateHistory || t.private
		if i == len(turns)-1 {
			out.Question = t.user
			out.PreviousAnswer = t.answer
			break
		}
		out.Messages = append(out.Messages, llm.Message{Role: "user", Content: t.user}, llm.Message{Role: "assistant", Content: t.answer})
	}
	return out, out.Question != ""
}

func (s *Store) trimReplay(turns []turn) []turn {
	chars := 0
	for _, t := range turns {
		chars += utf8.RuneCountInString(t.user) + utf8.RuneCountInString(t.answer)
	}
	first := 0
	for first < len(turns) && (len(turns)-first > s.cfg.MaxTurns || chars > s.cfg.MaxChars) {
		chars -= utf8.RuneCountInString(turns[first].user) + utf8.RuneCountInString(turns[first].answer)
		first++
	}
	if first > 0 {
		return slices.Clone(turns[first:])
	}
	return turns
}

// CaptureReplay is called only after a response was delivered. Intermediate
// streaming fragments stay pending and never become repeatable questions.
func (s *Store) CaptureReplay(ctx context.Context, ticket Ticket, question, text string, final, action, private, retry bool) {
	if s == nil || !s.cfg.Enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.current(ctx, ticket) {
		return
	}
	e := s.entries[ticket.key]
	if action {
		e.replayInvalid = true
		e.replay = nil
		e.replayReady = false
	}
	if e.replayReady {
		return
	}
	if retry && !e.retryAnswered {
		if !final {
			return
		}
		e.pending = false
		if ticket.replay != nil {
			e.replay = slices.Clone(ticket.replay.turns)
			e.replayReady = true
			e.at = ticket.replay.at
		}
		return
	}
	if strings.HasPrefix(text, e.replayPartial) {
		e.replayPartial = text
	} else {
		e.replayPartial += text
	}
	if utf8.RuneCountInString(e.replayPartial)+utf8.RuneCountInString(question) > s.cfg.MaxChars {
		e.replayInvalid = true
		e.replayPartial = ""
		e.replay = nil
	}
	if !final {
		return
	}
	e.pending = false
	if e.replayInvalid || strings.TrimSpace(e.replayPartial) == "" || strings.TrimSpace(question) == "" {
		return
	}
	if retry {
		if ticket.replay == nil {
			return
		}
		e.replay = slices.Clone(ticket.replay.turns[:len(ticket.replay.turns)-1])
		question = ticket.replay.turns[len(ticket.replay.turns)-1].user
	}
	e.replay = append(e.replay, turn{user: question, answer: e.replayPartial, private: private})
	e.replay = s.trimReplay(e.replay)
	e.replayReady = len(e.replay) > 0
	e.replayPartial = ""
	e.at = s.now()
}

func (s *Store) CommitRetry(ctx context.Context, ticket Ticket, answer string) bool {
	if s == nil || !s.cfg.Enabled || ticket.replay == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.current(ctx, ticket) {
		return false
	}
	e := s.entries[ticket.key]
	turns := slices.Clone(ticket.replay.turns)
	turns[len(turns)-1].answer = answer
	e.turns = s.trimReplay(turns)
	e.retryAnswered = true
	e.pending = false
	e.at = s.now()
	return true
}

func (s *Store) ClearReplay(ctx context.Context, ticket Ticket) bool {
	if s == nil || !s.cfg.Enabled {
		return ctx.Err() == nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.current(ctx, ticket) {
		return false
	}
	e := s.entries[ticket.key]
	e.replay = nil
	e.replayReady = false
	e.replayPartial = ""
	e.replayInvalid = true
	return true
}
