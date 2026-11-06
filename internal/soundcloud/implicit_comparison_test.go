package soundcloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quasarbridge/internal/sound"
)

func TestImplicitComparisonAdmissionDoesNotChangeDefaultRouting(t *testing.T) {
	var requests atomic.Int64
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNotFound) })
	p.cfg.CompareYandex = true
	ctx := providerContext(t, "a", new(providerRecorder))
	for _, text := range []string{"включи Нас нет", "поставь First", "запусти Performer Melody", "Алиса, включи Белая ночь", "включи 7 Nation Army", "поставь Ноль42"} {
		if !p.ImplicitComparisonEnabled(ctx, text) {
			t.Errorf("bare play comparison rejected: %q", text)
		}
		if p.ComparisonEnabled(ctx, text) || p.CanHandle(ctx, text) {
			t.Errorf("implicit comparison changed strict/default routing: %q", text)
		}
	}
	for _, text := range []string{"Нас нет", "а теперь Нас нет", "не включай First", "выключи First", "найди First", "пауза", "следующий трек", "включи песню First", "включи First SoundCloud", "включи First на Яндекс Музыке", "включи First на spotify", "включи AirPlay", "включи https://example.com/song", "включи example.com/song", "включи (example.com/song)", "включи https://soundcloud.com/artist/song", "включи музыку", "включи музыку для сна", "поставь спокойное", "включи радио", "включи мою волну", "включи что-нибудь", "включи что-то веселое", "включи свет", "включи чайник", "включи кондиционер", "включи бра", "включи неизвестное устройство", "запусти уборку", "включи телевизор", "включи таймер", "поставь будильник на семь", "поставь яркость на 50", "включи 42", "поставь 10 минут", "поставь 5минут", "включи свет42", "включи First и поставь таймер"} {
		if p.ImplicitComparisonEnabled(ctx, text) {
			t.Errorf("unsafe implicit comparison admitted: %q", text)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("admission made catalog HTTP requests")
	}
	p.SetDefault(false)
	if !p.ImplicitComparisonEnabled(ctx, "включи First") || p.CanHandle(ctx, "включи First") {
		t.Fatal("implicit eligibility depends on provider default")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if p.ImplicitComparisonEnabled(canceled, "включи First") || p.ImplicitComparisonEnabled(t.Context(), "включи First") {
		t.Fatal("missing live speaker admitted")
	}
	p.cfg.CompareYandex = false
	if p.ImplicitComparisonEnabled(ctx, "включи First") {
		t.Fatal("disabled comparison admitted")
	}
}

func TestImplicitComparisonPreparesOnlyExactMetadata(t *testing.T) {
	var streams atomic.Int64
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			streams.Add(1)
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		if got := r.URL.Query().Get("q"); got != "нас нет" {
			t.Errorf("implicit search query=%q", got)
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"Нас нет (Remix)","duration":120000,"metadata_artist":"Artist"},{"id":2,"title":"03 Artist - Нас нет!","duration":120000,"metadata_artist":"Artist"},{"id":3,"title":"Нас","duration":120000,"metadata_artist":"Other"}]}`)
	})
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := sound.WithRequestSequence(providerContext(t, "a", r), 10)
	prepared, err := p.Prepare(ctx, "Включи Нас нет")
	if err != nil || prepared == nil {
		t.Fatal(prepared, err)
	}
	defer prepared.Close()
	if prepared.Candidate().Title != "03 Artist - Нас нет!" || streams.Load() != 1 || len(r.all()) != 0 {
		t.Fatal("implicit preparation selected partial/versioned metadata or mutated player", prepared.Candidate(), streams.Load())
	}
	if err = prepared.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	first := r.all()[0]
	if first.TrackID != "soundcloud:tracks:2" {
		t.Fatal(first)
	}
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first))
	if len(r.all()) != 1 {
		t.Fatal("nonexact implicit candidates entered automatic queue")
	}
}

func TestImplicitComparisonNoExactResultPreservesOriginalRoute(t *testing.T) {
	var streams atomic.Int64
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			streams.Add(1)
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First remix","duration":120000,"metadata_artist":"Artist"},{"id":2,"title":"First Song","duration":120000,"metadata_artist":"Artist"},{"id":3,"title":"Other","duration":120000,"metadata_artist":"First"}]}`)
	})
	p.cfg.CompareYandex = true
	ctx := providerContext(t, "a", new(providerRecorder))
	prepared, err := p.Prepare(ctx, "включи First")
	if prepared != nil || !errors.Is(err, ErrUnavailable) || streams.Load() != 0 {
		t.Fatal("nonexact candidates replaced fallback", prepared, err, streams.Load())
	}
	strict, err := p.Prepare(ctx, "включи песню First")
	if err != nil || strict == nil {
		t.Fatal("strict musical comparison was narrowed", strict, err)
	}
	strict.Close()
}

