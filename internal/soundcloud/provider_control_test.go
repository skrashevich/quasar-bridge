package soundcloud

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"quasarbridge/internal/sound"
)

func TestProviderStructuredControlsQueueAndResume(t *testing.T) {
	p := providerFixture(t, nil)
	p.SetDefault(false)
	r := new(providerRecorder)
	ctx := providerContext(t, "structured", r)
	first := providerStart(t, p, ctx, r)
	if !p.CanControl(ctx, sound.ControlNext) {
		t.Fatal("confirmed SoundCloud playback did not admit a structured next")
	}
	if text, err := p.HandleControl(ctx, sound.ControlNext); text != "" || err != nil {
		t.Fatal(text, err)
	}
	second := r.all()[1]
	if second.TrackID != "soundcloud:tracks:2" || second.SessionID != first.SessionID || second.PreviousTrackID != first.TrackID {
		t.Fatalf("next selected the wrong queue item: %+v", second)
	}
	if _, err := p.HandleEvent(ctx, EventPrefix+"started", eventPayload(second)); err != nil {
		t.Fatal(err)
	}
	if !p.CanControl(ctx, sound.ControlPrevious) {
		t.Fatal("structured previous was not admitted")
	}
	if text, err := p.HandleControl(ctx, sound.ControlPrevious); text != "" || err != nil {
		t.Fatal(text, err)
	}
	previous := r.all()[2]
	if previous.TrackID != first.TrackID || previous.Title != "First" || previous.NextTrackID != second.TrackID || previous.ArtworkURL == "" {
		t.Fatalf("previous lost queue metadata: %+v", previous)
	}
	paused := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: previous.StreamID, PlayerState: "Paused", OffsetKnown: true, OffsetMS: 13700})
	if _, err := p.HandleEvent(paused, EventPrefix+"stopped", eventPayload(previous)); err != nil {
		t.Fatal(err)
	}
	cleared := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true})
	if !p.CanControl(cleared, sound.ControlResume) {
		t.Fatal("paused ownership was lost")
	}
	if text, err := p.HandleControl(cleared, sound.ControlResume); text != "" || err != nil {
		t.Fatal(text, err)
	}
	resumed := r.all()[3]
	if resumed.TrackID != previous.TrackID || resumed.OffsetMS != 13700 || resumed.SessionID != previous.SessionID || resumed.StreamID == previous.StreamID {
		t.Fatalf("resume did not preserve position and queue identity: %+v", resumed)
	}
}

func TestProviderStructuredControlOwnership(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	if p.CanControl(ctx, sound.ControlNext) {
		t.Fatal("empty provider claimed native controls")
	}
	first := providerStart(t, p, ctx, r)
	other := providerContext(t, "b", new(providerRecorder))
	if p.CanControl(other, sound.ControlNext) {
		t.Fatal("another speaker acquired this queue")
	}
	foreign := sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: "yandex-stream", PlayerState: "Playing"})
	if p.CanControl(foreign, sound.ControlNext) {
		t.Fatal("foreign playback was claimed")
	}
	if text, err := p.HandleControl(ctx, sound.ControlNext); text != "" || err != nil {
		t.Fatal(text, err)
	}
	if _, err := p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first)); err != nil {
		t.Fatal(err)
	}
	if len(r.all()) != 1 {
		t.Fatal("revoked queue replaced another source")
	}
}

func TestProviderInvalidStructuredControlDoesNotObserveOrMutate(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, sound.WithRequestSequence(ctx, 10), r)
	foreign := sound.WithPlaybackState(sound.WithRequestSequence(ctx, 100), sound.PlaybackState{Present: true, StreamID: "foreign", PlayerState: "Playing"})
	for _, control := range []sound.Control{"", "search", "resume SoundCloud", "NEXT"} {
		if p.CanControl(foreign, control) {
			t.Fatalf("invalid control %q was admitted", control)
		}
		if text, err := p.HandleControl(foreign, control); text != "" || err != nil {
			t.Fatal(control, text, err)
		}
	}
	if len(r.all()) != 1 || !p.CanDeliver(ctx, first) || !p.CanControl(ctx, sound.ControlNext) {
		t.Fatal("invalid command changed playback ownership or generation")
	}
	if text, err := p.HandleControl(sound.WithRequestSequence(ctx, 11), sound.ControlNext); text != "" || err != nil {
		t.Fatal("invalid command raised the arrival watermark", text, err)
	}
}

func TestProviderStructuredControlsPreserveArrivalOrdering(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, sound.WithRequestSequence(ctx, 20), r)
	stale := sound.WithPlaybackState(sound.WithRequestSequence(ctx, 10), sound.PlaybackState{Present: true, StreamID: "foreign", PlayerState: "Playing"})
	for _, control := range []sound.Control{sound.ControlNext, sound.ControlPrevious, sound.ControlResume} {
		if !p.CanControl(stale, control) {
			t.Fatal("stale own operation was not consumed locally", control)
		}
		if text, err := p.HandleControl(stale, control); text != "" || !errors.Is(err, context.Canceled) {
			t.Fatal("stale operation was not rejected before observing foreign state", control, text, err)
		}
	}
	if len(r.all()) != 1 || !p.CanDeliver(ctx, first) || !p.CanControl(ctx, sound.ControlNext) {
		t.Fatal("old arrival revoked or replaced current playback")
	}
	if text, err := p.HandleControl(sound.WithRequestSequence(ctx, 21), sound.ControlNext); text != "" || err != nil {
		t.Fatal(text, err)
	}
	second := r.all()[1]
	if p.CanDeliver(ctx, first) || !p.CanDeliver(ctx, second) {
		t.Fatal("new structured operation did not supersede old queued audio")
	}
}

