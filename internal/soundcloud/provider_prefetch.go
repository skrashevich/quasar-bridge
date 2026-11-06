package soundcloud

import (
	"context"
	"errors"
	"maps"
	"slices"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

type providerPrefetch struct {
	request   sound.Request
	operation playbackOperation
	revision  uint64
	delivered bool
}

func (s *providerSession) invalidatePrefetch() {
	s.prefetch = nil
	s.prefetchRevision++
}

func (p *Provider) currentPrefetchLocked(s *providerSession) *providerPrefetch {
	if s == nil || s.revoked || s.prefetch == nil || s.generation != s.committed {
		return nil
	}
	next := s.prefetch
	if next.operation.session != s || next.operation.generation != s.generation || next.revision != s.prefetchRevision || next.request.SessionID != s.id || next.request.AfterStreamID != s.stream {
		return nil
	}
	return next
}

func (p *Provider) canDeliverPrefetchLocked(s *providerSession, r sound.Request) bool {
	next := p.currentPrefetchLocked(s)
	return next != nil && r == next.request
}

// Prefetch resolves the next natural queue item for a native warm-up request.
// It does not reserve a playback mutation or change the currently playing item.
func (p *Provider) Prefetch(ctx context.Context, sessionID, streamID, token string) (bool, error) {
	if p == nil || !p.cfg.Enabled || ctx.Err() != nil {
		return true, ctx.Err()
	}
	key := dialogue.SpeakerKey(ctx)
	if key == "" || sessionID == "" || streamID == "" || token == "" {
		return true, nil
	}
	p.mu.Lock()
	s := p.sessionLocked(key, false)
	if next := p.currentPrefetchLocked(s); next != nil && sessionID == next.request.SessionID && streamID == next.request.StreamID && token == next.request.CallbackToken {
		p.promoteObservedPrefetchLocked(s, sound.CurrentPlayback(ctx))
	}
	valid := func() bool {
		return s != nil && !s.revoked && s.id == sessionID && s.stream == streamID && s.token == token && s.committed == s.generation && s.index >= 0 && s.index < len(s.queue) && !s.terminal && (s.status == "playing" || s.status == "pending")
	}
	if !valid() || p.revokeForeignLocked(ctx, s) || !valid() {
		p.mu.Unlock()
		return true, nil
	}
	// A deferred relay URL must remain leased until the current track ends.
	// Long mixes fall back to their native EOF event instead of staging a URL
	// hours before the speaker will use it.
	offset := int64(0)
	if s.offsetKnown {
		offset = max(0, s.offset)
	}
	state := sound.CurrentPlayback(ctx)
	if state.Present && state.StreamID == s.stream && state.OffsetKnown && state.OffsetMS >= 0 {
		offset = state.OffsetMS
	}
	if max(0, s.queue[s.index].DurationMS-offset) > (relayResourceTTL - providerPlaybackGrace).Milliseconds() {
		p.mu.Unlock()
		return true, nil
	}
	if next := p.currentPrefetchLocked(s); next != nil {
		err := sound.Play(ctx, next.request)
		p.mu.Unlock()
		return true, err
	}
	op := playbackOperation{speaker: key, session: s, generation: s.generation, queue: slices.Clone(s.queue), original: slices.Clone(s.original), index: s.index, offset: 0, id: s.id, repeat: s.repeat, shuffle: s.shuffle, failed: maps.Clone(s.failed)}
	revision := s.prefetchRevision
	nextIndex := providerNext(op, 1, true)
	if nextIndex < 0 {
		p.mu.Unlock()
		return true, nil
	}
	op.index = nextIndex
	p.mu.Unlock()
	if p.client == nil || p.relay == nil {
		return true, errors.New("soundcloud: воспроизведение недоступно")
	}
	var track Track
	var media Media
	var err error
	for range len(op.queue) {
		track = op.queue[op.index]
		err = ErrUnavailable
		if track.DurationMS > 0 {
			var upstream string
			upstream, err = p.client.Stream(ctx, track)
			if err == nil {
				media, err = p.registerStream(track, upstream)
			}
		}
		if !errors.Is(err, ErrUnavailable) {
			break
		}
		if op.failed == nil {
			op.failed = make(map[string]bool)
		}
		op.failed[track.URN] = true
		op.index = providerNext(op, 1, false)
		if op.index < 0 {
			return true, nil
		}
	}
	if err != nil {
		return true, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if !p.currentLocked(op) || s.prefetchRevision != revision || !valid() || p.revokeForeignLocked(ctx, s) || !valid() {
		return true, context.Canceled
	}
	if next := p.currentPrefetchLocked(s); next != nil {
		return true, sound.Play(ctx, next.request)
	}
	request := preparedSoundRequest(op, track, media)
	request.Prefetch, request.AfterStreamID = true, streamID
	if err := sound.Play(ctx, request); err != nil {
		return true, err
	}
	s.prefetch = &providerPrefetch{request: request, operation: op, revision: revision}
	return true, nil
}

// PrefetchDelivered is called only after the deferred AliceResponse is written.
func (p *Provider) PrefetchDelivered(ctx context.Context, request sound.Request) {
	if p == nil || !p.cfg.Enabled || !request.Prefetch {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessionLocked(dialogue.SpeakerKey(ctx), false)
	if p.canDeliverPrefetchLocked(s, request) {
		s.prefetch.delivered = true
	}
}

func (p *Provider) promotePrefetchLocked(s *providerSession, next *providerPrefetch) {
	op, r := next.operation, next.request
	s.queue, s.original, s.index = op.queue, op.original, op.index
	s.media = Media{URL: r.URL, ArtworkURL: r.ArtworkURL}
	previous := s.stream
	s.stream, s.token, s.status = r.StreamID, r.CallbackToken, "pending"
	s.offset, s.offsetKnown, s.started, s.terminal, s.at = 0, true, false, false, p.now()
	s.previousStreams = []string{previous}
	s.failed = op.failed
	s.invalidatePrefetch()
	p.renewPlaybackLocked(s)
}

func (p *Provider) promoteObservedPrefetchLocked(s *providerSession, state sound.PlaybackState) {
	next := p.currentPrefetchLocked(s)
	if next == nil || !state.Present || state.StreamID != next.request.StreamID {
		return
	}
	p.promotePrefetchLocked(s, next)
}

// Returns true when a prepared-stream callback was consumed without promoting.
func (p *Provider) handlePreparedLifecycleLocked(ctx context.Context, s *providerSession, sid, stream, token, action string) bool {
	next := p.currentPrefetchLocked(s)
	if next == nil || sid != next.request.SessionID || stream != next.request.StreamID || token != next.request.CallbackToken {
		return false
	}
	state := sound.CurrentPlayback(ctx)
	if state.Present && state.StreamID != "" && state.StreamID != s.stream && state.StreamID != stream {
		p.revokeForeignLocked(ctx, s)
		return true
	}
	if action == "started" || state.Present && state.StreamID == stream || action == "failed" && s.terminal && s.status == "waiting" {
		p.promotePrefetchLocked(s, next)
		return false
	}
	if action == "failed" {
		if s.failed == nil {
			s.failed = make(map[string]bool)
		}
		s.failed[next.request.TrackID] = true
		s.invalidatePrefetch()
	}
	return true
}

func (p *Provider) awaitDeliveredPrefetchLocked(s *providerSession) bool {
	next := p.currentPrefetchLocked(s)
	if next == nil || !next.delivered {
		return false
	}
	s.status, s.terminal, s.at = "waiting", true, p.now()
	return true
}
