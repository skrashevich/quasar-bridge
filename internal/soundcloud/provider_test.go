package soundcloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

type providerRecorder struct {
	mu       sync.Mutex
	requests []sound.Request
	reject   bool
}

func (r *providerRecorder) play(_ context.Context, req sound.Request) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reject {
		return fmt.Errorf("rejected")
	}
	r.requests = append(r.requests, req)
	return nil
}
func (r *providerRecorder) all() []sound.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sound.Request(nil), r.requests...)
}
func providerContext(t *testing.T, speaker string, r *providerRecorder) context.Context {
	return sound.WithPlayer(dialogue.WithSpeaker(t.Context(), speaker), r.play)
}
func providerFixture(t *testing.T, extra http.HandlerFunc) *Provider {
	t.Helper()
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if extra != nil {
			extra(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist","artwork_url":"https://i1.sndcdn.com/cover.jpg","permalink_url":"https://soundcloud.com/artist/first"},{"id":2,"title":"Second","duration":180000,"metadata_artist":"Artist"}]}`)
	})
	cfg := Config{Enabled: true, Default: true, API: "official", AccessToken: "test"}
	client, err := NewClient(cfg, hc)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := NewRelay("http://bridge:8080", hc)
	if err != nil {
		t.Fatal(err)
	}
	return NewProvider(cfg, client, relay)
}
func eventPayload(req sound.Request) map[string]any {
	return map[string]any{"session_id": req.SessionID, "stream_id": req.StreamID, "token": req.CallbackToken}
}
func TestProviderMetadataAndSpeakerIsolation(t *testing.T) {
	p := providerFixture(t, nil)
	a, b := new(providerRecorder), new(providerRecorder)
	ctx := providerContext(t, "a", a)
	if _, err := p.Handle(ctx, "включи First на SoundCloud"); err != nil {
		t.Fatal(err)
	}
	req := a.all()[0]
	if req.Provider != "SoundCloud" || req.Format != "HLS" || req.Kind != "Track" || req.Title != "First" || req.Subtitle != "Artist" || req.DurationMS != 120000 || req.TrackID != "soundcloud:tracks:1" || req.SessionID == "" || req.CallbackToken == "" || req.NextTrackID != "soundcloud:tracks:2" || !strings.HasPrefix(req.ArtworkURL, "http://bridge:8080/soundcloud/") {
		t.Fatalf("metadata: %+v", req)
	}
	_ = map[sound.Request]bool{req: true}
	if p.CanHandle(ctx, "что играет") {
		t.Fatal("pending is not confirmed playback")
	}
	if handled, err := p.HandleEvent(ctx, EventPrefix+"started", eventPayload(req)); !handled || err != nil {
		t.Fatal(handled, err)
	}
	if !p.CanHandle(ctx, "что играет") || p.CanHandle(providerContext(t, "b", b), "что играет") {
		t.Fatal("ownership leaked")
	}
	text, err := p.Handle(ctx, "что играет")
	if err != nil || !strings.Contains(text, "First") || !strings.Contains(text, "Artist") {
		t.Fatal(text, err)
	}
	if _, err := p.HandleEvent(providerContext(t, "b", b), EventPrefix+"finished", eventPayload(req)); err != nil {
		t.Fatal(err)
	}
	if len(b.all()) != 0 || len(a.all()) != 1 {
		t.Fatal("cross speaker event played")
	}
}

