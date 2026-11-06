package soundcloud

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

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

var implicitComparisonPlay = regexp.MustCompile(`(?i)^(?:алиса[,\s]+)?(?:включи|поставь|запусти)\s+(.+)$`)

// ImplicitComparisonEnabled admits only a separate, exact-metadata comparison.
// It never changes the strict/default-provider command parser or claims playback.
func (p *Provider) ImplicitComparisonEnabled(ctx context.Context, text string) bool {
	if p == nil || !p.cfg.Enabled || !p.cfg.CompareYandex || ctx.Err() != nil || dialogue.SpeakerKey(ctx) == "" {
		return false
	}
	return parseImplicitComparisonCommand(text).action != ""
}

func parseImplicitComparisonCommand(text string) providerCommand {
	if parseProviderCommand(text).action != "" || providerName.MatchString(text) {
		return providerCommand{}
	}
	match := implicitComparisonPlay.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return providerCommand{}
	}
	for word := range strings.FieldsSeq(text) {
		word = strings.Trim(word, "()[]{}<>,.!?\"'")
		if strings.Contains(word, "://") || strings.HasPrefix(strings.ToLower(word), "www.") || providerWebsite.MatchString(strings.ToLower(word)) {
			return providerCommand{}
		}
	}
	query := musicmatch.Query(text)
	if query == "" {
		return providerCommand{}
	}
	for _, prefix := range []string{"что нибудь", "что то", "что угодно", "чтонибудь", "какую нибудь", "какой нибудь", "любую", "любой", "something", "anything", "some", "random"} {
		if query == prefix || strings.HasPrefix(query, prefix+" ") {
			return providerCommand{}
		}
	}
	for word := range strings.FieldsSeq(query) {
		// Numeric device names and time units remain controls; digits inside
		// an otherwise literal title are valid catalog metadata.
		guardWord := strings.TrimFunc(word, unicode.IsDigit)
		switch guardWord {
		case "музыку", "музыка", "музыки", "музыке", "music", "радио", "radio", "волну", "волна", "волне", "трек", "треки", "песни", "песню", "альбом", "плейлист", "исполнителя",
			"свет", "света", "бра", "люстру", "подсветку", "чайник", "кофеварку", "кондиционер", "пылесос", "телевизор", "телек", "шторы", "жалюзи", "вентилятор", "отопление", "обогреватель", "увлажнитель", "сцену", "сценарий", "режим", "яркость", "громкость", "громче", "тише", "потише", "погромче", "уборку", "обновление",
			"будильник", "таймер", "напоминание", "напоминалку", "сигнализацию", "камеру", "видео", "кино", "фильм", "мультик", "сериал", "канал", "минут", "минуту", "минуты", "секунд", "секунду", "секунды", "час", "часа", "часов",
			"bluetooth", "блютуз", "блютус", "airplay", "эйрплей", "яндекс", "яндексе", "яндекса", "yandex", "spotify", "youtube", "ютуб", "сначала", "потом", "включи", "поставь", "запусти":
			return providerCommand{}
		}
		for _, prefix := range []string{"радиостанц", "светиль", "ламп", "розет", "устройств", "спокойн", "расслаб", "бодрящ", "грустн", "весел", "танцевальн", "романтич", "фонов", "детск", "классическ", "новогодн", "relax", "calm", "happy", "sad", "chill", "sleep", "mood", "soothing"} {
			if strings.HasPrefix(guardWord, prefix) {
				return providerCommand{}
			}
		}
	}
	return providerCommand{action: "search", kind: TrackKind, query: query}
}

