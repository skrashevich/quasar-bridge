package soundcloud

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	mathrand "math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

const EventPrefix = "music_thin_client_on_"
const providerTTL = 2 * time.Hour
const providerMaxSpeakers = 128
const providerMaxPendingStreams = 32
const providerPlaybackGrace = 5 * time.Minute
const providerMaxPlaybackLease = 24 * time.Hour

// Provider owns a bounded music session per speaker. It never retains a request
// context or the native player's request-scoped closure.
type Provider struct {
	cfg             Config
	defaultProvider atomic.Bool
	client          *Client
	relay           *Relay
	mu              sync.Mutex
	sessions        map[string]*providerSession
	now             func() time.Time
}
type providerSession struct {
	media                                            Media
	prefetch                                         *providerPrefetch
	prefetchRevision                                 uint64
	at, activeUntil                                  time.Time
	generation, committed, latestSequence            uint64
	id, stream, token, status, repeat                string
	queue, original, results                         []Track
	previousStreams                                  []string
	index                                            int
	offset                                           int64
	offsetKnown, shuffle, terminal, started, revoked bool
	failed                                           map[string]bool
}
type playbackOperation struct {
	speaker         string
	session         *providerSession
	generation      uint64
	queue, original []Track
	index           int
	offset          int64
	id, repeat      string
	shuffle         bool
	failed          map[string]bool
}

func NewProvider(cfg Config, client *Client, relay *Relay) *Provider {
	cfg.Defaults()
	cfg.QueueLimit = min(100, max(1, cfg.QueueLimit))
	p := &Provider{cfg: cfg, client: client, relay: relay, sessions: make(map[string]*providerSession), now: time.Now}
	p.defaultProvider.Store(cfg.Default)
	return p
}

// SetDefault affects future unqualified catalog requests without touching queues.
// The application persists the preference before publishing it here.
func (p *Provider) SetDefault(value bool) { p.defaultProvider.Store(value) }
func (p *Provider) IsDefault() bool       { return p != nil && p.defaultProvider.Load() }

func (p *Provider) CanHandle(ctx context.Context, text string) bool {
	return p.canHandleCommand(ctx, parseProviderCommand(text))
}

// CanControl admits native controls only while this speaker owns the queue.
func (p *Provider) CanControl(ctx context.Context, control sound.Control) bool {
	return p.canHandleCommand(ctx, providerControlCommand(control))
}

func providerControlCommand(control sound.Control) providerCommand {
	switch control {
	case sound.ControlNext, sound.ControlPrevious:
		return providerCommand{action: string(control), native: true}
	case sound.ControlResume:
		return providerCommand{action: string(control)}
	default:
		return providerCommand{}
	}
}

func (p *Provider) canHandleCommand(ctx context.Context, c providerCommand) bool {
	if p == nil || !p.cfg.Enabled || ctx.Err() != nil || dialogue.SpeakerKey(ctx) == "" {
		return false
	}
	if c.action == "" || isCatalogCommand(c.action) && !c.explicit && !p.IsDefault() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessionLocked(dialogue.SpeakerKey(ctx), false)
	if sequence := sound.RequestSequence(ctx); s != nil && sequence != 0 && sequence < s.latestSequence {
		// Consume a stale music request; Handle will reject it without even
		// applying playback observations carried by the older request.
		return true
	}
	p.revokeForeignLocked(ctx, s)
	if c.action == "select" {
		return s != nil && c.number <= len(s.results)
	}
	if isCatalogCommand(c.action) {
		return c.explicit || p.IsDefault()
	}
	if c.explicit {
		return true
	}
	return p.ownsCommandLocked(ctx, s, c)
}