func providerStart(t *testing.T, p *Provider, ctx context.Context, r *providerRecorder) sound.Request {
	t.Helper()
	if text, err := p.Handle(ctx, "включи First SoundCloud"); err != nil || text != "" {
		t.Fatal(text, err)
	}
	req := r.all()[len(r.all())-1]
	if _, err := p.HandleEvent(ctx, EventPrefix+"started", eventPayload(req)); err != nil {
		t.Fatal(err)
	}
	return req
}
func TestProviderControlsAndCallbacks(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	paused := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 42000})
	if _, err := p.Handle(paused, "пауза"); err != nil {
		t.Fatal(err)
	}
	cleared := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true})
	if !p.CanHandle(cleared, "продолжи") {
		t.Fatal("paused ownership lost")
	}
	if text, err := p.Handle(cleared, "продолжи"); err != nil || text != "" {
		t.Fatal(text, err)
	}
	resumed := r.all()[2]
	if resumed.OffsetMS != 42000 || resumed.TrackID != first.TrackID || resumed.SessionID != first.SessionID || resumed.StreamID == first.StreamID {
		t.Fatalf("resume %+v", resumed)
	}
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(resumed))
	p.Handle(ctx, "перемотай вперед на 10 секунд")
	seek := r.all()[3]
	if seek.OffsetMS != 52000 {
		t.Fatalf("seek %+v", seek)
	}
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(seek))
	p.Handle(ctx, "начни сначала")
	restart := r.all()[4]
	if restart.OffsetMS != 0 {
		t.Fatal(restart)
	}
	// Older streams, wrong tokens and other speaker callbacks cannot advance.
	for _, req := range []sound.Request{first, resumed, seek} {
		p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(req))
	}
	bad := eventPayload(restart)
	bad["token"] = "wrong"
	p.HandleEvent(ctx, EventPrefix+"finished", bad)
	if len(r.all()) != 5 {
		t.Fatal("stale callback advanced")
	}
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(restart))
	second := r.all()[5]
	if second.TrackID != "soundcloud:tracks:2" || second.PreviousTrackID != first.TrackID {
		t.Fatal(second)
	}
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(restart))
	if len(r.all()) != 6 {
		t.Fatal("duplicate callback advanced")
	}
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(second))
	p.Handle(ctx, "стоп")
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(second))
	if len(r.all()) != 7 || r.all()[6].Command != "stop" || p.CanHandle(ctx, "что играет") {
		t.Fatal("stop progressed")
	}
}
func TestProviderForeignStreamInvalidatesOwnership(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	req := providerStart(t, p, ctx, r)
	foreign := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: "foreign", PlayerState: "Playing"})
	if p.CanHandle(foreign, "что играет") {
		t.Fatal("foreign claimed")
	}
	if p.CanHandle(ctx, "что играет") {
		t.Fatal("ownership revived after foreign playback")
	}
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(req))
	if len(r.all()) != 1 {
		t.Fatal("old callback replaced foreign stream")
	}
}
func TestProviderRejectedPlaybackPreservesCommittedTrack(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	r.reject = true
	if text, err := p.Handle(ctx, "следующий"); err != nil || text == "" {
		t.Fatal(text, err)
	}
	r.reject = false
	text, _ := p.Handle(ctx, "что играет")
	if !strings.Contains(text, "First") {
		t.Fatal(text)
	}
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first))
	if len(r.all()) != 2 || r.all()[1].Title != "Second" {
		t.Fatal(r.all())
	}
}
func TestProviderSearchResultsAndLikesAuthorization(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	text, err := p.Handle(ctx, "найди трек First SoundCloud")
	if err != nil || !strings.Contains(text, "1. Artist — First") || len(r.all()) != 0 {
		t.Fatal(text, err, r.all())
	}
	if !p.CanHandle(ctx, "включи номер 2") {
		t.Fatal("selection not routed")
	}
	p.Handle(ctx, "включи номер 2")
	if len(r.all()) != 1 || r.all()[0].Title != "Second" {
		t.Fatal(r.all())
	}
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(r.all()[0]))
	p.client.userAuth = false
	for _, cmd := range []string{"лайк", "убери лайк", "включи избранное SoundCloud"} {
		text, err = p.Handle(ctx, cmd)
		if err != nil || !strings.Contains(text, "авторизация") {
			t.Fatal(cmd, text, err)
		}
	}
}
func TestProviderConcurrentTerminalDedupAndRepeatFailure(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	req := providerStart(t, p, ctx, r)
	p.Handle(ctx, "повторяй всё")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { p.HandleEvent(ctx, EventPrefix+"failed", eventPayload(req)) })
	}
	wg.Wait()
	if len(r.all()) != 2 || r.all()[1].Title != "Second" {
		t.Fatal(r.all())
	}
	p.HandleEvent(ctx, EventPrefix+"failed", eventPayload(r.all()[1]))
	if len(r.all()) != 2 || p.CanHandle(ctx, "следующий") {
		t.Fatal("failed repeat loop")
	}
}

