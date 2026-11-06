package soundcloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

func prepareProviderNext(t *testing.T, p *Provider, ctx context.Context, r *providerRecorder, current sound.Request) sound.Request {
	t.Helper()
	before := len(r.all())
	if handled, err := p.Prefetch(ctx, current.SessionID, current.StreamID, current.CallbackToken); !handled || err != nil {
		t.Fatal(handled, err)
	}
	all := r.all()
	if len(all) != before+1 {
		t.Fatalf("prefetch did not queue exactly one deferred item: %d -> %d", before, len(all))
	}
	return all[len(all)-1]
}

func TestProviderPrefetchDoesNotAdvanceCurrent(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	next := prepareProviderNext(t, p, ctx, r, first)
	if !next.Prefetch || next.AfterStreamID != first.StreamID || next.TrackID != "soundcloud:tracks:2" || next.PreviousTrackID != first.TrackID || next.Title != "Second" || next.Subtitle != "Artist" || next.DurationMS != 180000 || next.SessionID != first.SessionID || next.Generation != first.Generation || next.URL == "" || next.StreamID == first.StreamID || next.CallbackToken == first.CallbackToken {
		t.Fatalf("bad prepared item: %+v", next)
	}
	if text, err := p.Handle(ctx, "что играет"); err != nil || !strings.Contains(text, "First") {
		t.Fatal("prefetch changed current", text, err)
	}
	if !p.CanDeliver(ctx, first) || !p.CanDeliver(ctx, next) {
		t.Fatal("prefetch invalidated current delivery or cannot be delivered")
	}
	repeated := prepareProviderNext(t, p, ctx, r, first)
	if repeated != next {
		t.Fatal("repeated warmup changed prepared identity")
	}
	other := providerContext(t, "b", new(providerRecorder))
	if p.CanDeliver(other, next) {
		t.Fatal("prefetch leaked across speakers")
	}
}

func TestProviderDeliveredPrefetchStartsOnlyAfterCurrentEOF(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	next := prepareProviderNext(t, p, ctx, r, first)
	p.PrefetchDelivered(ctx, next)
	eof := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Stopped", OffsetKnown: true, OffsetMS: 120000})
	p.HandleEvent(eof, EventPrefix+"stopped", eventPayload(first))
	p.HandleEvent(eof, EventPrefix+"finished", eventPayload(first))
	if len(r.all()) != 2 {
		t.Fatal("EOF pushed an immediate replacement despite staged next")
	}
	if _, err := p.HandleEvent(ctx, EventPrefix+"started", eventPayload(next)); err != nil {
		t.Fatal(err)
	}
	if text, err := p.Handle(ctx, "что играет"); err != nil || !strings.Contains(text, "Second") {
		t.Fatal("staged item did not become current", text, err)
	}
	if p.CanDeliver(ctx, first) || p.CanDeliver(ctx, next) {
		t.Fatal("a staged directive remained deliverable after promotion")
	}
	if !p.CanControl(ctx, sound.ControlPrevious) {
		t.Fatal("promoted item lost playback ownership")
	}
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(next))
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first))
	if len(r.all()) != 2 {
		t.Fatal("duplicate or old lifecycle advanced queue")
	}
}

func TestProviderPrefetchPlaybackObservationPromotesBeforeForeignCheck(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	next := prepareProviderNext(t, p, ctx, r, first)
	p.PrefetchDelivered(ctx, next)
	playing := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: next.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 2300})
	if !p.CanControl(playing, sound.ControlPrevious) {
		t.Fatal("own staged next was mistaken for foreign playback")
	}
	text, err := p.Handle(playing, "что играет")
	if err != nil || !strings.Contains(text, "Second") {
		t.Fatal(text, err)
	}
	if _, err = p.HandleControl(playing, sound.ControlPrevious); err != nil {
		t.Fatal(err)
	}
	if got := r.all()[2]; got.TrackID != first.TrackID || got.Prefetch {
		t.Fatal("previous did not act on promoted queue index", got)
	}
}