// CanDeliver validates queued audio at the final native delivery boundary.
// A new reserved mutation supersedes older queued work even before it resolves.
func (p *Provider) CanDeliver(ctx context.Context, request sound.Request) bool {
	if request.Provider != "SoundCloud" {
		return true
	}
	if p == nil || !p.cfg.Enabled || ctx.Err() != nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessionLocked(dialogue.SpeakerKey(ctx), false)
	if s == nil || s.revoked || request.Generation == 0 || request.SessionID != s.id || request.Generation != s.generation || request.Generation != s.committed {
		return false
	}
	if request.Prefetch {
		return !p.revokeForeignLocked(ctx, s) && p.canDeliverPrefetchLocked(s, request)
	}
	if request.Command != "" {
		return (request.Command == "pause" || request.Command == "stop") && request.StreamID == ""
	}
	return request.StreamID != "" && request.StreamID == s.stream && request.CallbackToken != "" && request.CallbackToken == s.token
}

func isCatalogCommand(action string) bool {
	return action == "search" || action == "resolve" || action == "likes"
}

func isPlaybackMutation(action string) bool {
	switch action {
	case "info", "artist_info", "like", "unlike":
		return false
	}
	return true
}

func (p *Provider) sessionLocked(key string, create bool) *providerSession {
	now := p.now()
	for k, s := range p.sessions {
		if s.expired(now) {
			delete(p.sessions, k)
		}
	}
	s := p.sessions[key]
	if s == nil && create {
		if len(p.sessions) >= providerMaxSpeakers {
			var oldest string
			var at time.Time
			for k, e := range p.sessions {
				if at.IsZero() || e.at.Before(at) {
					oldest, at = k, e.at
				}
			}
			delete(p.sessions, oldest)
		}
		s = &providerSession{at: now, repeat: "off"}
		p.sessions[key] = s
	}
	return s
}

func (s *providerSession) expired(now time.Time) bool {
	until := s.at.Add(providerTTL)
	if (s.status == "pending" || s.status == "playing") && s.activeUntil.After(until) {
		until = s.activeUntil
	}
	return !now.Before(until)
}
func (p *Provider) renewPlaybackLocked(s *providerSession) {
	if s.index < 0 || s.index >= len(s.queue) {
		return
	}
	remaining := max(0, s.queue[s.index].DurationMS-max(0, s.offset))
	lease := time.Duration(min(remaining, providerMaxPlaybackLease.Milliseconds()))*time.Millisecond + providerPlaybackGrace
	s.activeUntil = p.now().Add(min(providerMaxPlaybackLease, max(providerTTL, lease)))
}

// revokeForeignLocked is idempotent: observing another source invalidates the
// old stream and its unresolved replacement work once, regardless of entry point.
func (p *Provider) revokeForeignLocked(ctx context.Context, s *providerSession) bool {
	if s == nil || s.stream == "" {
		return false
	}
	state := sound.CurrentPlayback(ctx)
	p.promoteObservedPrefetchLocked(s, state)
	if !state.Present || state.StreamID == "" || state.StreamID == s.stream || s.pendingPrevious(state.StreamID) {
		return false
	}
	if !s.revoked {
		s.invalidatePrefetch()
		s.generation++
		s.committed, s.status, s.terminal, s.revoked = s.generation, "stopped", true, true
	}
	return true
}
func (s *providerSession) pendingPrevious(stream string) bool {
	return (s.status == "pending" || s.generation != s.committed) && slices.Contains(s.previousStreams, stream)
}
func (p *Provider) ownsLocked(ctx context.Context, s *providerSession) bool {
	if s == nil || s.stream == "" || s.status == "stopped" || s.revoked || ctx.Err() != nil {
		return false
	}
	if p.revokeForeignLocked(ctx, s) {
		return false
	}
	state := sound.CurrentPlayback(ctx)
	wasPending := s.status == "pending"
	if state.Present {
		if s.pendingPrevious(state.StreamID) {
			return false
		}
		if state.StreamID == "" {
			if s.status == "paused" {
				return true
			}
			if s.status == "pending" {
				return false
			}
			// Cleared playback is not evidence of continued ownership. Keep
			// callback eligibility so a subsequent natural finish can advance.
			s.status = "idle"
			return false
		}
		if s.status != "pending" && (strings.EqualFold(state.PlayerState, "Stopped") || strings.EqualFold(state.PlayerState, "Idle") || strings.EqualFold(state.PlayerState, "Finished")) {
			s.status = "idle"
		}
		if strings.EqualFold(state.PlayerState, "Playing") {
			s.status, s.started, s.previousStreams = "playing", true, nil
		}
		if strings.EqualFold(state.PlayerState, "Paused") {
			s.status, s.started, s.previousStreams = "paused", true, nil
		}
		if state.OffsetKnown && state.OffsetMS >= 0 && !(s.status == "paused" && !strings.EqualFold(state.PlayerState, "Paused")) {
			s.offset, s.offsetKnown = state.OffsetMS, true
		}
		if s.status == "playing" && (wasPending || state.OffsetKnown) {
			p.renewPlaybackLocked(s)
		}
	}
	return s.status == "playing" || s.status == "paused"
}