func TestProviderLateSearchAndCanceledContext(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelContext), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("q") == "slow" {
					close(entered)
					<-release
				}
				if strings.HasSuffix(r.URL.Path, "/streams") {
					fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
					return
				}
				fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist"},{"id":2,"title":"Second","duration":180000,"metadata_artist":"Artist"}]}`)
			})
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			providerStart(t, p, ctx, r)
			slow, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := p.Handle(slow, "включи slow SoundCloud"); done <- err }()
			<-entered
			// Info must not cancel playback work; the old metadata remains coherent.
			if text, err := p.Handle(ctx, "что играет"); err != nil || !strings.Contains(text, "First") {
				t.Fatal(text, err)
			}
			if cancelContext {
				cancel()
			} else {
				p.Handle(ctx, "следующий")
			}
			close(release)
			if err := <-done; err == nil {
				t.Fatal("stale/canceled search was not rejected")
			}
			if cancelContext {
				if len(r.all()) != 1 {
					t.Fatal(r.all())
				}
			} else if len(r.all()) != 2 || r.all()[1].Title != "Second" {
				t.Fatal(r.all())
			}
		})
	}
}
func TestProviderInfoDoesNotCancelPendingNext(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tracks/soundcloud:tracks:2/streams" || r.URL.Path == "/tracks/2/streams" {
			close(entered)
			<-release
		}
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist"},{"id":2,"title":"Second","duration":180000,"metadata_artist":"Artist"}]}`)
	})
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	done := make(chan error, 1)
	go func() { _, err := p.Handle(ctx, "следующий"); done <- err }()
	<-entered
	p.Handle(ctx, "кто поёт")
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first))
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(r.all()) != 2 || r.all()[1].Title != "Second" {
		t.Fatal(r.all())
	}
}
func TestProviderCatalogExpansionAndPersonalOperations(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/users":
			fmt.Fprint(w, `{"collection":[{"id":8,"kind":"user","username":"Artist"}]}`)
		case "/playlists":
			fmt.Fprint(w, `{"collection":[{"id":9,"kind":"playlist","is_album":true,"title":"Collection"}]}`)
		case "/resolve":
			fmt.Fprint(w, `{"id":3,"kind":"track","title":"Resolved","duration":160000,"metadata_artist":"Artist"}`)
		case "/likes/tracks/soundcloud:tracks:1", "/likes/tracks/1":
			w.WriteHeader(http.StatusNoContent)
		default:
			if strings.HasSuffix(r.URL.Path, "/streams") {
				fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
				return
			}
			fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist"},{"id":2,"title":"Second","duration":180000,"metadata_artist":"Artist"}]}`)
		}
	})
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	for _, cmd := range []string{"включи исполнителя Artist SoundCloud", "включи плейлист Collection SoundCloud", "включи альбом Collection SoundCloud", "включи https://soundcloud.com/artist/song", "включи избранное SoundCloud"} {
		if text, err := p.Handle(ctx, cmd); err != nil || text != "" {
			t.Fatal(cmd, text, err)
		}
	}
	reqs := r.all()
	if len(reqs) != 5 || reqs[3].Title != "Resolved" {
		t.Fatal(reqs)
	}
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(reqs[4]))
	for _, cmd := range []string{"лайк", "убери лайк", "включи похожее"} {
		if text, err := p.Handle(ctx, cmd); err != nil {
			t.Fatal(cmd, text, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(seen, "\n")
	for _, part := range []string{"GET /users/", "GET /playlists/", "GET /resolve", "GET /me/likes/tracks", "POST /likes/tracks/", "DELETE /likes/tracks/", "/related"} {
		if !strings.Contains(joined, part) {
			t.Fatalf("missing %s in %s", part, joined)
		}
	}
}
func TestProviderBoundsAndTTL(t *testing.T) {
	p := providerFixture(t, nil)
	p.cfg.QueueLimit = 1
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	providerStart(t, p, ctx, r)
	if r.all()[0].NextTrackID != "" {
		t.Fatal("queue limit ignored")
	}
	now := p.now()
	p.now = func() time.Time { return now }
	p.mu.Lock()
	for i := range providerMaxSpeakers + 2 {
		p.sessionLocked(fmt.Sprint(i), true)
	}
	if len(p.sessions) != providerMaxSpeakers {
		t.Fatal(len(p.sessions))
	}
	p.mu.Unlock()
	now = now.Add(providerTTL)
	if p.CanHandle(ctx, "что играет") {
		t.Fatal("expired ownership")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sessions) != 0 {
		t.Fatal("TTL did not prune")
	}
}
func TestProviderModesAndReportedPlayback(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	p.Handle(ctx, "включи музыку")
	req := r.all()[0]
	reported := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: req.StreamID, PlayerState: "Playing"})
	if !p.CanHandle(reported, "what is playing") {
		t.Fatal("reported Playing did not confirm")
	}
	for _, cmd := range []string{"включи повтор", "перемешай", "выключи случайный порядок"} {
		p.Handle(reported, cmd)
	}
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(req))
	repeat := r.all()[1]
	if repeat.TrackID != req.TrackID || repeat.RepeatMode != "one" || repeat.Shuffle {
		t.Fatal(repeat)
	}
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(repeat))
	p.Handle(ctx, "выключи повтор")
	p.Handle(ctx, "следующий")
	second := r.all()[2]
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(second))
	p.Handle(ctx, "предыдущий")
	if r.all()[3].TrackID != req.TrackID {
		t.Fatal("previous lost queue identity")
	}
	for _, name := range []string{EventPrefix + "unknown", EventPrefix + "finished"} {
		if handled, err := p.HandleEvent(ctx, name, map[string]any{"token": 42}); !handled || err != nil {
			t.Fatal(handled, err)
		}
	}
	if handled, err := p.HandleEvent(ctx, "other_event", nil); handled || err != nil {
		t.Fatal(handled, err)
	}
}