func TestProviderUndeliveredPrefetchFallsBackAtEOF(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	next := prepareProviderNext(t, p, ctx, r, first)
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first))
	all := r.all()
	if len(all) != 3 || all[2].Prefetch || all[2].TrackID != next.TrackID || all[2].StreamID == next.StreamID || p.CanDeliver(ctx, next) {
		t.Fatal("lost warmup response suppressed EOF fallback", all)
	}
	p.PrefetchDelivered(ctx, next)
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(next))
	if !p.CanDeliver(ctx, all[2]) {
		t.Fatal("late staged callback replaced current fallback")
	}
}

func TestProviderPrefetchInvalidationAndAdmission(t *testing.T) {
	for _, kind := range []string{"next", "resume", "mode", "foreign", "token", "speaker"} {
		t.Run(kind, func(t *testing.T) {
			p := providerFixture(t, nil)
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			first := providerStart(t, p, ctx, r)
			next := prepareProviderNext(t, p, ctx, r, first)
			switch kind {
			case "next":
				p.HandleControl(ctx, sound.ControlNext)
			case "resume":
				p.HandleControl(ctx, sound.ControlResume)
			case "mode":
				p.Handle(sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 2000}), "повторяй песню")
			case "foreign":
				p.CanControl(sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: "yandex", PlayerState: "Playing"}), sound.ControlNext)
			case "token":
				bad := next
				bad.CallbackToken = "wrong"
				p.PrefetchDelivered(ctx, bad)
				p.HandleEvent(ctx, EventPrefix+"started", eventPayload(bad))
				if !p.CanDeliver(ctx, next) {
					t.Fatal("bad token invalidated valid prepared item")
				}
				return
			case "speaker":
				other := providerContext(t, "b", new(providerRecorder))
				p.PrefetchDelivered(other, next)
				p.HandleEvent(other, EventPrefix+"started", eventPayload(next))
				if !p.CanDeliver(ctx, next) {
					t.Fatal("other speaker altered prepared item")
				}
				return
			}
			if p.CanDeliver(ctx, next) {
				t.Fatal("superseded prefetch remained deliverable")
			}
			before := len(r.all())
			p.HandleEvent(ctx, EventPrefix+"started", eventPayload(next))
			p.PrefetchDelivered(ctx, next)
			if len(r.all()) != before {
				t.Fatal("stale prefetch changed queue")
			}
		})
	}
}

func TestProviderPrefetchRejectsStaleIdentityAndNoNext(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	for _, bad := range []sound.Request{{SessionID: "wrong", StreamID: first.StreamID, CallbackToken: first.CallbackToken}, {SessionID: first.SessionID, StreamID: "wrong", CallbackToken: first.CallbackToken}, {SessionID: first.SessionID, StreamID: first.StreamID, CallbackToken: "wrong"}} {
		if _, err := p.Prefetch(ctx, bad.SessionID, bad.StreamID, bad.CallbackToken); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.all()) != 1 {
		t.Fatal("invalid identity prefetched")
	}
	p.HandleControl(ctx, sound.ControlNext)
	second := r.all()[1]
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(second))
	if _, err := p.Prefetch(ctx, second.SessionID, second.StreamID, second.CallbackToken); err != nil {
		t.Fatal(err)
	}
	if len(r.all()) != 2 {
		t.Fatal("queue end prefetched extra item")
	}
}

