package soundcloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"quasarbridge/internal/sound"
)

func TestComparisonIntentIsNarrowAndIndependentOfDefault(t *testing.T) {
	p := providerFixture(t, nil)
	p.cfg.CompareYandex = true
	p.SetDefault(false)
	ctx := providerContext(t, "speaker", new(providerRecorder))
	for _, text := range []string{"включи песню First", "поставь трек First", "play song First", "сыграй песню Что-нибудь спокойное", "play song Something relaxing", "включи песню Радио"} {
		if !p.ComparisonEnabled(ctx, text) {
			t.Errorf("not eligible %q", text)
		}
	}
	for _, text := range []string{"сыграй что-нибудь спокойное", "play something relaxing", "сыграй спокойную музыку", "play relaxing music", "включи песню First на SoundCloud", "включи песню First на Яндекс Музыке", "включи https://soundcloud.com/artist/track", "найди песню First", "включи исполнителя First", "включи альбом First", "включи плейлист First", "включи музыку для сна", "включи радио", "включи мою волну", "включи музыку", "пауза", "следующий трек", "включи SoundCloud по умолчанию", "включи кондиционер", "не включай песню First"} {
		if p.ComparisonEnabled(ctx, text) {
			t.Errorf("unexpected comparison %q", text)
		}
	}
	p.cfg.CompareYandex = false
	if p.ComparisonEnabled(ctx, "включи песню First") {
		t.Fatal("disabled comparison accepted")
	}
}

func TestComparisonPreparationAndClosePreserveExistingQueue(t *testing.T) {
	p := providerFixture(t, nil)
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := sound.WithRequestSequence(providerContext(t, "speaker", r), 1)
	first := providerStart(t, p, ctx, r)
	prepared, err := p.Prepare(sound.WithRequestSequence(ctx, 2), "включи песню First")
	if err != nil {
		t.Fatal(err)
	}
	if c := prepared.Candidate(); c.Title != "First" || c.Artist != "Artist" || c.DurationMS != 120000 {
		t.Fatal(c)
	}
	prepared.Close()
	if !p.CanDeliver(ctx, first) || len(r.all()) != 1 {
		t.Fatal("preparation changed old queue")
	}
	if err := prepared.Commit(sound.WithRequestSequence(ctx, 2)); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed commit: %v", err)
	}
}

func TestComparisonCommitUsesPreparedStreamWithoutHTTP(t *testing.T) {
	var requests atomic.Int32
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First (cover)","duration":120000,"metadata_artist":"Other"},{"id":2,"title":"First","duration":120000,"metadata_artist":"Artist"}]}`)
	})
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := sound.WithRequestSequence(providerContext(t, "speaker", r), 4)
	prepared, err := p.Prepare(ctx, "включи песню First")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	before := requests.Load()
	if err := prepared.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != before {
		t.Fatal("commit made network request")
	}
	req := r.all()[0]
	if req.Title != "First" || req.Subtitle != "Artist" || req.TrackID != "soundcloud:tracks:2" || !p.CanDeliver(ctx, req) {
		t.Fatalf("wrong committed metadata %+v", req)
	}
	if err := prepared.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("duplicate commit succeeded")
	}
}

func TestComparisonFailedStreamOrCommitKeepsOldQueue(t *testing.T) {
	for _, failure := range []string{"stream", "enqueue", "canceled", "stale"} {
		t.Run(failure, func(t *testing.T) {
			p := providerFixture(t, nil)
			p.cfg.CompareYandex = true
			r := new(providerRecorder)
			ctx := sound.WithRequestSequence(providerContext(t, "speaker", r), 5)
			first := providerStart(t, p, ctx, r)
			if failure == "stream" {
				p.client = nil
				if _, err := p.Prepare(ctx, "включи песню First"); err == nil {
					t.Fatal("missing stream accepted")
				}
				if !p.CanDeliver(ctx, first) {
					t.Fatal("failed prepare changed queue")
				}
				return
			}
			prepared, err := p.Prepare(ctx, "включи песню First")
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			commitCtx := ctx
			switch failure {
			case "enqueue":
				r.reject = true
			case "canceled":
				var cancel context.CancelFunc
				commitCtx, cancel = context.WithCancel(ctx)
				cancel()
			case "stale":
				if err := p.AcceptYandex(sound.WithRequestSequence(ctx, 6)); err != nil {
					t.Fatal(err)
				}
			}
			if err := prepared.Commit(commitCtx); err == nil {
				t.Fatal("failed commit succeeded")
			}
			if failure != "stale" && !p.CanDeliver(ctx, first) {
				t.Fatal("failed commit invalidated old queue")
			}
		})
	}
}