func TestProviderModeChangeDuringResolutionIsPreserved(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tracks:2/streams") || r.URL.Path == "/tracks/2/streams" {
			close(entered)
			<-release
		}
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist"},{"id":2,"title":"Second","duration":180000,"metadata_artist":"Artist"}]}`)
	})
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	providerStart(t, p, ctx, r)
	done := make(chan error, 1)
	go func() { _, err := p.Handle(ctx, "следующий"); done <- err }()
	<-entered
	p.Handle(ctx, "повторяй всё")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.all()[1].RepeatMode != "all" {
		t.Fatal("late playback overwrote the new repeat mode")
	}
}
func TestProviderSkipsUnavailableBestMatch(t *testing.T) {
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tracks:1/streams") || r.URL.Path == "/tracks/1/streams" {
			fmt.Fprint(w, `{}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist"},{"id":2,"title":"Second","duration":180000,"metadata_artist":"Artist"}]}`)
	})
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	if text, err := p.Handle(ctx, "включи First SoundCloud"); err != nil || text != "" {
		t.Fatal(text, err)
	}
	if len(r.all()) != 1 || r.all()[0].Title != "Second" || r.all()[0].PreviousTrackID != "" {
		t.Fatal(r.all())
	}
}

func TestProviderReportedEndedStreamCannotRevive(t *testing.T) {
	for _, state := range []string{"cleared", "Stopped"} {
		t.Run(state, func(t *testing.T) {
			p := providerFixture(t, nil)
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			req := providerStart(t, p, ctx, r)
			report := sound.PlaybackState{Present: true}
			if state != "cleared" {
				report.StreamID = req.StreamID
				report.PlayerState = state
			}
			if p.CanHandle(sound.WithPlaybackState(ctx, report), "что играет") {
				t.Fatal("ended playback claimed")
			}
			if p.CanHandle(ctx, "следующий") {
				t.Fatal("ended ownership revived without a new start")
			}
		})
	}
}

func TestProviderDuplicateStartedCannotReviveEndedOwnership(t *testing.T) {
	for _, reported := range []string{"Idle", "cleared"} {
		t.Run(reported, func(t *testing.T) {
			p := providerFixture(t, nil)
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			req := providerStart(t, p, ctx, r)
			state := sound.PlaybackState{Present: true, StreamID: req.StreamID, PlayerState: reported}
			if reported == "cleared" {
				state.StreamID = ""
			}
			if p.CanHandle(sound.WithPlaybackState(ctx, state), "что играет") {
				t.Fatal("ended ownership")
			}
			p.HandleEvent(ctx, EventPrefix+"started", eventPayload(req))
			if p.CanHandle(ctx, "что играет") {
				t.Fatal("duplicate started revived ended ownership")
			}
			if len(r.all()) != 1 {
				t.Fatal("duplicate started queued audio")
			}
			// Ignoring duplicate start must not suppress the genuine natural finish.
			p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(req))
			if len(r.all()) != 2 || r.all()[1].Title != "Second" {
				t.Fatal(r.all())
			}
		})
	}
}
func TestProviderForeignObservedInCallbackRevokesLaterCallbacks(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	req := providerStart(t, p, ctx, r)
	foreign := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: "foreign", PlayerState: "Playing"})
	p.HandleEvent(foreign, EventPrefix+"finished", eventPayload(req))
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(req))
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(req))
	if len(r.all()) != 1 || p.CanHandle(ctx, "что играет") {
		t.Fatal("foreign observation failed to revoke old callback ownership")
	}
}
func TestProviderForeignObservedDuringCatalogRoutingRevokesCallbacks(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	req := providerStart(t, p, ctx, r)
	foreign := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: "foreign", PlayerState: "Playing"})
	if !p.CanHandle(foreign, "найди музыку на SoundCloud") {
		t.Fatal("catalog routing changed")
	}
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(req))
	if len(r.all()) != 1 {
		t.Fatal("catalog route ignored foreign observation")
	}
}