func TestImplicitComparisonCommitPreservesCancellationAndOrdering(t *testing.T) {
	p := providerFixture(t, nil)
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := sound.WithRequestSequence(providerContext(t, "a", r), 10)
	first := providerStart(t, p, ctx, r)
	prepared, err := p.Prepare(ctx, "включи First")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = prepared.Commit(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(r.all()) != 1 || !p.CanDeliver(ctx, first) {
		t.Fatal("canceled implicit commit changed playback")
	}
	p.AcceptYandex(sound.WithRequestSequence(ctx, 11))
	if err = prepared.Commit(ctx); !errors.Is(err, context.Canceled) || len(r.all()) != 1 {
		t.Fatal("stale implicit commit replaced newer selection", err)
	}
}

func implicitArtistFixture(t *testing.T, global, scoped string, queries *atomic.Int64, failScoped bool) *Provider {
	t.Helper()
	return providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		q := strings.ToLower(r.URL.Query().Get("q"))
		if q == "first" {
			fmt.Fprint(w, `{"collection":[{"id":1,"title":"Current","duration":120000,"metadata_artist":"Hint"}]}`)
			return
		}
		if strings.HasSuffix(q, " hint") {
			queries.Add(1)
			if failScoped {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, scoped)
			return
		}
		fmt.Fprint(w, global)
	})
}

func TestImplicitComparisonArtistHintSearchAndTiePreference(t *testing.T) {
	scopedQueries := new(atomic.Int64)
	p := implicitArtistFixture(t, `{"collection":[{"id":2,"title":"Wanted","duration":120000,"metadata_artist":"Other"}]}`, `{"collection":[{"id":3,"title":"Wanted","duration":120000,"metadata_artist":"HINT"}]}`, scopedQueries, false)
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	current := providerStart(t, p, ctx, r)
	observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: current.StreamID, PlayerState: "Stopped"})
	prepared, err := p.Prepare(observed, "включи Wanted")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if prepared.Candidate().Artist != "HINT" || scopedQueries.Load() != 1 {
		t.Fatal("matching current artist did not contribute missing exact candidate", prepared.Candidate(), scopedQueries.Load())
	}
	if len(r.all()) != 1 || !p.CanDeliver(ctx, current) {
		t.Fatal("artist hint preparation changed current queue")
	}
}

func TestImplicitComparisonArtistHintNeverOverridesHigherScore(t *testing.T) {
	scopedQueries := new(atomic.Int64)
	p := implicitArtistFixture(t, `{"collection":[{"id":2,"title":"Melody","duration":120000,"metadata_artist":"Performer"}]}`, `{"collection":[{"id":3,"title":"Performer Melody","duration":120000,"metadata_artist":"Hint"}]}`, scopedQueries, false)
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	current := providerStart(t, p, ctx, r)
	observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: current.StreamID, PlayerState: "Playing"})
	prepared, err := p.Prepare(observed, "поставь Performer Melody")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if prepared.Candidate().Artist != "Performer" {
		t.Fatal("artist context overrode explicit artist/higher metadata score", prepared.Candidate())
	}
}

