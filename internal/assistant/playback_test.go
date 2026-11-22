package assistant

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/skrashevich/quasar-bridge/internal/dialogue"
	"github.com/skrashevich/quasar-bridge/internal/sound"
)

func playbackContext(t *testing.T, speaker string, state sound.PlaybackState, capture func(sound.Request) error) context.Context {
	t.Helper()
	ctx := dialogue.WithSpeaker(t.Context(), speaker)
	ctx = sound.WithPlaybackState(ctx, state)
	return sound.WithPlayer(ctx, func(_ context.Context, req sound.Request) error { return capture(req) })
}

func TestMusicPlaybackBindsOnlyReportedStreamAndSpeaker(t *testing.T) {
	player := NewMusicPlayback(nil)
	var requests []sound.Request
	capture := func(req sound.Request) error { requests = append(requests, req); return nil }
	ctx := playbackContext(t, "office", sound.PlaybackState{Present: true}, capture)
	if err := player.Start(ctx, []WebLink{{URL: "https://audio.example/live.m3u8", Title: "Live"}}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || len(requests[0].StreamID) != 35 || requests[0].Title != "Live" {
		t.Fatalf("missing explicit playback identity: %+v", requests)
	}
	id := requests[0].StreamID
	if player.Active(ctx) {
		t.Fatal("queued playback became active without a speaker report")
	}
	for _, tc := range []struct {
		speaker string
		state   sound.PlaybackState
		want    bool
	}{
		{"bedroom", sound.PlaybackState{Present: true, StreamID: id}, false},
		{"office", sound.PlaybackState{StreamID: id}, false},
		{"office", sound.PlaybackState{Present: true, StreamID: "foreign"}, false},
		{"office", sound.PlaybackState{Present: true, StreamID: id}, true},
		{"office", sound.PlaybackState{Present: true, StreamID: id}, true}, // A new request/session keeps the speaker identity.
	} {
		if got := player.Active(playbackContext(t, tc.speaker, tc.state, capture)); got != tc.want {
			t.Errorf("speaker=%s state=%+v active=%v", tc.speaker, tc.state, got)
		}
	}
	if player.Active(sound.WithPlaybackState(t.Context(), sound.PlaybackState{Present: true, StreamID: id})) {
		t.Fatal("bound playback without speaker identity")
	}
}

func TestMusicPlaybackPauseResumeKeepsReportedOffset(t *testing.T) {
	player := NewMusicPlayback(nil)
	var requests []sound.Request
	capture := func(req sound.Request) error { requests = append(requests, req); return nil }
	ctx := playbackContext(t, "office", sound.PlaybackState{Present: true}, capture)
	if err := player.Start(ctx, []WebLink{{URL: "https://audio.example/track.ogg", Title: "Track"}}); err != nil {
		t.Fatal(err)
	}
	id := requests[0].StreamID
	ctx = playbackContext(t, "office", sound.PlaybackState{Present: true, StreamID: id, OffsetMS: 12345, OffsetKnown: true}, capture)
	if _, err := player.Control(ctx, "pause"); err != nil {
		t.Fatal(err)
	}
	if requests[1].Command != "pause" {
		t.Fatalf("pause=%+v", requests[1])
	}
	paused := playbackContext(t, "office", sound.PlaybackState{Present: true, OffsetMS: -1}, capture)
	if !player.Active(paused) {
		t.Fatal("own pause lost binding after firmware cleared currentlyPlayingID")
	}
	if _, err := player.Control(paused, "resume"); err != nil {
		t.Fatal(err)
	}
	resume := requests[2]
	if resume.URL != "https://audio.example/track.ogg" || resume.OffsetMS != 12345 || resume.StreamID == id {
		t.Fatalf("resume did not use saved speaker offset: %+v", resume)
	}
	if player.Active(playbackContext(t, "office", sound.PlaybackState{Present: true, StreamID: "somebody-elses-music"}, capture)) {
		t.Fatal("paused binding claimed foreign music")
	}
}

func TestMusicPlaybackRejectedAndCanceledCommandsKeepPreviousBinding(t *testing.T) {
	player := NewMusicPlayback(nil)
	var requests []sound.Request
	reject := false
	capture := func(req sound.Request) error {
		if reject {
			return context.Canceled
		}
		requests = append(requests, req)
		return nil
	}
	ctx := playbackContext(t, "office", sound.PlaybackState{Present: true}, capture)
	tracks := []WebLink{{URL: "https://audio.example/track.ogg"}}
	if err := player.Start(ctx, tracks); err != nil {
		t.Fatal(err)
	}
	id := requests[0].StreamID
	ctx = playbackContext(t, "office", sound.PlaybackState{Present: true, StreamID: id, OffsetMS: 5000, OffsetKnown: true}, capture)
	if !player.Active(ctx) {
		t.Fatal("first stream not bound")
	}
	reject = true
	if err := player.Start(ctx, tracks); !errors.Is(err, context.Canceled) {
		t.Fatalf("start error=%v", err)
	}
	if _, err := player.Control(ctx, "pause"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pause error=%v", err)
	}
	reject = false
	if !player.Active(ctx) {
		t.Fatal("rejected update replaced active playback")
	}
	if player.Active(playbackContext(t, "office", sound.PlaybackState{Present: true}, capture)) {
		t.Fatal("rejected pause changed paused state")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := player.Start(canceled, tracks); !errors.Is(err, context.Canceled) || len(requests) != 1 {
		t.Fatalf("canceled start mutated playback: error=%v requests=%d", err, len(requests))
	}
	if err := player.Start(sound.WithPlayer(t.Context(), func(context.Context, sound.Request) error { t.Fatal("played without speaker identity"); return nil }), tracks); err == nil {
		t.Fatal("accepted missing speaker identity")
	}
}

func TestMusicPlaybackNextPreviousUseReportedOffset(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	frames := map[string][]byte{
		"/first.mp3":  bytes.Repeat(musicFrame(9, 1, 0x11), 100),
		"/second.mp3": bytes.Repeat(musicFrame(9, 1, 0x22), 100),
		"/third.mp3":  bytes.Repeat(musicFrame(9, 1, 0x33), 100),
	}
	reader.http = &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(frames[req.URL.Path]))}, nil
	})}
	queue, err := NewMusicQueue("http://127.0.0.1:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	player := NewMusicPlayback(queue)
	var requests []sound.Request
	capture := func(req sound.Request) error { requests = append(requests, req); return nil }
	ctx := playbackContext(t, "office", sound.PlaybackState{Present: true}, capture)
	tracks := []WebLink{{URL: "https://audio.example/first.mp3", Title: "First"}, {URL: "https://audio.example/second.mp3", Title: "Second"}, {URL: "https://audio.example/third.mp3", Title: "Third"}}
	if err := player.Start(ctx, tracks); err != nil {
		t.Fatal(err)
	}
	stream := requests[0]
	response := queueRequest(t, queue, http.MethodGet, stream.URL) // Full download must not imply that the third track is playing.
	if response.Code != http.StatusOK {
		t.Fatalf("queue status=%d", response.Code)
	}
	ctx = playbackContext(t, "office", sound.PlaybackState{Present: true, StreamID: stream.StreamID, OffsetMS: 2500, OffsetKnown: true}, capture) // Second track, 100 ms in.
	if _, err := player.Control(ctx, "next"); err != nil {
		t.Fatal(err)
	}
	next := requests[1]
	if !strings.Contains(next.URL, "start=2") || next.Title != "Third" || next.StreamID == stream.StreamID {
		t.Fatalf("next=%+v", next)
	}
	if !player.Active(ctx) {
		t.Fatal("pending next hid still-reported previous stream")
	}
	ctx = playbackContext(t, "office", sound.PlaybackState{Present: true, StreamID: next.StreamID, OffsetMS: 0, OffsetKnown: true}, capture)
	if _, err := player.Control(ctx, "previous"); err != nil {
		t.Fatal(err)
	}
	previous := requests[2]
	if !strings.Contains(previous.URL, "start=1") || previous.Title != "Second" {
		t.Fatalf("previous=%+v", previous)
	}
}