func TestProviderLaggingOwnReportDoesNotRevokePendingReplacement(t *testing.T) {
	for _, reported := range []string{"previous", "cleared"} {
		t.Run(reported, func(t *testing.T) {
			p := providerFixture(t, nil)
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			first := providerStart(t, p, ctx, r)
			p.Handle(ctx, "следующий")
			second := r.all()[1]
			state := sound.PlaybackState{Present: true}
			if reported == "previous" {
				state.StreamID = first.StreamID
				state.PlayerState = "Playing"
				state.OffsetKnown = true
				state.OffsetMS = 110000
			}
			lagging := sound.WithPlaybackState(ctx, state)
			if p.CanHandle(lagging, "что играет") {
				t.Fatal("lagging state confirmed pending replacement")
			}
			p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first))
			if len(r.all()) != 2 {
				t.Fatal("old finish advanced pending replacement")
			}
			p.HandleEvent(lagging, EventPrefix+"started", eventPayload(second))
			if !p.CanHandle(ctx, "что играет") {
				t.Fatal("pending replacement was revoked by previous/cleared report")
			}
			text, err := p.Handle(ctx, "что играет")
			if err != nil || !strings.Contains(text, "Second") {
				t.Fatal(text, err)
			}
			if _, err = p.Handle(ctx, "предыдущий"); err != nil {
				t.Fatal(err)
			}
			if len(r.all()) != 3 || r.all()[2].TrackID != first.TrackID || r.all()[2].OffsetMS != 0 {
				t.Fatal(r.all())
			}
		})
	}
}
func TestProviderActualForeignStillRevokesPendingReplacement(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	providerStart(t, p, ctx, r)
	p.Handle(ctx, "следующий")
	second := r.all()[1]
	foreign := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: "foreign", PlayerState: "Playing"})
	if p.CanHandle(foreign, "что играет") {
		t.Fatal("foreign ownership claimed")
	}
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(second))
	if p.CanHandle(ctx, "что играет") {
		t.Fatal("foreign pending replacement revived")
	}
}