func TestImplicitComparisonArtistHintRequiresLiveMatchingOwnSession(t *testing.T) {
	for _, kind := range []string{"absent", "cleared", "foreign", "speaker", "revoked", "expired"} {
		t.Run(kind, func(t *testing.T) {
			queries := new(atomic.Int64)
			p := implicitArtistFixture(t, `{"collection":[{"id":2,"title":"Wanted","duration":120000,"metadata_artist":"Other"}]}`, `{"collection":[{"id":3,"title":"Wanted","duration":120000,"metadata_artist":"Hint"}]}`, queries, false)
			p.cfg.CompareYandex = true
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			current := providerStart(t, p, ctx, r)
			state := sound.PlaybackState{Present: true, StreamID: current.StreamID, PlayerState: "Stopped"}
			switch kind {
			case "absent":
				state = sound.PlaybackState{}
			case "cleared":
				state.StreamID = ""
			case "foreign":
				state.StreamID = "yandex"
			case "speaker":
				ctx = providerContext(t, "b", new(providerRecorder))
			case "revoked":
				p.AcceptYandex(ctx)
			case "expired":
				p.now = func() time.Time { return time.Now().Add(4 * time.Hour) }
			}
			prepared, err := p.Prepare(sound.WithPlaybackState(ctx, state), "включи Wanted")
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if queries.Load() != 0 || prepared.Candidate().Artist != "Other" {
				t.Fatal("artist hint leaked from unproven/retired session", queries.Load(), prepared.Candidate())
			}
		})
	}
}

func TestImplicitComparisonScopedFailureKeepsGlobalCandidate(t *testing.T) {
	queries := new(atomic.Int64)
	p := implicitArtistFixture(t, `{"collection":[{"id":2,"title":"Wanted","duration":120000,"metadata_artist":"Hint"}]}`, "", queries, true)
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	current := providerStart(t, p, ctx, r)
	prepared, err := p.Prepare(sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: current.StreamID}), "включи Wanted")
	if err != nil || prepared == nil {
		t.Fatal(prepared, err)
	}
	defer prepared.Close()
	if prepared.Candidate().Artist != "Hint" || queries.Load() != 1 {
		t.Fatal(prepared.Candidate(), queries.Load())
	}
}

func TestImplicitComparisonArtistSearchesShareCancellation(t *testing.T) {
	entered := make(chan string, 2)
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		q := strings.ToLower(r.URL.Query().Get("q"))
		if q == "first" {
			fmt.Fprint(w, `{"collection":[{"id":1,"title":"Current","duration":120000,"metadata_artist":"Hint"}]}`)
			return
		}
		entered <- q
		<-r.Context().Done()
	})
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	current := providerStart(t, p, ctx, r)
	observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: current.StreamID})
	bounded, cancel := context.WithTimeout(observed, 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		prepared, err := p.Prepare(bounded, "включи Wanted")
		if prepared != nil {
			prepared.Close()
			done <- fmt.Errorf("canceled preparation returned candidate")
			return
		}
		done <- err
	}()
	for range 2 {
		select {
		case <-entered:
		case <-bounded.Done():
			t.Fatal("global and artist-scoped searches did not run concurrently")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("shared cancellation was lost", err)
	}
	if len(r.all()) != 1 || !p.CanDeliver(ctx, current) {
		t.Fatal("canceled contextual preparation mutated queue")
	}
}

func TestImplicitComparisonContextMergeIsDeduplicatedAndBounded(t *testing.T) {
	queries := new(atomic.Int64)
	p := implicitArtistFixture(t, `{"collection":[{"id":2,"title":"Wanted","duration":120000,"metadata_artist":"Other"},{"id":3,"title":"Wanted","duration":120000,"metadata_artist":"Hint"}]}`, `{"collection":[{"id":3,"title":"Wanted","duration":120000,"metadata_artist":"Hint"},{"id":4,"title":"Wanted","duration":120000,"metadata_artist":"Hint"}]}`, queries, false)
	p.cfg.CompareYandex = true
	p.cfg.QueueLimit = 2
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	current := providerStart(t, p, ctx, r)
	observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: current.StreamID})
	prepared, err := p.Prepare(observed, "включи Wanted")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if err = prepared.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	first := r.all()[1]
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first))
	second := r.all()[2]
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(second))
	if len(r.all()) != 3 || first.TrackID != "soundcloud:tracks:3" || second.TrackID != "soundcloud:tracks:4" {
		t.Fatal("merged implicit queue duplicated or exceeded its limit", r.all())
	}
}