// The hint is scoped to a currently observed own stream. Reading it must not
// revive an ended session, reserve an operation, or revoke another source.
func (p *Provider) implicitArtistContext(ctx context.Context) (string, []Track) {
	state := sound.CurrentPlayback(ctx)
	if !state.Present || state.StreamID == "" {
		return "", nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[dialogue.SpeakerKey(ctx)]
	if s == nil || s.revoked || s.expired(p.now()) || s.stream != state.StreamID || s.index < 0 || s.index >= len(s.queue) {
		return "", nil
	}
	return strings.TrimSpace(s.queue[s.index].Artist), slices.Clone(s.queue[:min(len(s.queue), p.cfg.QueueLimit)])
}

type comparisonSearchResult struct {
	tracks []Track
	err    error
}

func (p *Provider) comparisonSearch(ctx context.Context, query, artistHint string, cached bool) ([]Track, bool, error) {
	if artistHint == "" {
		tracks, err := p.client.Search(ctx, query, TrackKind, p.cfg.QueueLimit)
		return tracks, true, err
	}
	// The artist lookup is optional: leave time for a usable global result
	// to resolve its stream before the enclosing comparison deadline.
	budget := 1500 * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)/2)
	}
	scopedCtx, cancelScoped := context.WithTimeout(ctx, budget)
	globalCtx, cancelGlobal := ctx, func() {}
	if cached {
		globalCtx, cancelGlobal = context.WithTimeout(ctx, budget)
	}
	var global, scoped comparisonSearchResult
	var wg sync.WaitGroup
	wg.Go(func() {
		defer cancelGlobal()
		global.tracks, global.err = p.client.Search(globalCtx, query, TrackKind, p.cfg.QueueLimit)
	})
	wg.Go(func() {
		defer cancelScoped()
		scoped.tracks, scoped.err = p.client.Search(scopedCtx, query+" "+artistHint, TrackKind, min(p.cfg.QueueLimit, 10))
	})
	wg.Wait()
	slog.Info("soundcloud_artist_comparison_search", "global_tracks", len(global.tracks), "artist_tracks", len(scoped.tracks), "global_ok", global.err == nil, "artist_ok", scoped.err == nil, "artist_timeout", errors.Is(scoped.err, context.DeadlineExceeded))
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if global.err != nil && scoped.err != nil {
		return nil, false, global.err
	}
	var tracks []Track
	if global.err == nil {
		tracks = append(tracks, global.tracks[:min(len(global.tracks), p.cfg.QueueLimit)]...)
	}
	if scoped.err == nil {
		tracks = append(tracks, scoped.tracks[:min(len(scoped.tracks), p.cfg.QueueLimit)]...)
	}
	return tracks, scoped.err == nil, nil
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
	implicit := !p.ComparisonEnabled(ctx, text)
	if implicit && !p.ImplicitComparisonEnabled(ctx, text) {
		return nil, errors.New("soundcloud: comparison is unavailable")
	}
	if p.client == nil || p.relay == nil {
		return nil, ErrUnavailable
	}
	c := parseProviderCommand(text)
	artistHint := ""
	var cached []Track
	if implicit {
		c = parseImplicitComparisonCommand(text)
		artistHint, cached = p.implicitArtistContext(ctx)
		cached = slices.DeleteFunc(cached, func(t Track) bool {
			return !musicmatch.SameArtist(t.Artist, artistHint) || !musicmatch.ExactMatch(text, candidate(t))
		})
		slog.Info("soundcloud_implicit_comparison", "artist_hint_present", artistHint != "", "playback_present", sound.CurrentPlayback(ctx).Present)
	}
	tracks, artistComplete, err := p.comparisonSearch(ctx, c.query, artistHint, len(cached) > 0)
	if err != nil && (len(cached) == 0 || ctx.Err() != nil) {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if implicit {
		tracks = append(tracks, cached...)
		tracks = slices.DeleteFunc(tracks, func(t Track) bool {
			c := candidate(t)
			return !musicmatch.ExactMatch(text, c) || artistHint != "" && !artistComplete && !musicmatch.SameArtist(t.Artist, artistHint) && !musicmatch.ExplicitArtistMatch(text, c)
		})
		slog.Info("soundcloud_implicit_comparison_candidates", "cached_candidates", len(cached), "artist_lookup_complete", artistComplete, "eligible_tracks", len(tracks))
	}
	slices.SortStableFunc(tracks, func(a, b Track) int {
		if order := musicmatch.Score(text, candidate(b)) - musicmatch.Score(text, candidate(a)); order != 0 {
			return order
		}
		if artistHint != "" {
			aHint, bHint := musicmatch.SameArtist(a.Artist, artistHint), musicmatch.SameArtist(b.Artist, artistHint)
			if aHint != bHint {
				if aHint {
					return -1
				}
				return 1
			}
		}
		return 0
	})
	if implicit {
		seen := make(map[string]bool, len(tracks))
		unique := tracks[:0]
		for _, t := range tracks {
			if !seen[t.URN] {
				seen[t.URN] = true
				unique = append(unique, t)
				if len(unique) == p.cfg.QueueLimit {
					break
				}
			}
		}
		tracks = unique
	}
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