func TestProviderArrivalSequenceRejectsLatePlaybackMutations(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	base := providerContext(t, "a", r)
	newest := sound.WithRequestSequence(base, 20)
	providerStart(t, p, newest, r)
	for _, command := range []string{"включи Second SoundCloud", "стоп SoundCloud", "перемешай SoundCloud", "найди трек Second SoundCloud"} {
		stale := sound.WithPlaybackState(sound.WithRequestSequence(base, 10), sound.PlaybackState{Present: true, StreamID: "foreign", PlayerState: "Playing"})
		if !p.CanHandle(stale, command) {
			t.Fatal("stale SoundCloud mutation escaped routing", command)
		}
		text, err := p.Handle(stale, command)
		if text != "" || !errors.Is(err, context.Canceled) {
			t.Fatal(command, text, err)
		}
	}
	if len(r.all()) != 1 || !p.CanHandle(base, "что играет") {
		t.Fatal("old arrival replaced/stopped/revoked newer playback", r.all())
	}
	// Unsequenced legacy requests still work, without lowering the saved sequence.
	if text, err := p.Handle(base, "следующий"); text != "" || err != nil {
		t.Fatal(text, err)
	}
	if _, err := p.Handle(sound.WithRequestSequence(base, 15), "включи First SoundCloud"); !errors.Is(err, context.Canceled) {
		t.Fatal("seq0 reset latest sequence", err)
	}
	if len(r.all()) != 2 {
		t.Fatal(r.all())
	}
	// A different speaker has independent arrival order.
	other := new(providerRecorder)
	if text, err := p.Handle(sound.WithRequestSequence(providerContext(t, "b", other), 1), "включи First SoundCloud"); text != "" || err != nil {
		t.Fatal(text, err)
	}
	if len(other.all()) != 1 {
		t.Fatal("sequence leaked across speakers")
	}
}
func TestProviderNewerInfoDoesNotSupersedePendingSequencedPlay(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "slow" {
			close(entered)
			<-release
		}
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist"},{"id":2,"title":"Second","duration":180000,"metadata_artist":"Artist"}]}`)
	})
	r := new(providerRecorder)
	base := providerContext(t, "a", r)
	providerStart(t, p, sound.WithRequestSequence(base, 5), r)
	done := make(chan error, 1)
	go func() {
		_, err := p.Handle(sound.WithRequestSequence(base, 10), "включи slow SoundCloud")
		done <- err
	}()
	<-entered
	for _, command := range []string{"что играет", "кто поёт", "лайк", "убери лайк"} {
		if _, err := p.Handle(sound.WithRequestSequence(base, 30), command); err != nil {
			t.Fatal(command, err)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(r.all()) != 2 {
		t.Fatal("info canceled pending play", r.all())
	}
	// Info must not raise the arrival watermark above the pending play's sequence.
	if text, err := p.Handle(sound.WithRequestSequence(base, 20), "включи First SoundCloud"); text != "" || err != nil {
		t.Fatal(text, err)
	}
	if len(r.all()) != 3 {
		t.Fatal(r.all())
	}
}

func TestProviderStaleSequenceDoesNotEnableDefaultMusicRouting(t *testing.T) {
	p := providerFixture(t, nil)
	p.SetDefault(false)
	r := new(providerRecorder)
	base := providerContext(t, "a", r)
	providerStart(t, p, sound.WithRequestSequence(base, 20), r)
	old := sound.WithRequestSequence(base, 10)
	if p.CanHandle(old, "включи песню Second") {
		t.Fatal("stale sequence bypassed default=false")
	}
	if text, err := p.Handle(old, "включи песню Second"); text != "" || err != nil {
		t.Fatal(text, err)
	}
	if len(r.all()) != 1 {
		t.Fatal(r.all())
	}
}

func TestProviderDeliveryValidationRejectsSupersededRequests(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	if !p.CanDeliver(ctx, first) {
		t.Fatal("current playback cannot deliver")
	}
	p.Handle(ctx, "что играет")
	p.Handle(ctx, "кто поёт")
	p.Handle(ctx, "лайк")
	p.Handle(ctx, "повторяй всё")
	if !p.CanDeliver(ctx, first) {
		t.Fatal("metadata/mode request invalidated pending delivery")
	}
	p.Handle(ctx, "следующий")
	second := r.all()[1]
	if p.CanDeliver(ctx, first) || !p.CanDeliver(ctx, second) {
		t.Fatal("superseded delivery eligibility")
	}
	if p.CanDeliver(providerContext(t, "b", new(providerRecorder)), second) {
		t.Fatal("cross speaker delivery")
	}
	wrong := second
	wrong.CallbackToken = "bad"
	if p.CanDeliver(ctx, wrong) {
		t.Fatal("bad token delivery")
	}
	wrong = second
	wrong.StreamID = "bad"
	if p.CanDeliver(ctx, wrong) {
		t.Fatal("bad stream delivery")
	}
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(second))
	p.Handle(ctx, "пауза")
	pause := r.all()[2]
	if pause.Provider != "SoundCloud" || pause.SessionID != second.SessionID || pause.StreamID != "" || pause.Generation == 0 || !p.CanDeliver(ctx, pause) || p.CanDeliver(ctx, second) {
		t.Fatal("pause guard", pause)
	}
	p.Handle(ctx, "продолжи")
	resumed := r.all()[3]
	if p.CanDeliver(ctx, pause) || !p.CanDeliver(ctx, resumed) {
		t.Fatal("queued pause survived resumed stream")
	}
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(resumed))
	p.Handle(ctx, "стоп")
	stop := r.all()[4]
	if stop.Provider != "SoundCloud" || stop.StreamID != "" || stop.Generation == 0 || !p.CanDeliver(ctx, stop) {
		t.Fatal("stop guard", stop)
	}
	p.Handle(ctx, "включи First SoundCloud")
	latest := r.all()[5]
	if p.CanDeliver(ctx, stop) || !p.CanDeliver(ctx, latest) {
		t.Fatal("queued stop survived new playback")
	}
	if !p.CanDeliver(ctx, sound.Request{Sound: "other"}) {
		t.Fatal("non SC request was blocked")
	}
}
func TestProviderDeliveryValidationRejectsReservedAndRevokedGeneration(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "slow" {
			close(entered)
			<-release
		}
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist"}]}`)
	})
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	done := make(chan error, 1)
	go func() { _, err := p.Handle(ctx, "включи slow SoundCloud"); done <- err }()
	<-entered
	if p.CanDeliver(ctx, first) {
		t.Fatal("reserved newer generation did not supersede queued sound")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	latest := r.all()[1]
	foreign := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: "foreign", PlayerState: "Playing"})
	p.CanHandle(foreign, "что играет")
	if p.CanDeliver(ctx, latest) {
		t.Fatal("revoked generation delivered")
	}
}