func TestProviderNativeControlsDuringOwnPendingLoad(t *testing.T) {
	for _, observedPrevious := range []bool{false, true} {
		t.Run(fmt.Sprintf("previous_observation=%v", observedPrevious), func(t *testing.T) {
			p := providerFixture(t, nil)
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			first := providerStart(t, p, ctx, r)
			p.HandleControl(ctx, sound.ControlNext)
			pending := r.all()[1]
			state := sound.PlaybackState{Present: true, StreamID: pending.StreamID, PlayerState: "Idle", OffsetKnown: true, OffsetMS: 0}
			if observedPrevious {
				state.StreamID = first.StreamID
				state.PlayerState = "Playing"
				state.OffsetMS = 117000
			}
			loading := sound.WithPlaybackState(ctx, state)
			if p.CanHandle(loading, "предыдущий трек") {
				t.Fatal("text controls unexpectedly acquired pending ownership")
			}
			if p.CanControl(loading, sound.ControlResume) {
				t.Fatal("pending skip admission leaked into resume")
			}
			if !p.CanControl(loading, sound.ControlPrevious) {
				t.Fatal("native previous escaped own pending queue")
			}
			if text, err := p.HandleControl(loading, sound.ControlPrevious); text != "" || err != nil {
				t.Fatal(text, err)
			}
			all := r.all()
			if len(all) != 3 || all[2].TrackID != first.TrackID || all[2].OffsetMS != 0 || all[2].StreamID == first.StreamID {
				t.Fatal("pending previous lost queue index or applied old position", all)
			}
			previous := all[2]
			loading = sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: previous.StreamID, PlayerState: "Idle"})
			if !p.CanControl(loading, sound.ControlNext) {
				t.Fatal("native next escaped own pending queue")
			}
			p.HandleControl(loading, sound.ControlNext)
			all = r.all()
			if len(all) != 4 || all[3].TrackID != pending.TrackID || all[3].OffsetMS != 0 || !p.CanDeliver(ctx, all[3]) || p.CanDeliver(ctx, previous) {
				t.Fatal("pending next failed to supersede previous", all)
			}
		})
	}
}

func TestProviderNativePendingControlRequiresObservedOwnStream(t *testing.T) {
	for _, kind := range []string{"absent", "cleared", "unknown", "speaker"} {
		t.Run(kind, func(t *testing.T) {
			p := providerFixture(t, nil)
			r := new(providerRecorder)
			ctx := providerContext(t, "a", r)
			providerStart(t, p, ctx, r)
			p.HandleControl(ctx, sound.ControlNext)
			pending := r.all()[1]
			state := sound.PlaybackState{}
			if kind != "absent" {
				state.Present = true
			}
			if kind == "unknown" {
				state.StreamID = "yandex"
				state.PlayerState = "Playing"
			}
			if kind == "speaker" {
				ctx = providerContext(t, "b", new(providerRecorder))
				state.StreamID = pending.StreamID
				state.PlayerState = "Idle"
			}
			observed := sound.WithPlaybackState(ctx, state)
			for _, control := range []sound.Control{sound.ControlNext, sound.ControlPrevious} {
				if p.CanControl(observed, control) {
					t.Fatal("unproven pending ownership claimed", kind, control)
				}
				if text, err := p.HandleControl(observed, control); text != "" || err != nil {
					t.Fatal(text, err)
				}
			}
			if len(r.all()) != 2 {
				t.Fatal("unproven pending control changed queue")
			}
		})
	}
}

func TestProviderNativePendingControlPreservesArrivalOrdering(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	providerStart(t, p, sound.WithRequestSequence(ctx, 10), r)
	p.HandleControl(sound.WithRequestSequence(ctx, 20), sound.ControlNext)
	pending := r.all()[1]
	stale := sound.WithPlaybackState(sound.WithRequestSequence(ctx, 15), sound.PlaybackState{Present: true, StreamID: pending.StreamID, PlayerState: "Idle"})
	if !p.CanControl(stale, sound.ControlPrevious) {
		t.Fatal("stale native control was not consumed locally")
	}
	if _, err := p.HandleControl(stale, sound.ControlPrevious); !errors.Is(err, context.Canceled) {
		t.Fatal("stale native control mutated pending queue", err)
	}
	if len(r.all()) != 2 || !p.CanDeliver(ctx, pending) {
		t.Fatal("stale pending control replaced newer request")
	}
}

func TestProviderNativeLifecycleOwnsQueueWithoutLegacyCallbackAlias(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "a", r)
	first := providerStart(t, p, ctx, r)
	if handled, err := p.HandleEvent(ctx, "music_thin_client_on_finished", eventPayload(first)); !handled || err != nil {
		t.Fatal("native lifecycle did not reach provider queue", handled, err)
	}
	all := r.all()
	if len(all) != 2 || all[1].TrackID != "soundcloud:tracks:2" {
		t.Fatal("native lifecycle did not advance queue", all)
	}
	if handled, err := p.HandleEvent(ctx, "quasar_soundcloud_finished", eventPayload(all[1])); handled || err != nil {
		t.Fatal("removed callback namespace is still supported", handled, err)
	}
	if len(r.all()) != 2 {
		t.Fatal("removed callback namespace changed queue")
	}
}
