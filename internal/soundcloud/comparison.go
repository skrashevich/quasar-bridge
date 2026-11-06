package soundcloud

import (
	"context"
	"crypto/rand"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/musicmatch"
	"quasarbridge/internal/sound"
)

var comparisonSongKind = regexp.MustCompile(`(?i)^(?:алиса[,\s]+)?(?:включи|поставь|сыграй|играй|запусти|play)\s+(?:песню|трек|композицию|song|track)\s+`)

func (p *Provider) ComparisonEnabled(ctx context.Context, text string) bool {
	if p == nil || !p.cfg.Enabled || !p.cfg.CompareYandex || ctx.Err() != nil || dialogue.SpeakerKey(ctx) == "" {
		return false
	}
	c := parseProviderCommand(text)
	if c.action != "search" || c.kind != TrackKind || c.explicit || c.find || c.query == "" {
		return false
	}
	// Broad music and mood commands retain their provider's normal scenario;
	// comparison only arbitrates a requested song or track.
	if !comparisonSongKind.MatchString(strings.TrimSpace(text)) {
		query := musicmatch.Query(text)
		for _, prefix := range []string{"что нибудь", "что то", "что угодно", "чтонибудь", "какую нибудь", "какой нибудь", "любую", "любой", "мою волну", "моя волна", "something", "anything", "some", "random"} {
			if query == prefix || strings.HasPrefix(query, prefix+" ") {
				return false
			}
		}
		for word := range strings.FieldsSeq(query) {
			switch word {
			case "музыку", "музыка", "music", "radio", "радио":
				return false
			}
		}
	}
	return true
}

func candidate(track Track) musicmatch.Candidate {
	return musicmatch.Candidate{Title: track.Title, Artist: track.Artist, DurationMS: track.DurationMS}
}

type preparedComparison struct {
	mu       sync.Mutex
	provider *Provider
	speaker  string
	sequence uint64
	tracks   []Track
	upstream string
	metadata musicmatch.Candidate
	closed   bool
}

func (p *Provider) Prepare(ctx context.Context, text string) (musicmatch.Prepared, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !p.ComparisonEnabled(ctx, text) {
		return nil, errors.New("soundcloud: comparison is unavailable")
	}
	if p.client == nil || p.relay == nil {
		return nil, ErrUnavailable
	}
	c := parseProviderCommand(text)
	tracks, err := p.client.Search(ctx, c.query, TrackKind, p.cfg.QueueLimit)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(tracks, func(a, b Track) int {
		return musicmatch.Score(text, candidate(b)) - musicmatch.Score(text, candidate(a))
	})
	for index, track := range tracks {
		if track.DurationMS <= 0 {
			continue
		}
		upstream, err := p.client.Stream(ctx, track)
		if errors.Is(err, ErrUnavailable) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		queue := slices.Clone(tracks[index:])
		return &preparedComparison{provider: p, speaker: dialogue.SpeakerKey(ctx), sequence: sound.RequestSequence(ctx), tracks: queue, upstream: upstream, metadata: candidate(track)}, nil
	}
	return nil, ErrUnavailable
}

func (p *preparedComparison) Candidate() musicmatch.Candidate { return p.metadata }

func (p *preparedComparison) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.upstream = ""
	p.tracks = nil
}

func (prepared *preparedComparison) Commit(ctx context.Context) error {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.closed || ctx.Err() != nil || dialogue.SpeakerKey(ctx) != prepared.speaker || sound.RequestSequence(ctx) != prepared.sequence {
		return context.Canceled
	}
	if !sound.HasPlayer(ctx) {
		return errors.New("soundcloud: comparison playback is unavailable")
	}
	p := prepared.provider
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessionLocked(prepared.speaker, false)
	if s != nil && prepared.sequence != 0 && prepared.sequence < s.latestSequence {
		return context.Canceled
	}
	created := s == nil
	if created {
		s = p.sessionLocked(prepared.speaker, true)
	}
	old := *s
	op := p.reserveLocked(prepared.speaker, s)
	op.queue = slices.Clone(prepared.tracks)
	op.original = slices.Clone(op.queue)
	op.index, op.offset, op.id, op.failed = 0, 0, rand.Text(), make(map[string]bool)
	media, err := p.registerStream(op.queue[0], prepared.upstream)
	if err == nil {
		err = p.enqueuePreparedLocked(ctx, op, op.queue[0], media)
	}
	if err != nil {
		*s = old
		if created {
			delete(p.sessions, prepared.speaker)
		}
		return err
	}
	if prepared.sequence != 0 {
		s.latestSequence = prepared.sequence
	}
	prepared.closed = true
	prepared.upstream = ""
	prepared.tracks = nil
	return nil
}

// AcceptYandex revokes this speaker's former SC ownership only after the native
// arbiter has selected the complete original Yandex response for delivery.
func (p *Provider) AcceptYandex(ctx context.Context) error {
	if p == nil || ctx.Err() != nil {
		return context.Canceled
	}
	key := dialogue.SpeakerKey(ctx)
	if key == "" {
		return context.Canceled
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s := p.sessionLocked(key, false)
	sequence := sound.RequestSequence(ctx)
	if s != nil && sequence != 0 && sequence < s.latestSequence {
		return context.Canceled
	}
	if s == nil {
		s = p.sessionLocked(key, true)
	}
	s.generation++
	s.committed, s.status, s.terminal, s.revoked, s.at = s.generation, "stopped", true, true, p.now()
	if sequence != 0 {
		s.latestSequence = sequence
	}
	return nil
}