func TestProviderLongTrackLeaseAndIdleTTL(t *testing.T) {
	for _, status := range []string{"pending", "playing", "paused", "stopped"} {
		t.Run(status, func(t *testing.T) {
			p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/streams") {
					fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
					return
				}
				fmt.Fprint(w, `{"collection":[{"id":1,"title":"Long Mix","duration":10800000,"metadata_artist":"Artist"}]}`)
			})
			now := time.Now()
			p.now = func() time.Time { return now }
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			p.Handle(ctx, "включи Long Mix SoundCloud")
			req := r.all()[0]
			if status != "pending" {
				p.HandleEvent(ctx, EventPrefix+"started", eventPayload(req))
			}
			if status == "paused" {
				p.Handle(ctx, "пауза")
			}
			if status == "stopped" {
				p.Handle(ctx, "стоп")
			}
			now = now.Add(2*time.Hour + 30*time.Minute)
			active := status == "pending" || status == "playing"
			if p.CanDeliver(ctx, req) != active {
				t.Fatal("duration lease / idle TTL", status)
			}
			if active {
				// A 3h stream expires after its remaining finite duration plus grace;
				// checking delivery does not itself renew that duration.
				now = now.Add(36 * time.Minute)
				if p.CanDeliver(ctx, req) {
					t.Fatal("finite playback lease did not expire")
				}
			}
		})
	}
}

func TestProviderLaggingPreviousOffsetDoesNotLeakIntoReplacement(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	p.Handle(ctx, "следующий")
	second := r.all()[1]
	lagging := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 100000})
	p.HandleEvent(lagging, EventPrefix+"stopped", eventPayload(second))
	if text, err := p.Handle(ctx, "продолжи"); text != "" || err != nil {
		t.Fatal(text, err)
	}
	resumed := r.all()[2]
	if resumed.TrackID != second.TrackID || resumed.OffsetMS != 0 {
		t.Fatal("previous stream offset leaked", resumed)
	}
}

func TestProviderOlderMetadataCannotRevokeNewerStartedPlayback(t *testing.T) {
	for _, viaRouting := range []bool{false, true} {
		for _, command := range []string{"что играет", "кто поёт", "лайк", "убери лайк"} {
			t.Run(fmt.Sprint(viaRouting)+"/"+command, func(t *testing.T) {
				var mu sync.Mutex
				accountActions := 0
				p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/likes/") {
						mu.Lock()
						accountActions++
						mu.Unlock()
						w.WriteHeader(http.StatusNoContent)
						return
					}
					if strings.HasSuffix(r.URL.Path, "/streams") {
						fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
						return
					}
					fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist"},{"id":2,"title":"Second","duration":180000,"metadata_artist":"Artist"},{"id":3,"title":"Third","duration":180000,"metadata_artist":"Artist"}]}`)
				})
				r := new(providerRecorder)
				base := providerContext(t, "a", r)
				first := providerStart(t, p, sound.WithRequestSequence(base, 1), r)
				if text, err := p.Handle(sound.WithRequestSequence(base, 2), "следующий"); text != "" || err != nil {
					t.Fatal(text, err)
				}
				second := r.all()[1]
				p.HandleEvent(base, EventPrefix+"started", eventPayload(second))
				old := sound.WithPlaybackState(sound.WithRequestSequence(base, 1), sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Playing"})
				if viaRouting && !p.CanHandle(old, command) {
					t.Fatal("stale metadata escaped local consumption")
				}
				if text, err := p.Handle(old, command); text != "" || !errors.Is(err, context.Canceled) {
					t.Fatal(text, err)
				}
				mu.Lock()
				actions := accountActions
				mu.Unlock()
				if actions != 0 {
					t.Fatal("stale request acted on the newer track's account state")
				}
				if !p.CanDeliver(base, second) || !p.CanHandle(base, "что играет") {
					t.Fatal("stale metadata revoked newer started playback")
				}
				p.HandleEvent(base, EventPrefix+"finished", eventPayload(second))
				if len(r.all()) != 3 || r.all()[2].Title != "Third" || !p.CanDeliver(base, r.all()[2]) {
					t.Fatal("newer lifecycle lost progression", r.all())
				}
				// The watermark belongs only to this speaker.
				other := new(providerRecorder)
				otherCtx := sound.WithRequestSequence(providerContext(t, "b", other), 1)
				providerStart(t, p, otherCtx, other)
				if text, err := p.Handle(otherCtx, "что играет"); err != nil || !strings.Contains(text, "First") {
					t.Fatal("metadata ordering leaked across speakers", text, err)
				}
			})
		}
	}
}