func TestMusicPlaybackPrunesExpiredAndBoundsSpeakers(t *testing.T) {
	player := NewMusicPlayback(nil)
	now := time.Now()
	player.now = func() time.Time { return now }
	var last sound.Request
	capture := func(req sound.Request) error { last = req; return nil }
	for i := range 129 {
		ctx := playbackContext(t, string(rune('a'+i)), sound.PlaybackState{Present: true}, capture)
		if err := player.Start(ctx, []WebLink{{URL: "https://audio.example/live.m3u8"}}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	if len(player.entries) != 128 {
		t.Fatalf("speaker book size=%d", len(player.entries))
	}
	now = now.Add(2 * time.Hour)
	ctx := playbackContext(t, string(rune('a'+128)), sound.PlaybackState{Present: true, StreamID: last.StreamID}, capture)
	if player.Active(ctx) {
		t.Fatal("expired playback remained active")
	}
}

func TestMusicPlaybackSingleMP3ResumesThroughTrimmedQueue(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.http = &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(bytes.Repeat(musicFrame(9, 1, 0x11), 200)))}, nil
	})}
	queue, err := NewMusicQueue("http://127.0.0.1:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	player := NewMusicPlayback(queue)
	var requests []sound.Request
	capture := func(req sound.Request) error { requests = append(requests, req); return nil }
	ctx := playbackContext(t, "office", sound.PlaybackState{Present: true}, capture)
	if err := player.Start(ctx, []WebLink{{URL: "https://audio.example/one.mp3", Title: "One"}}); err != nil {
		t.Fatal(err)
	}
	first := requests[0]
	if !strings.Contains(first.URL, "/music-stream/") {
		t.Fatal("single MP3 bypassed tracked queue")
	}
	if response := queueRequest(t, queue, http.MethodGet, first.URL); response.Code != http.StatusOK {
		t.Fatalf("queue status=%d", response.Code)
	}
	ctx = playbackContext(t, "office", sound.PlaybackState{Present: true, StreamID: first.StreamID, OffsetMS: 1005, OffsetKnown: true}, capture)
	if _, err := player.Control(ctx, "stop"); err != nil || requests[1].Command != "stop" {
		t.Fatalf("stop=%+v error=%v", requests, err)
	}
	ctx = playbackContext(t, "office", sound.PlaybackState{Present: true, OffsetMS: 0}, capture)
	if _, err := player.Control(ctx, "resume"); err != nil {
		t.Fatal(err)
	}
	resumed := requests[2]
	u, err := url.Parse(resumed.URL)
	if err != nil || u.Query().Get("offset_ms") != "1005" || resumed.OffsetMS != 0 || resumed.Title != "One" {
		t.Fatalf("MP3 resume must trim on server and start native offset at zero: %+v error=%v", resumed, err)
	}
}