func TestProviderPrefetchSupersededDuringHTTP(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			if strings.Contains(r.URL.Path, "tracks:2/") {
				select {
				case <-entered:
				default:
					close(entered)
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000},{"id":2,"title":"Second","duration":180000}]}`)
	})
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	done := make(chan error, 1)
	go func() { _, err := p.Prefetch(ctx, first.SessionID, first.StreamID, first.CallbackToken); done <- err }()
	<-entered
	if _, err := p.Handle(ctx, "стоп"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(r.all()) != 2 || r.all()[1].Command != "stop" {
		t.Fatal("HTTP prefetch committed after stop", r.all())
	}
}

func TestProviderPrefetchRepeatCycleAndMetadata(t *testing.T) {
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist","artwork_url":"https://i1.sndcdn.com/first.jpg"},{"id":2,"title":"Second","duration":180000,"metadata_artist":"Other Artist","artwork_url":"https://i1.sndcdn.com/second.jpg"}]}`)
	})
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	p.Handle(ctx, "повторяй всё")
	next := prepareProviderNext(t, p, ctx, r, first)
	if next.ArtworkURL == "" || next.ArtworkURL == first.ArtworkURL || next.Title != "Second" || next.Subtitle != "Other Artist" || next.NextTrackID != first.TrackID || next.RepeatMode != "all" {
		t.Fatal("next metadata does not describe prepared item", next)
	}
	p.PrefetchDelivered(ctx, next)
	oldObservation := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Playing"})
	p.HandleEvent(oldObservation, EventPrefix+"started", eventPayload(next))
	if !p.CanControl(ctx, sound.ControlNext) {
		t.Fatal("old playback observation revoked the new started item")
	}
	cycle := prepareProviderNext(t, p, ctx, r, next)
	if cycle.TrackID != first.TrackID || cycle.AfterStreamID != next.StreamID || cycle.PreviousTrackID != next.TrackID || cycle.SessionID != next.SessionID || cycle.StreamID == first.StreamID {
		t.Fatal("next prefetch cycle lost queue position", cycle)
	}
}

func TestProviderPrefetchFailureDoesNotInterruptCurrent(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	next := prepareProviderNext(t, p, ctx, r, first)
	p.PrefetchDelivered(ctx, next)
	current := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 4000})
	p.HandleEvent(current, EventPrefix+"failed", eventPayload(next))
	if text, err := p.Handle(current, "что играет"); err != nil || !strings.Contains(text, "First") {
		t.Fatal("prepared failure interrupted current item", text, err)
	}
	if p.CanDeliver(ctx, next) {
		t.Fatal("failed prepared item remained deliverable")
	}
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first))
	if len(r.all()) != 2 {
		t.Fatal("failed next track was retried instead of skipped")
	}
}

func TestProviderPrefetchCallbackProvesDeliveryBeforeHook(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	next := prepareProviderNext(t, p, ctx, r, first)
	// The device can return started before the socket write completion hook runs.
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(next))
	p.PrefetchDelivered(ctx, next)
	if text, err := p.Handle(ctx, "что играет"); err != nil || !strings.Contains(text, "Second") {
		t.Fatal("racing callback was lost", text, err)
	}
	if p.CanDeliver(ctx, next) {
		t.Fatal("completion hook recreated a promoted prefetch")
	}
}

func TestProviderPrefetchForeignObservationAtDelivery(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	next := prepareProviderNext(t, p, ctx, r, first)
	foreign := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: "another-source", PlayerState: "Playing"})
	if p.CanDeliver(foreign, next) {
		t.Fatal("prepared audio overwrote an observed foreign source")
	}
	if p.CanDeliver(ctx, first) || p.CanDeliver(ctx, next) {
		t.Fatal("foreign delivery check did not revoke old queue")
	}
}

func TestProviderDeliveredPrefetchModeRefreshesCurrentAtObservedOffset(t *testing.T) {
	for _, command := range []string{"повторяй песню", "повторяй всё", "перемешай"} {
		t.Run(command, func(t *testing.T) {
			p := providerFixture(t, nil)
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			first := providerStart(t, p, ctx, r)
			next := prepareProviderNext(t, p, ctx, r, first)
			p.PrefetchDelivered(ctx, next)
			observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 37000})
			if _, err := p.Handle(observed, command); err != nil {
				t.Fatal(err)
			}
			all := r.all()
			if len(all) != 3 {
				t.Fatal("mode change did not refresh staged stack", len(all))
			}
			fresh := all[2]
			if fresh.TrackID != first.TrackID || fresh.OffsetMS != 37000 || fresh.URL != first.URL || fresh.StreamID == first.StreamID || fresh.Prefetch || !p.CanDeliver(ctx, fresh) || p.CanDeliver(ctx, next) {
				t.Fatal("refresh lost track/position/ownership", fresh)
			}
			if command == "повторяй песню" && fresh.RepeatMode != "one" || command == "повторяй всё" && fresh.RepeatMode != "all" || command == "перемешай" && !fresh.Shuffle {
				t.Fatal("refresh lost playback mode", fresh)
			}
			p.HandleEvent(ctx, EventPrefix+"started", eventPayload(next))
			p.PrefetchDelivered(ctx, next)
			if !p.CanDeliver(ctx, fresh) {
				t.Fatal("stale staged start replaced refreshed current")
			}
		})
	}
}