func TestImplicitComparisonSlowArtistLookupPreservesGlobalAndStreamBudget(t *testing.T) {
	var streams atomic.Int64
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			streams.Add(1)
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		switch strings.ToLower(r.URL.Query().Get("q")) {
		case "first":
			fmt.Fprint(w, `{"collection":[{"id":1,"title":"Current","duration":120000,"metadata_artist":"Hint"}]}`)
		case "wanted hint":
			<-r.Context().Done()
		default:
			fmt.Fprint(w, `{"collection":[{"id":2,"title":"Wanted","duration":120000,"metadata_artist":"Hint"}]}`)
		}
	})
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	current := providerStart(t, p, ctx, r)
	observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: current.StreamID})
	bounded, cancel := context.WithTimeout(observed, time.Second)
	defer cancel()
	before := streams.Load()
	prepared, err := p.Prepare(bounded, "включи Wanted")
	if err != nil || prepared == nil {
		t.Fatal("optional scoped lookup consumed global stream budget", prepared, err)
	}
	defer prepared.Close()
	if prepared.Candidate().Artist != "Hint" || streams.Load() != before+1 || bounded.Err() != nil {
		t.Fatal("global candidate was lost or stream did not resolve before parent deadline", prepared.Candidate(), streams.Load(), bounded.Err())
	}
}

func TestImplicitComparisonFailedArtistLookupRejectsForeignHomonym(t *testing.T) {
	queries := new(atomic.Int64)
	p := implicitArtistFixture(t, `{"collection":[{"id":2,"title":"Wanted","duration":120000,"metadata_artist":"Other"}]}`, "", queries, true)
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	current := providerStart(t, p, ctx, r)
	observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: current.StreamID, PlayerState: "Paused"})
	prepared, err := p.Prepare(observed, "включи Wanted")
	if prepared != nil {
		prepared.Close()
		t.Fatal("incomplete artist lookup selected a foreign same-title track")
	}
	if !errors.Is(err, ErrUnavailable) || queries.Load() != 1 || len(r.all()) != 1 || !p.CanDeliver(ctx, current) {
		t.Fatal("failed lookup did not preserve original playback", err, queries.Load())
	}
}

func TestImplicitComparisonCompletedEmptyArtistLookupAllowsGlobal(t *testing.T) {
	queries := new(atomic.Int64)
	p := implicitArtistFixture(t, `{"collection":[{"id":2,"title":"Wanted","duration":120000,"metadata_artist":"Other"}]}`, `{"collection":[]}`, queries, false)
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	current := providerStart(t, p, ctx, r)
	prepared, err := p.Prepare(sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: current.StreamID}), "включи Wanted")
	if err != nil || prepared == nil {
		t.Fatal(prepared, err)
	}
	defer prepared.Close()
	if prepared.Candidate().Artist != "Other" {
		t.Fatal(prepared.Candidate())
	}
}

func TestImplicitComparisonFailedArtistLookupKeepsExplicitOtherArtist(t *testing.T) {
	queries := new(atomic.Int64)
	p := implicitArtistFixture(t, `{"collection":[{"id":2,"title":"Wanted","duration":120000,"metadata_artist":"Other"}]}`, "", queries, true)
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	current := providerStart(t, p, ctx, r)
	prepared, err := p.Prepare(sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: current.StreamID}), "включи Other Wanted")
	if err != nil || prepared == nil {
		t.Fatal(prepared, err)
	}
	defer prepared.Close()
	if prepared.Candidate().Artist != "Other" {
		t.Fatal(prepared.Candidate())
	}
}