func TestMusicPlaybackUnknownPositionAndQueueBoundariesDoNotQueue(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.http = &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(bytes.Repeat(musicFrame(9, 1, 0x11), 100)))}, nil
	})}
	queue, err := NewMusicQueue("http://127.0.0.1:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	player := NewMusicPlayback(queue)
	var requests []sound.Request
	capture := func(req sound.Request) error { requests = append(requests, req); return nil }
	ctx := playbackContext(t, "office", sound.PlaybackState{Present: true}, capture)
	if err := player.Start(ctx, []WebLink{{URL: "https://audio.example/one.mp3"}}); err != nil {
		t.Fatal(err)
	}
	first := requests[0]
	queueRequest(t, queue, http.MethodGet, first.URL)
	for _, tc := range []struct {
		action string
		offset int64
		want   string
	}{
		{"previous", 0, "первый"},
		{"next", 0, "последний"},
		{"next", -1, "позицию"},
		{"resume", -1, "позицию"},
	} {
		ctx = playbackContext(t, "office", sound.PlaybackState{Present: true, StreamID: first.StreamID, OffsetMS: tc.offset, OffsetKnown: tc.offset >= 0}, capture)
		message, err := player.Control(ctx, tc.action)
		if err != nil || !strings.Contains(message, tc.want) || len(requests) != 1 {
			t.Errorf("action=%s offset=%d message=%q error=%v requests=%d", tc.action, tc.offset, message, err, len(requests))
		}
	}
}