// A native skip during loading still belongs to the issued queue, even
// before its started callback. Require an explicit own current/transition stream;
// absent state and unqualified text keep the stricter confirmed-playback rules.
func (p *Provider) ownsCommandLocked(ctx context.Context, s *providerSession, c providerCommand) bool {
	if p.ownsLocked(ctx, s) {
		return true
	}
	if !c.native || (c.action != "next" && c.action != "previous") || s == nil || s.revoked || ctx.Err() != nil || s.stream == "" || s.index < 0 || s.index >= len(s.queue) {
		return false
	}
	if s.status != "pending" && s.generation == s.committed {
		return false
	}
	state := sound.CurrentPlayback(ctx)
	return state.Present && state.StreamID != "" && (state.StreamID == s.stream || s.pendingPrevious(state.StreamID))
}

func (p *Provider) reserveLocked(key string, s *providerSession) playbackOperation {
	if s.status != "pending" && s.generation == s.committed {
		s.previousStreams = nil
	}
	if next := p.currentPrefetchLocked(s); next != nil {
		s.previousStreams = append(s.previousStreams, next.request.StreamID)
		if len(s.previousStreams) > providerMaxPendingStreams {
			s.previousStreams = slices.Clone(s.previousStreams[len(s.previousStreams)-providerMaxPendingStreams:])
		}
	}
	s.invalidatePrefetch()
	s.generation++
	s.at = p.now()
	failed := make(map[string]bool, len(s.failed))
	for k, v := range s.failed {
		failed[k] = v
	}
	return playbackOperation{speaker: key, session: s, generation: s.generation, queue: slices.Clone(s.queue), original: slices.Clone(s.original), index: s.index, offset: s.offset, id: s.id, repeat: s.repeat, shuffle: s.shuffle, failed: failed}
}
func (p *Provider) currentLocked(op playbackOperation) bool {
	return p.sessions[op.speaker] == op.session && op.session.generation == op.generation && !op.session.expired(p.now())
}
func (p *Provider) operationFailed(op playbackOperation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Restore callback eligibility for the committed stream after a rejected
	// replacement; a newer operation must never be altered by this failure.
	if p.currentLocked(op) {
		op.session.committed = op.generation
	}
}

func (p *Provider) Handle(ctx context.Context, text string) (string, error) {
	return p.handleCommand(ctx, parseProviderCommand(text))
}

// HandleControl uses the same queue and ordering rules as spoken commands.
func (p *Provider) HandleControl(ctx context.Context, control sound.Control) (string, error) {
	c := providerControlCommand(control)
	if c.action == "" {
		return "", nil
	}
	return p.handleCommand(ctx, c)
}