func TestImplicitComparisonCachedOwnArtistSurvivesLookupFailure(t *testing.T) {
	for _, failure := range []string{"scoped", "both", "timeout", "stream"} {
		t.Run(failure, func(t *testing.T) {
			var cachedStreams, foreignStreams atomic.Int64
			p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/streams") {
					if strings.Contains(r.URL.Path, "tracks:2/") {
						cachedStreams.Add(1)
						if failure == "stream" {
							w.WriteHeader(http.StatusNotFound)
							return
						}
					}
					if strings.Contains(r.URL.Path, "tracks:3/") {
						foreignStreams.Add(1)
					}
					fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
					return
				}
				switch strings.ToLower(r.URL.Query().Get("q")) {
				case "first":
					fmt.Fprint(w, `{"collection":[{"id":1,"title":"Current","duration":120000,"metadata_artist":"Hint"},{"id":2,"title":"Wanted","duration":120000,"metadata_artist":"Hint"},{"id":3,"title":"Wanted","duration":120000,"metadata_artist":"Other"}]}`)
				case "wanted hint":
					if failure == "timeout" {
						<-r.Context().Done()
						return
					}
					w.WriteHeader(http.StatusServiceUnavailable)
				default:
					if failure == "both" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					fmt.Fprint(w, `{"collection":[{"id":3,"title":"Wanted","duration":120000,"metadata_artist":"Other"}]}`)
				}
			})
			p.cfg.CompareYandex = true
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			current := providerStart(t, p, ctx, r)
			observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: current.StreamID, PlayerState: "Paused"})
			bounded, cancel := context.WithTimeout(observed, time.Second)
			defer cancel()
			prepared, err := p.Prepare(bounded, "включи Wanted")
			if failure == "stream" {
				if prepared != nil {
					prepared.Close()
					t.Fatal("unavailable cached track fell through to foreign homonym")
				}
				if !errors.Is(err, ErrUnavailable) || cachedStreams.Load() != 1 || foreignStreams.Load() != 0 {
					t.Fatal(err, cachedStreams.Load(), foreignStreams.Load())
				}
				return
			}
			if err != nil || prepared == nil {
				t.Fatal("known own queue candidate was lost", prepared, err)
			}
			defer prepared.Close()
			if prepared.Candidate().Artist != "Hint" || cachedStreams.Load() != 1 || foreignStreams.Load() != 0 {
				t.Fatal("cached metadata was not revalidated or foreign cache entry leaked", prepared.Candidate(), cachedStreams.Load(), foreignStreams.Load())
			}
			if len(r.all()) != 1 || !p.CanDeliver(ctx, current) {
				t.Fatal("cached comparison preparation mutated playback")
			}
			if err = prepared.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if got := r.all()[1]; got.TrackID != "soundcloud:tracks:2" {
				t.Fatal("wrong cached track committed", got)
			}
		})
	}
}

func TestImplicitComparisonArtistLookupLimitIsSmall(t *testing.T) {
	var globalLimit, scopedLimit string
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		q := strings.ToLower(r.URL.Query().Get("q"))
		if q == "first" {
			fmt.Fprint(w, `{"collection":[{"id":1,"title":"Current","duration":120000,"metadata_artist":"Hint"}]}`)
			return
		}
		if q == "wanted hint" {
			scopedLimit = r.URL.Query().Get("limit")
		} else {
			globalLimit = r.URL.Query().Get("limit")
		}
		fmt.Fprint(w, `{"collection":[{"id":2,"title":"Wanted","duration":120000,"metadata_artist":"Hint"}]}`)
	})
	p.cfg.CompareYandex = true
	p.cfg.QueueLimit = 50
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	current := providerStart(t, p, ctx, r)
	prepared, err := p.Prepare(sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: current.StreamID}), "включи Wanted")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if globalLimit != "50" || scopedLimit != "10" {
		t.Fatal("artist lookup did not keep its small independent result budget", globalLimit, scopedLimit)
	}
}

func TestImplicitComparisonCachedCandidateKeepsBudgetWhenBothCatalogsHang(t *testing.T) {
	var resolved atomic.Int64
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			if strings.Contains(r.URL.Path, "tracks:2/") {
				resolved.Add(1)
			}
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		if strings.ToLower(r.URL.Query().Get("q")) == "first" {
			fmt.Fprint(w, `{"collection":[{"id":1,"title":"Current","duration":120000,"metadata_artist":"Hint"},{"id":2,"title":"Wanted","duration":120000,"metadata_artist":"Hint"}]}`)
			return
		}
		<-r.Context().Done()
	})
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	current := providerStart(t, p, ctx, r)
	observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: current.StreamID, PlayerState: "Paused"})
	bounded, cancel := context.WithTimeout(observed, time.Second)
	defer cancel()
	prepared, err := p.Prepare(bounded, "включи Wanted")
	if err != nil || prepared == nil {
		t.Fatal("hanging catalogs consumed cached stream budget", prepared, err)
	}
	defer prepared.Close()
	if prepared.Candidate().Artist != "Hint" || resolved.Load() != 1 || bounded.Err() != nil {
		t.Fatal("cached metadata was not revalidated before parent deadline", prepared.Candidate(), resolved.Load(), bounded.Err())
	}
}