func TestMusicPlaybackMissingOffsetDoesNotBecomeZero(t *testing.T) {
	player := NewMusicPlayback(nil)
	var requests []sound.Request
	capture := func(req sound.Request) error { requests = append(requests, req); return nil }
	ctx := playbackContext(t, "office", sound.PlaybackState{Present: true}, capture)
	if err := player.Start(ctx, []WebLink{{URL: "https://audio.example/track.ogg"}}); err != nil {
		t.Fatal(err)
	}
	id := requests[0].StreamID
	known := playbackContext(t, "office", sound.PlaybackState{Present: true, StreamID: id, OffsetMS: 12345, OffsetKnown: true}, capture)
	if !player.Active(known) {
		t.Fatal("known playback not bound")
	}
	missing := playbackContext(t, "office", sound.PlaybackState{Present: true, StreamID: id}, capture)
	for _, action := range []string{"next", "previous", "restart", "resume"} {
		message, err := player.Control(missing, action)
		if err != nil || !strings.Contains(message, "позицию") || len(requests) != 1 {
			t.Fatalf("missing offset allowed %s: message=%q error=%v requests=%d", action, message, err, len(requests))
		}
	}
	if _, err := player.Control(missing, "pause"); err != nil {
		t.Fatal(err)
	}
	paused := playbackContext(t, "office", sound.PlaybackState{Present: true}, capture)
	if _, err := player.Control(paused, "resume"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 || requests[2].OffsetMS != 12345 {
		t.Fatalf("pause replaced previous reported snapshot with absent offset: %+v", requests)
	}
}

func TestMusicPlaybackReportedStateReconcilesDiscardedPause(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  string
		offset int64
		known  bool
	}{
		{"playing", "Playing", 9876, true},
		{"buffering", "Buffering", 5678, true},
		{"paused-at-zero", "Paused", 0, true},
		{"playing-without-offset", "Playing", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			player := NewMusicPlayback(nil)
			var requests []sound.Request
			capture := func(req sound.Request) error { requests = append(requests, req); return nil }
			ctx := playbackContext(t, "office", sound.PlaybackState{Present: true}, capture)
			if err := player.Start(ctx, []WebLink{{URL: "https://audio.example/track.ogg"}}); err != nil {
				t.Fatal(err)
			}
			id := requests[0].StreamID
			pause := playbackContext(t, "office", sound.PlaybackState{Present: true, StreamID: id, OffsetMS: 1000, OffsetKnown: true, PlayerState: "Playing"}, capture)
			pause, cancel := context.WithCancel(pause)
			if _, err := player.Control(pause, "pause"); err != nil {
				t.Fatal(err)
			}
			cancel() // The queued directive was discarded before the speaker received it.
			actual := playbackContext(t, "office", sound.PlaybackState{Present: true, StreamID: id, OffsetMS: tc.offset, OffsetKnown: tc.known, PlayerState: tc.state}, capture)
			message, err := player.Control(actual, "resume")
			if err != nil {
				t.Fatal(err)
			}
			if !tc.known {
				if len(requests) != 2 || !strings.Contains(message, "позицию") {
					t.Fatalf("actual playing state reused stale pause snapshot: requests=%+v message=%q", requests, message)
				}
				return
			}
			if len(requests) != 3 || requests[2].OffsetMS != tc.offset {
				t.Fatalf("actual %s offset %d lost to discarded pause snapshot: requests=%+v", tc.state, tc.offset, requests)
			}
		})
	}
}