func TestProviderStationStoppedNearEOFAdvancesOnce(t *testing.T) {
	for _, tc := range []struct {
		name, state                                            string
		offset, duration                                       int64
		known, present, foreign, userStop, userPause, wantNext bool
	}{
		{name: "native EOF", state: "Stopped", offset: 140662, known: true, present: true, wantNext: true},
		{name: "canonical fractional duration", state: "Stopped", offset: 140662, duration: 141500, known: true, present: true, wantNext: true},
		{name: "at end", state: "Stopped", offset: 141000, known: true, present: true, wantNext: true},
		{name: "pause near end", state: "Paused", offset: 140662, known: true, present: true},
		{name: "replacement playing", state: "Playing", offset: 140662, known: true, present: true},
		{name: "unknown", state: "Unknown", offset: 140662, known: true, present: true},
		{name: "early stop", state: "Stopped", offset: 10000, known: true, present: true},
		{name: "missing offset", state: "Stopped", offset: 140662, present: true},
		{name: "missing state", state: "Stopped", offset: 140662, known: true},
		{name: "foreign stream", state: "Stopped", offset: 140662, known: true, present: true, foreign: true},
		{name: "impossible offset", state: "Stopped", offset: 150000, known: true, present: true},
		{name: "user pause wins", state: "Stopped", offset: 140662, known: true, present: true, userPause: true},
		{name: "user stop wins", state: "Stopped", offset: 140662, known: true, present: true, userStop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			duration := tc.duration
			if duration == 0 {
				duration = 141000
			}
			p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/streams") {
					fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
					return
				}
				fmt.Fprintf(w, `{"collection":[{"id":1,"title":"First","duration":%d,"metadata_artist":"Artist"},{"id":2,"title":"Second","duration":180000,"metadata_artist":"Artist"}]}`, duration)
			})
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			if text, err := p.Handle(ctx, "включи First SoundCloud"); text != "" || err != nil {
				t.Fatal(text, err)
			}
			first := r.all()[0]
			started := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 0})
			p.HandleEvent(started, EventPrefix+"started", eventPayload(first))
			if tc.userPause {
				p.Handle(ctx, "пауза")
			}
			if tc.userStop {
				p.Handle(ctx, "стоп")
			}
			before := len(r.all())
			stream := first.StreamID
			if tc.foreign {
				stream = "foreign"
			}
			ended := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: tc.present, StreamID: stream, PlayerState: tc.state, OffsetKnown: tc.known, OffsetMS: tc.offset})
			for range 2 {
				if handled, err := p.HandleEvent(ended, EventPrefix+"stopped", eventPayload(first)); !handled || err != nil {
					t.Fatal(handled, err)
				}
			}
			want := before
			if tc.wantNext {
				want++
			}
			if len(r.all()) != want {
				t.Fatal("unexpected auto progression", tc.name, r.all())
			}
			if tc.wantNext {
				next := r.all()[len(r.all())-1]
				if next.Title != "Second" || !p.CanDeliver(ctx, next) {
					t.Fatal(next)
				}
			}
		})
	}
}