func (p *Provider) handleCommand(ctx context.Context, c providerCommand) (string, error) {
	if p == nil || !p.cfg.Enabled {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key := dialogue.SpeakerKey(ctx)
	if key == "" {
		return "Для SoundCloud нужна текущая колонка.", nil
	}
	if c.action == "" {
		return "", nil
	}
	catalog := isCatalogCommand(c.action)
	if catalog && !c.explicit && !p.IsDefault() {
		return "", nil
	}
	p.mu.Lock()
	s := p.sessionLocked(key, false)
	sequence := sound.RequestSequence(ctx)
	mutation := isPlaybackMutation(c.action)
	if sequence != 0 && s != nil && sequence < s.latestSequence {
		p.mu.Unlock()
		return "", context.Canceled
	}
	owned := p.ownsCommandLocked(ctx, s, c)
	if c.action == "select" {
		if s == nil || c.number > len(s.results) {
			p.mu.Unlock()
			return "Такого результата поиска нет.", nil
		}
	} else if !catalog && !owned && !c.explicit {
		p.mu.Unlock()
		return "", nil
	}
	// Arrival order survives ASR/routing delays independently of dialogue.
	// Metadata queries and legacy/callback contexts never raise or reset it.
	if mutation && sequence != 0 {
		if s == nil {
			s = p.sessionLocked(key, true)
		}
		s.latestSequence = sequence
	}
	if !catalog && c.action != "select" && (s == nil || len(s.queue) == 0) {
		// Explicit stop also cancels a still resolving first search.
		if c.action == "stop" && s != nil {
			s.generation++
			s.committed = s.generation
		}
		p.mu.Unlock()
		return "На этой колонке сейчас нет очереди SoundCloud.", nil
	}
	if !catalog && c.action != "select" && !owned {
		state := sound.CurrentPlayback(ctx)
		if state.Present && state.StreamID != "" && state.StreamID != s.stream {
			p.mu.Unlock()
			return "Сейчас на колонке играет другой источник.", nil
		}
	}
	if s == nil {
		s = p.sessionLocked(key, true)
	}
	s.at = p.now()
	switch c.action {
	case "info", "artist_info":
		track := s.queue[s.index]
		p.mu.Unlock()
		if c.action == "artist_info" {
			return "Исполнитель: " + track.Artist + ".", nil
		}
		return fmt.Sprintf("%s — %s. SoundCloud.", track.Artist, track.Title), nil
	case "like", "unlike":
		track := s.queue[s.index]
		p.mu.Unlock()
		if p.client == nil {
			return providerError(ctx, errors.New("unavailable"))
		}
		if err := p.client.Like(ctx, track, c.action == "like"); err != nil {
			return providerError(ctx, err)
		}
		if c.action == "unlike" {
			return "Трек удалён из избранного SoundCloud.", nil
		}
		return "Трек добавлен в избранное SoundCloud.", nil
	case "pause", "stop":
		op := p.reserveLocked(key, s)
		if err := sound.Play(ctx, sound.Request{Command: c.action, Provider: "SoundCloud", SessionID: s.id, Generation: op.generation}); err != nil {
			s.committed = op.generation
			p.mu.Unlock()
			return providerError(ctx, err)
		}
		s.committed = op.generation
		s.terminal = true
		if c.action == "pause" {
			s.status = "paused"
			p.mu.Unlock()
			return "Музыка SoundCloud на паузе.", nil
		}
		s.status = "stopped"
		p.mu.Unlock()
		return "Музыка SoundCloud остановлена.", nil
	case "repeat_one", "repeat_all", "repeat_off", "shuffle_on", "shuffle_off":
		refresh := p.currentPrefetchLocked(s) != nil
		state := sound.CurrentPlayback(ctx)
		if refresh && (!sound.HasPlayer(ctx) || !state.Present || state.StreamID != s.stream || !state.OffsetKnown || state.OffsetMS < 0 || !strings.EqualFold(state.PlayerState, "Playing") || s.media.URL == "") {
			p.mu.Unlock()
			return "Не удалось определить текущую позицию трека для смены режима. Повторите команду во время воспроизведения.", nil
		}
		previous := *s
		if refresh {
			previous.queue = slices.Clone(s.queue)
		}
		if !refresh {
			s.invalidatePrefetch()
		}
		// An already staged native continuation must be replaced together with
		// the current stream. Reuse its relay lease to avoid an HTTP gap between
		// invalidating the staged item and enqueueing the replacement. Otherwise
		// modes remain synchronous edits without canceling an in-flight search.
		switch c.action {
		case "repeat_one":
			s.repeat = "one"
		case "repeat_all":
			s.repeat = "all"
		case "repeat_off":
			s.repeat = "off"
		case "shuffle_on":
			if !s.shuffle {
				s.shuffle = true
				mathrand.Shuffle(len(s.queue)-s.index-1, func(i, j int) { i += s.index + 1; j += s.index + 1; s.queue[i], s.queue[j] = s.queue[j], s.queue[i] })
			}
		case "shuffle_off":
			if s.shuffle {
				urn := s.queue[s.index].URN
				s.queue = slices.Clone(s.original)
				s.index = slices.IndexFunc(s.queue, func(t Track) bool { return t.URN == urn })
				s.shuffle = false
			}
		}
		if refresh {
			op := p.reserveLocked(key, s)
			op.offset = state.OffsetMS
			if err := p.enqueuePreparedLocked(ctx, op, op.queue[op.index], s.media); err != nil {
				*s = previous
				p.mu.Unlock()
				return providerError(ctx, err)
			}
		}
		p.mu.Unlock()
		return "Режим воспроизведения SoundCloud изменён.", nil
	}
	if !sound.HasPlayer(ctx) && !(c.action == "search" && c.find) {
		p.mu.Unlock()
		return "Воспроизведение недоступно для этого запроса.", nil
	}
	op := p.reserveLocked(key, s)
	if c.action == "select" {
		op.queue = slices.Clone(s.results)
		op.index = c.number - 1
		op.id = rand.Text()
		op.original = slices.Clone(op.queue)
		op.offset = 0
		op.failed = make(map[string]bool)
	}
	if c.action == "resume" && !s.offsetKnown {
		p.mu.Unlock()
		p.operationFailed(op)
		return "Не удалось определить позицию трека. Скажите «начни сначала».", nil
	}
	p.mu.Unlock()
	var err error
	switch c.action {
	case "search", "resolve", "likes", "related":
		if p.client == nil {
			err = errors.New("unavailable")
			break
		}
		var tracks []Track
		switch c.action {
		case "resolve":
			tracks, err = p.client.Resolve(ctx, c.query, p.cfg.QueueLimit)
		case "likes":
			tracks, err = p.client.Likes(ctx, p.cfg.QueueLimit)
		case "related":
			tracks, err = p.client.Related(ctx, op.queue[op.index], p.cfg.QueueLimit)
		default:
			query := c.query
			if query == "" {
				query = "music"
			}
			tracks, err = p.client.Search(ctx, query, c.kind, p.cfg.QueueLimit)
		}
		if err != nil {
			break
		}
		if len(tracks) == 0 {
			p.operationFailed(op)
			return "В SoundCloud ничего не найдено.", nil
		}
		tracks = tracks[:min(len(tracks), p.cfg.QueueLimit)]
		if c.find {
			p.mu.Lock()
			if !p.currentLocked(op) || ctx.Err() != nil {
				p.mu.Unlock()
				return "", ctx.Err()
			}
			s.results = slices.Clone(tracks)
			s.committed = op.generation
			p.mu.Unlock()
			var out strings.Builder
			out.WriteString("Найдено в SoundCloud:")
			for i, t := range tracks[:min(10, len(tracks))] {
				fmt.Fprintf(&out, "\n%d. %s — %s", i+1, t.Artist, t.Title)
			}
			out.WriteString("\nСкажите «включи номер» и номер результата.")
			return out.String(), nil
		}
		op.queue, op.original, op.index, op.offset, op.id, op.failed = tracks, slices.Clone(tracks), 0, 0, rand.Text(), make(map[string]bool)
	case "next", "previous":
		step := 1
		if c.action == "previous" {
			step = -1
		}
		next := providerNext(op, step, false)
		if next < 0 {
			p.operationFailed(op)
			return "Это крайний трек очереди SoundCloud.", nil
		}
		op.index, op.offset = next, 0
	case "restart":
		op.offset = 0
	case "seek":
		if c.relative {
			op.offset += c.offset
		} else {
			op.offset = c.offset
		}
		op.offset = max(0, min(op.offset, max(0, op.queue[op.index].DurationMS-1)))
	}
	if err == nil {
		// Catalog availability can change after search. Skip unavailable full
		// streams within the bounded queue rather than trying a preview.
		for range len(op.queue) {
			err = p.play(ctx, op)
			if !errors.Is(err, ErrUnavailable) {
				break
			}
			op.failed[op.queue[op.index].URN] = true
			next := providerNext(op, 1, false)
			if next < 0 {
				break
			}
			op.index, op.offset = next, 0
		}
	}
	if err != nil {
		p.operationFailed(op)
		return providerError(ctx, err)
	}
	return "", nil
}

func providerError(ctx context.Context, err error) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", err
	}
	if errors.Is(err, ErrAuthorization) {
		return "Для избранного SoundCloud нужна авторизация пользователя. Настройте личный токен доступа.", nil
	}
	return "Не удалось воспроизвести или выполнить запрос SoundCloud. Попробуйте другой трек или повторите запрос.", nil
}