func TestAcceptYandexRevokesSCAndRejectsOlderComparison(t *testing.T) {
	p := providerFixture(t, nil)
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := sound.WithRequestSequence(providerContext(t, "speaker", r), 1)
	first := providerStart(t, p, ctx, r)
	prepared, err := p.Prepare(ctx, "включи песню First")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if err := p.AcceptYandex(sound.WithRequestSequence(ctx, 2)); err != nil {
		t.Fatal(err)
	}
	if p.CanDeliver(ctx, first) {
		t.Fatal("old SC directive still eligible")
	}
	if _, err := p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first)); err != nil {
		t.Fatal(err)
	}
	if len(r.all()) != 1 {
		t.Fatal("old SC callback advanced after Yandex win")
	}
	if err := prepared.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("older commit: %v", err)
	}
	if err := p.AcceptYandex(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("older Yandex acceptance succeeded")
	}
}

func TestComparisonPrepareStreamHTTPFailureKeepsOldQueue(t *testing.T) {
	var fail atomic.Bool
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			if fail.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist"}]}`)
	})
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := providerContext(t, "speaker", r)
	first := providerStart(t, p, ctx, r)
	fail.Store(true)
	if prepared, err := p.Prepare(ctx, "включи песню First"); err == nil || prepared != nil {
		t.Fatal("HTTP stream failure accepted")
	}
	if !p.CanDeliver(ctx, first) || len(r.all()) != 1 {
		t.Fatal("failed stream changed former queue")
	}
}

func TestPreparedComparisonRejectsCrossSpeakerAndNewerSCRequest(t *testing.T) {
	p := providerFixture(t, nil)
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := sound.WithRequestSequence(providerContext(t, "speaker", r), 1)
	prepared, err := p.Prepare(ctx, "включи песню First")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if err := prepared.Commit(sound.WithRequestSequence(providerContext(t, "other", r), 1)); !errors.Is(err, context.Canceled) {
		t.Fatal("cross speaker commit accepted")
	}
	newer := sound.WithRequestSequence(ctx, 2)
	first := providerStart(t, p, newer, r)
	if err := prepared.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("late prepared commit accepted")
	}
	if !p.CanDeliver(newer, first) {
		t.Fatal("late commit changed newer queue")
	}
}

func TestComparisonCanceledPreparationDoesNotSearch(t *testing.T) {
	p := providerFixture(t, nil)
	p.cfg.CompareYandex = true
	ctx, cancel := context.WithCancel(providerContext(t, "speaker", new(providerRecorder)))
	cancel()
	if _, err := p.Prepare(ctx, "включи песню First"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled prepare: %v", err)
	}
}

type comparisonCheckedContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *comparisonCheckedContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}

func TestAcceptYandexCanceledWhileWaitingDoesNotRevoke(t *testing.T) {
	p := providerFixture(t, nil)
	p.cfg.CompareYandex = true
	r := new(providerRecorder)
	ctx := providerContext(t, "speaker", r)
	first := providerStart(t, p, ctx, r)
	base, cancel := context.WithCancel(ctx)
	observed := &comparisonCheckedContext{Context: base, checked: make(chan struct{})}
	p.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- p.AcceptYandex(observed) }()
	<-observed.checked
	cancel()
	p.mu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acceptance: %v", err)
	}
	if !p.CanDeliver(ctx, first) {
		t.Fatal("canceled acceptance revoked old SC")
	}
}