func TestProviderDeliveredPrefetchModeUnknownPositionPreservesStagedQueue(t *testing.T) {
	for _, state := range []sound.PlaybackState{{}, {Present: true, StreamID: "foreign", PlayerState: "Playing", OffsetKnown: true, OffsetMS: 2000}, {Present: true, PlayerState: "Playing"}, {Present: true, PlayerState: "Paused", OffsetKnown: true, OffsetMS: 2000}} {
		p := providerFixture(t, nil)
		r := new(providerRecorder)
		ctx := providerContext(t, "a", r)
		first := providerStart(t, p, ctx, r)
		next := prepareProviderNext(t, p, ctx, r, first)
		p.PrefetchDelivered(ctx, next)
		if state.Present && state.StreamID == "" {
			state.StreamID = first.StreamID
		}
		observed := sound.WithPlaybackState(ctx, state)
		p.Handle(observed, "повторяй песню")
		if state.StreamID == "foreign" {
			continue
		}
		if len(r.all()) != 2 || !p.CanDeliver(ctx, next) {
			t.Fatal("mode with unavailable live position invalidated staged queue", state)
		}
		p.HandleEvent(ctx, EventPrefix+"started", eventPayload(next))
		if text, err := p.Handle(ctx, "что играет"); err != nil || !strings.Contains(text, "Second") {
			t.Fatal("preserved staged item could not promote", text, err)
		}
	}
}

func TestProviderDeliveredPrefetchModeEnqueueFailureRollsBack(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	next := prepareProviderNext(t, p, ctx, r, first)
	p.PrefetchDelivered(ctx, next)
	r.mu.Lock()
	r.reject = true
	r.mu.Unlock()
	observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 21000})
	p.Handle(observed, "повторяй песню")
	if !p.CanDeliver(ctx, next) || !p.CanDeliver(ctx, first) {
		t.Fatal("failed refresh orphaned already staged native audio")
	}
	r.mu.Lock()
	r.reject = false
	r.mu.Unlock()
	p.HandleEvent(ctx, EventPrefix+"started", eventPayload(next))
	if !p.CanControl(ctx, sound.ControlPrevious) {
		t.Fatal("failed mode refresh lost playback ownership")
	}
}