func (p *Provider) play(ctx context.Context, op playbackOperation) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if p.client == nil || p.relay == nil {
		return errors.New("soundcloud: воспроизведение недоступно")
	}
	if op.index < 0 || op.index >= len(op.queue) {
		return errors.New("soundcloud: очередь закончилась")
	}
	track := op.queue[op.index]
	if track.DurationMS <= 0 {
		return ErrUnavailable
	}
	upstream, err := p.client.Stream(ctx, track)
	if err != nil {
		return err
	}
	media, err := p.registerStream(track, upstream)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enqueuePreparedLocked(ctx, op, track, media)
}

func (p *Provider) registerStream(track Track, upstream string) (Media, error) {
	var resolveMu sync.Mutex
	return p.relay.Register(track.ArtworkURL, func(ctx context.Context) (string, error) {
		resolveMu.Lock()
		first := upstream
		upstream = ""
		resolveMu.Unlock()
		if first != "" {
			return first, nil
		}
		return p.client.Stream(ctx, track)
	})
}

// Caller holds p.mu; media is already resolved and registered, so enqueue does no HTTP.
func (p *Provider) enqueuePreparedLocked(ctx context.Context, op playbackOperation, track Track, media Media) error {
	if !p.currentLocked(op) {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s := op.session
	// Repeat/shuffle can change while catalog HTTP is in flight. Apply the
	// latest modes and queue ordering at the same point as the generation check.
	if op.id == s.id {
		op.queue, op.original = slices.Clone(s.queue), slices.Clone(s.original)
		if op.index >= len(op.queue) || op.queue[op.index].URN != track.URN {
			op.index = slices.IndexFunc(op.queue, func(t Track) bool { return t.URN == track.URN })
		}
	} else if s.shuffle {
		mathrand.Shuffle(len(op.queue)-op.index-1, func(i, j int) {
			i += op.index + 1
			j += op.index + 1
			op.queue[i], op.queue[j] = op.queue[j], op.queue[i]
		})
	}
	op.repeat, op.shuffle = s.repeat, s.shuffle
	request := preparedSoundRequest(op, track, media)
	// Serialize the final generation check, native enqueue, and commit. Play is
	// the current request's local enqueue hook; all catalog HTTP stays outside.
	if err := sound.Play(ctx, request); err != nil {
		return err
	}
	// The speaker can still report an earlier own stream until it applies
	// this directive. Retain bounded transition identities until confirmation.
	if s.status != "pending" && s.generation == s.committed {
		s.previousStreams = nil
	}
	if s.stream != "" {
		s.previousStreams = append(s.previousStreams, s.stream)
	}
	if len(s.previousStreams) > providerMaxPendingStreams {
		s.previousStreams = append(s.previousStreams[:1:1], s.previousStreams[len(s.previousStreams)-providerMaxPendingStreams+1:]...)
	}
	s.queue, s.original, s.index = op.queue, op.original, op.index
	s.media = media
	s.id, s.stream, s.token, s.status = op.id, request.StreamID, request.CallbackToken, "pending"
	s.offset, s.offsetKnown = op.offset, true
	s.repeat, s.shuffle, s.failed = op.repeat, op.shuffle, op.failed
	s.committed, s.terminal, s.started, s.revoked, s.at = op.generation, false, false, false, p.now()
	p.renewPlaybackLocked(s)
	return nil
}

func providerNext(op playbackOperation, step int, terminal bool) int {
	if terminal && op.repeat == "one" && !op.failed[op.queue[op.index].URN] {
		return op.index
	}
	i := op.index
	for range len(op.queue) {
		i += step
		if i < 0 || i >= len(op.queue) {
			if op.repeat != "all" {
				return -1
			}
			i = (i + len(op.queue)) % len(op.queue)
		}
		if !op.failed[op.queue[i].URN] {
			return i
		}
	}
	return -1
}

// Station 2 reports natural EOF through its stopped callback. Only the
// current stream's explicit Stopped state and a finite near-end position are
// enough to distinguish EOF from pause, replacement, and early interruption.
func stoppedNearEnd(s *providerSession, state sound.PlaybackState) bool {
	if !state.Present || state.StreamID != s.stream || !strings.EqualFold(state.PlayerState, "Stopped") || !state.OffsetKnown || state.OffsetMS < 0 || s.index < 0 || s.index >= len(s.queue) {
		return false
	}
	duration := s.queue[s.index].DurationMS
	if duration <= 0 {
		return false
	}
	tolerance := min(int64(1000), duration/10)
	difference := state.OffsetMS - duration
	return difference >= -tolerance && difference <= tolerance
}

func (p *Provider) HandleEvent(ctx context.Context, name string, payload map[string]any) (bool, error) {
	if !strings.HasPrefix(name, EventPrefix) {
		return false, nil
	}
	actionLog, outcome := "unknown", "disabled_or_canceled"
	sessionPresent, sessionMatch, streamMatch, tokenMatch, generationCurrent, terminal, observationMatches, naturalEnd := false, false, false, false, false, false, false, false
	defer func() {
		slog.Info("soundcloud_event", "action", actionLog, "outcome", outcome, "session_present", sessionPresent, "session_match", sessionMatch, "stream_match", streamMatch, "token_match", tokenMatch, "generation_current", generationCurrent, "terminal", terminal, "observation_matches_stream", observationMatches, "natural_end", naturalEnd)
	}()
	if p == nil || !p.cfg.Enabled || ctx.Err() != nil {
		return true, nil
	}
	action := strings.TrimPrefix(name, EventPrefix)
	outcome = "unknown_action"
	if action != "started" && action != "stopped" && action != "finished" && action != "failed" {
		return true, nil
	}
	actionLog, outcome = action, "invalid_payload_or_scope"
	sid, _ := payload["session_id"].(string)
	stream, _ := payload["stream_id"].(string)
	token, _ := payload["token"].(string)
	key := dialogue.SpeakerKey(ctx)
	if key == "" || sid == "" || stream == "" || token == "" {
		return true, nil
	}
	p.mu.Lock()
	s := p.sessionLocked(key, false)
	// A retired track callback can already carry the following track in the
	// device state. Apply that matching prepared-stream observation before
	// matching the callback, so its old action can never run against the new item.
	p.promoteObservedPrefetchLocked(s, sound.CurrentPlayback(ctx))
	if p.handlePreparedLifecycleLocked(ctx, s, sid, stream, token, action) {
		p.mu.Unlock()
		return true, nil
	}
	sessionPresent = s != nil
	if s != nil {
		sessionMatch, streamMatch, tokenMatch = s.id == sid, s.stream == stream, s.token == token
		generationCurrent, terminal = s.committed == s.generation, s.terminal
		observationMatches = sound.CurrentPlayback(ctx).StreamID != "" && sound.CurrentPlayback(ctx).StreamID == s.stream
	}
	switch {
	case !sessionPresent:
		outcome = "session_missing"
	case !sessionMatch:
		outcome = "session_mismatch"
	case !streamMatch:
		outcome = "stream_mismatch"
	case !tokenMatch:
		outcome = "token_mismatch"
	case !generationCurrent:
		outcome = "generation_pending"
	case terminal:
		outcome = "terminal"
	default:
		outcome = "accepted"
	}
	if outcome != "accepted" {
		p.mu.Unlock()
		return true, nil
	}
	if p.revokeForeignLocked(ctx, s) {
		outcome = "foreign_observation"
		p.mu.Unlock()
		return true, nil
	}
	state := sound.CurrentPlayback(ctx)
	if action == "started" {
		outcome = "started_duplicate"
		// Start confirms this new stream once. Delayed duplicates must not
		// revive ownership after a later idle/cleared playback report.
		if !s.started && (s.status == "pending" || s.status == "playing") {
			outcome = "started"
			s.started, s.status, s.at = true, "playing", p.now()
			s.previousStreams = nil
			p.renewPlaybackLocked(s)
		}
		p.mu.Unlock()
		return true, nil
	}
	s.at = p.now()
	if action == "stopped" && stoppedNearEnd(s, state) {
		action = "finished"
		naturalEnd = true
	}
	if action == "stopped" {
		outcome = "stopped_paused"
		s.status = "paused"
		s.terminal = true
		if state.StreamID == s.stream && state.OffsetKnown && state.OffsetMS >= 0 {
			s.offset, s.offsetKnown = state.OffsetMS, true
		}
		p.mu.Unlock()
		return true, nil
	}
	if action == "finished" && p.awaitDeliveredPrefetchLocked(s) {
		outcome = "prefetched_next_staged"
		p.mu.Unlock()
		return true, nil
	}
	// Claim terminal delivery atomically before any HTTP request or queue change.
	s.terminal = true
	op := p.reserveLocked(key, s)
	if action == "failed" {
		outcome = "queue_unavailable"
		op.failed[op.queue[op.index].URN] = true
	}
	next := providerNext(op, 1, true)
	if next < 0 {
		outcome = "queue_end"
		s.status = "stopped"
		s.committed = op.generation
		p.mu.Unlock()
		return true, nil
	}
	op.index, op.offset = next, 0
	p.mu.Unlock()
	// Resolution failures skip at most the bounded queue, including repeat mode.
	for range len(op.queue) {
		err := p.play(ctx, op)
		if err == nil {
			outcome = "next_queued"
			return true, nil
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			outcome = "canceled"
			return true, nil
		}
		if !errors.Is(err, ErrUnavailable) {
			outcome = "next_error"
			p.operationFailed(op)
			return true, errors.New("SoundCloud: не удалось включить следующий трек")
		}
		op.failed[op.queue[op.index].URN] = true
		next = providerNext(op, 1, false)
		if next < 0 {
			break
		}
		op.index = next
	}
	p.mu.Lock()
	if p.currentLocked(op) {
		s.status = "stopped"
		s.committed = op.generation
	}
	p.mu.Unlock()
	return true, nil
}

func preparedSoundRequest(op playbackOperation, track Track, media Media) sound.Request {
	request := sound.Request{URL: media.URL, StreamID: sound.NewStreamID(), Title: track.Title, OffsetMS: op.offset, Format: "HLS", Kind: "Track", Provider: "SoundCloud", Subtitle: track.Artist, ArtworkURL: media.ArtworkURL, TrackID: track.URN, PermalinkURL: track.PermalinkURL, SessionID: op.id, Generation: op.generation, CallbackToken: rand.Text(), DurationMS: track.DurationMS, RepeatMode: op.repeat, Shuffle: op.shuffle}
	if prev := providerNext(op, -1, false); prev >= 0 {
		request.PreviousTrackID = op.queue[prev].URN
	}
	if next := providerNext(op, 1, false); next >= 0 {
		request.NextTrackID = op.queue[next].URN
	}
	return request
}
