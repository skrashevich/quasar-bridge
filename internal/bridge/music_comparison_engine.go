package bridge

import (
	"context"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/musicmatch"
	"quasarbridge/internal/sound"
	"time"
)

type MusicComparisonProvider interface {
	ComparisonEnabled(context.Context, string) bool
	Prepare(context.Context, string) (musicmatch.Prepared, error)
	IsDefault() bool
	AcceptYandex(context.Context) error
}
type comparisonTicket struct {
	sequence       uint64
	at             time.Time
	ctx            context.Context
	cancel         context.CancelFunc
	protected      bool
	yandexSequence uint64
}
type comparisonContextKey struct{}

// Comparison tickets do not cancel ordinary provider queues or dialogue work.
// Only the derived comparison context is superseded by a newer user input.
func (e *Engine) prepareComparisonInput(ctx context.Context) context.Context {
	key := dialogue.SpeakerKey(ctx)
	if key == "" {
		return ctx
	}
	sequence := sound.RequestSequence(ctx)
	e.comparisonMu.Lock()
	defer e.comparisonMu.Unlock()
	if e.comparisonInputs == nil {
		e.comparisonInputs = map[string]*comparisonTicket{}
	}
	now := time.Now()
	for k, t := range e.comparisonInputs {
		if now.Sub(t.at) > 2*time.Minute {
			t.cancel()
			delete(e.comparisonInputs, k)
		}
	}
	previous := e.comparisonInputs[key]
	cctx, cancel := context.WithCancel(ctx)
	ticket := &comparisonTicket{sequence: sequence, at: now, ctx: cctx, cancel: cancel}
	if previous != nil {
		ticket.protected = previous.protected
		ticket.yandexSequence = previous.yandexSequence
	}
	if previous != nil && sequence <= previous.sequence {
		cancel()
		return context.WithValue(ctx, comparisonContextKey{}, ticket)
	}
	if previous != nil {
		previous.cancel()
	}
	if len(e.comparisonInputs) >= 128 && previous == nil {
		var oldestKey string
		var oldest *comparisonTicket
		for k, t := range e.comparisonInputs {
			if oldest == nil || t.at.Before(oldest.at) {
				oldestKey, oldest = k, t
			}
		}
		if oldest != nil {
			oldest.cancel()
			delete(e.comparisonInputs, oldestKey)
		}
	}
	e.comparisonInputs[key] = ticket
	return context.WithValue(ctx, comparisonContextKey{}, ticket)
}

func (e *Engine) MarkYandexAccepted(ctx context.Context) {
	t, _ := ctx.Value(comparisonContextKey{}).(*comparisonTicket)
	e.comparisonMu.Lock()
	defer e.comparisonMu.Unlock()
	if t != nil && e.comparisonInputs[dialogue.SpeakerKey(ctx)] == t && t.ctx.Err() == nil {
		t.yandexSequence = t.sequence
	}
}
func (e *Engine) YandexAcceptedAfter(ctx context.Context) bool {
	t, _ := ctx.Value(comparisonContextKey{}).(*comparisonTicket)
	e.comparisonMu.Lock()
	defer e.comparisonMu.Unlock()
	current := e.comparisonInputs[dialogue.SpeakerKey(ctx)]
	return t != nil && current != nil && current.yandexSequence > t.sequence
}

func (e *Engine) ArmMusicComparison(ctx context.Context) bool {
	t, _ := ctx.Value(comparisonContextKey{}).(*comparisonTicket)
	e.comparisonMu.Lock()
	defer e.comparisonMu.Unlock()
	if t == nil || t.ctx.Err() != nil || e.comparisonInputs[dialogue.SpeakerKey(ctx)] != t {
		return false
	}
	t.protected = true
	return true
}
func (e *Engine) ComparisonProtected(ctx context.Context) bool {
	t, _ := ctx.Value(comparisonContextKey{}).(*comparisonTicket)
	e.comparisonMu.Lock()
	defer e.comparisonMu.Unlock()
	return t != nil && t.protected
}
func (e *Engine) ComparisonCurrent(ctx context.Context) bool {
	t, _ := ctx.Value(comparisonContextKey{}).(*comparisonTicket)
	if t == nil || t.ctx.Err() != nil || ctx.Err() != nil {
		return false
	}
	e.comparisonMu.Lock()
	defer e.comparisonMu.Unlock()
	return e.comparisonInputs[dialogue.SpeakerKey(ctx)] == t
}
func (e *Engine) ComparisonContext(ctx context.Context) context.Context {
	if t, ok := ctx.Value(comparisonContextKey{}).(*comparisonTicket); ok {
		return context.WithValue(t.ctx, comparisonContextKey{}, t)
	}
	return ctx
}
func (e *Engine) PrepareMusicComparison(ctx context.Context, text string) (musicmatch.Prepared, error) {
	p, ok := e.SoundCloud.(MusicComparisonProvider)
	if !ok || !e.ComparisonCurrent(ctx) {
		return nil, context.Canceled
	}
	return p.Prepare(ctx, text)
}
func (e *Engine) PreferSoundCloud() bool {
	p, ok := e.SoundCloud.(MusicComparisonProvider)
	return ok && p.IsDefault()
}
func (e *Engine) AcceptYandex(ctx context.Context) error {
	if !e.ComparisonCurrent(ctx) {
		return context.Canceled
	}
	if p, ok := e.SoundCloud.(MusicComparisonProvider); ok {
		return p.AcceptYandex(ctx)
	}
	return nil
}