func TestProviderWaitingPrefetchFailureAdvancesPastFailedTrack(t *testing.T) {
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000},{"id":2,"title":"Second","duration":180000},{"id":3,"title":"Third","duration":180000}]}`)
	})
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	next := prepareProviderNext(t, p, ctx, r, first)
	p.PrefetchDelivered(ctx, next)
	eof := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Stopped", OffsetKnown: true, OffsetMS: 120000})
	p.HandleEvent(eof, EventPrefix+"stopped", eventPayload(first))
	p.HandleEvent(eof, EventPrefix+"failed", eventPayload(next))
	all := r.all()
	if len(all) != 3 || all[2].TrackID != "soundcloud:tracks:3" || all[2].Prefetch {
		t.Fatal("waiting queue did not skip failed staged item", all)
	}
	p.HandleEvent(ctx, EventPrefix+"failed", eventPayload(next))
	if len(r.all()) != 3 {
		t.Fatal("duplicate staged failure advanced twice")
	}
}

func TestProviderSuccessfulPrefetchDeliverySurvivesRequestCancellation(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	next := prepareProviderNext(t, p, ctx, r, first)
	delivered, cancel := context.WithCancel(ctx)
	cancel()
	// Cancellation after a successful write must not erase the delivery fact.
	p.PrefetchDelivered(delivered, next)
	p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first))
	if len(r.all()) != 2 {
		t.Fatal("post-write cancellation lost staged continuation")
	}
}

func TestProviderRetiredStagedStreamDoesNotRevokePendingReplacement(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			if strings.Contains(r.URL.Path, "tracks:1/") && block.Load() {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000},{"id":2,"title":"Second","duration":180000}]}`)
	})
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	staged := prepareProviderNext(t, p, ctx, r, first)
	p.PrefetchDelivered(ctx, staged)
	block.Store(true)
	done := make(chan error, 1)
	go func() { _, err := p.HandleControl(ctx, sound.ControlResume); done <- err }()
	<-entered
	retired := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: staged.StreamID, PlayerState: "Playing"})
	p.CanControl(retired, sound.ControlNext)
	p.HandleEvent(retired, EventPrefix+"started", eventPayload(staged))
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	all := r.all()
	if len(all) != 3 || all[2].TrackID != first.TrackID || !p.CanDeliver(ctx, all[2]) {
		t.Fatal("retired staged identity revoked replacement", all)
	}
	p.CanControl(retired, sound.ControlNext)
	if !p.CanDeliver(ctx, all[2]) {
		t.Fatal("retired observation after enqueue revoked replacement")
	}
	current := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: all[2].StreamID, PlayerState: "Playing"})
	p.HandleEvent(current, EventPrefix+"started", eventPayload(all[2]))
	p.CanControl(sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: "unknown-foreign", PlayerState: "Playing"}), sound.ControlNext)
	if p.CanDeliver(ctx, all[2]) {
		t.Fatal("retirement guard admitted unrelated foreign stream")
	}
}

func TestProviderPreparedPrefetchModeUsesRefreshBeforeDeliveryHook(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	next := prepareProviderNext(t, p, ctx, r, first)
	// CanDeliver may have passed and the response write may still be blocked.
	if !p.CanDeliver(ctx, next) {
		t.Fatal("prepared item cannot reach delivery boundary")
	}
	p.Handle(ctx, "повторяй песню")
	if !p.CanDeliver(ctx, next) || len(r.all()) != 2 {
		t.Fatal("unknown-position mode orphaned an in-flight warm-up")
	}
	observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 49000})
	p.Handle(observed, "повторяй песню")
	all := r.all()
	if len(all) != 3 || all[2].TrackID != first.TrackID || all[2].OffsetMS != 49000 || all[2].RepeatMode != "one" || p.CanDeliver(ctx, next) {
		t.Fatal("mode did not replace potentially staged item", all)
	}
	retired := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: next.StreamID, PlayerState: "Playing"})
	p.CanControl(retired, sound.ControlNext)
	if !p.CanDeliver(ctx, all[2]) {
		t.Fatal("in-flight staged identity revoked refreshed stream")
	}
}

func TestProviderPrefetchObservedNextCanContinueWithoutStartedCallback(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	p.Handle(ctx, "повторяй всё")
	next := prepareProviderNext(t, p, ctx, r, first)
	p.PrefetchDelivered(ctx, next)
	observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: next.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 31000})
	after := prepareProviderNext(t, p, observed, r, next)
	if after.TrackID != first.TrackID || after.AfterStreamID != next.StreamID {
		t.Fatal("lost started callback stalled natural queue", after)
	}
}

func TestProviderOldLifecycleObservationPromotesPreparedWithoutApplyingOldAction(t *testing.T) {
	for _, action := range []string{"stopped", "finished"} {
		for _, waiting := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/waiting=%v", action, waiting), func(t *testing.T) {
				p := providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/streams") {
						fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
						return
					}
					fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000},{"id":2,"title":"Second","duration":180000},{"id":3,"title":"Third","duration":180000}]}`)
				})
				r := new(providerRecorder)
				ctx := providerContext(t, "a", r)
				first := providerStart(t, p, ctx, r)
				next := prepareProviderNext(t, p, ctx, r, first)
				p.PrefetchDelivered(ctx, next)
				if waiting {
					p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first))
				}
				observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: next.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 7000})
				p.HandleEvent(observed, EventPrefix+action, eventPayload(first))
				if len(r.all()) != 2 {
					t.Fatal("old lifecycle advanced past observed prepared item", r.all())
				}
				// Inspect independently of the observation: the old callback itself must
				// already have promoted B, including the waiting/terminal A case.
				p.mu.Lock()
				s := p.sessions[dialogue.SpeakerKey(ctx)]
				promoted := s != nil && s.stream == next.StreamID && s.token == next.CallbackToken && !s.terminal && s.status != "paused"
				p.mu.Unlock()
				if !promoted {
					t.Fatal("old lifecycle paused or failed to promote observed prepared item")
				}
				p.HandleEvent(observed, EventPrefix+"started", eventPayload(next))
				if text, err := p.Handle(observed, "что играет"); err != nil || !strings.Contains(text, "Second") {
					t.Fatal("valid next lifecycle lost ownership", text, err)
				}
				if !p.CanControl(observed, sound.ControlPrevious) {
					t.Fatal("valid next stream no longer accepts controls")
				}
				p.HandleEvent(observed, EventPrefix+"finished", eventPayload(next))
				all := r.all()
				if len(all) != 3 || all[2].TrackID != "soundcloud:tracks:3" {
					t.Fatal("valid next terminal callback lost progression", all)
				}
			})
		}
	}
}

func longPrefetchFixture(t *testing.T, nextStreams *atomic.Int64) *Provider {
	t.Helper()
	return providerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			if strings.Contains(r.URL.Path, "tracks:2/") {
				nextStreams.Add(1)
			}
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":10800000},{"id":2,"title":"Second","duration":180000}]}`)
	})
}

func TestProviderLongCurrentDefersPrefetchUntilNativeEOF(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(fmt.Sprintf("observed=%v", observed), func(t *testing.T) {
			streams := new(atomic.Int64)
			p := longPrefetchFixture(t, streams)
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			first := providerStart(t, p, ctx, r)
			warm := ctx
			if observed {
				warm = sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 30000})
			}
			if handled, err := p.Prefetch(warm, first.SessionID, first.StreamID, first.CallbackToken); !handled || err != nil {
				t.Fatal(handled, err)
			}
			if len(r.all()) != 1 || streams.Load() != 0 {
				t.Fatal("long current prepared a relay URL that can expire before playback", len(r.all()), streams.Load())
			}
			eof := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Stopped", OffsetKnown: true, OffsetMS: 10800000})
			p.HandleEvent(eof, "music_thin_client_on_stopped", eventPayload(first))
			all := r.all()
			if len(all) != 2 || all[1].TrackID != "soundcloud:tracks:2" || all[1].Prefetch || streams.Load() != 1 {
				t.Fatal("deferred warmup stalled native EOF progression", all, streams.Load())
			}
		})
	}
}

func TestProviderLongCurrentNearEndAllowsPrefetch(t *testing.T) {
	for _, source := range []string{"observation", "session"} {
		t.Run(source, func(t *testing.T) {
			streams := new(atomic.Int64)
			p := longPrefetchFixture(t, streams)
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			first := providerStart(t, p, ctx, r)
			observed := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: first.StreamID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 10770000})
			warm := observed
			if source == "session" {
				p.HandleControl(observed, sound.ControlResume)
				all := r.all()
				first = all[len(all)-1]
				p.HandleEvent(ctx, "music_thin_client_on_started", eventPayload(first))
				warm = ctx
			}
			next := prepareProviderNext(t, p, warm, r, first)
			if next.TrackID != "soundcloud:tracks:2" || streams.Load() != 1 {
				t.Fatal("near-end long track did not allow bounded prefetch", next, streams.Load())
			}
		})
	}
}
