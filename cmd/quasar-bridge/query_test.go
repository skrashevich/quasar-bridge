package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

type queryResolverFunc func(context.Context, string) (bridge.Resolution, error)

func (f queryResolverFunc) Resolve(ctx context.Context, text string) (bridge.Resolution, error) {
	return f(ctx, text)
}

// A failed or canceled resolution must discard even previously captured playback.
func TestResolveQueryDiscardsFailedPlayback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{name: "resolver failed"},
		{name: "canceled after capture", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := errors.New("resolution failed")
			if tc.cancel {
				want = context.Canceled
			}
			report, err := resolveQuery(ctx, queryResolverFunc(func(ctx context.Context, text string) (bridge.Resolution, error) {
				if text != "play a signal" || !sound.HasPlayer(ctx) || dialogue.SpeakerKey(ctx) == "" {
					t.Fatal("query has no diagnostic speaker/player context")
				}
				if err := sound.Play(ctx, sound.Request{URL: "https://audio.example/live.m3u8?token=private"}); err != nil {
					t.Fatal(err)
				}
				resolution := bridge.Resolution{Route: "llm", Answer: "queued"}
				if tc.cancel {
					cancel()
					return resolution, nil
				}
				return resolution, want
			}), nil, "play a signal")
			if !errors.Is(err, want) || report.Route != "llm" || report.Answer != "" || len(report.Playback) != 0 || report.PlaybackSimulated {
				t.Fatalf("failed playback reported as success: report=%+v error=%v want=%v", report, err, want)
			}
		})
	}
}

func TestResolveQueryRejectsCanceledPlayback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	report, err := resolveQuery(ctx, queryResolverFunc(func(ctx context.Context, _ string) (bridge.Resolution, error) {
		return bridge.Resolution{Route: "music"}, sound.Play(ctx, sound.Request{URL: "https://audio.example/live.m3u8"})
	}), nil, "play a stream")
	if !errors.Is(err, context.Canceled) || len(report.Playback) != 0 || report.PlaybackSimulated {
		t.Fatalf("canceled playback reported as success: report=%+v error=%v", report, err)
	}
}

func TestResolveQueryIncludesOriginalQueueTracks(t *testing.T) {
	queue, err := assistant.NewMusicQueue("https://query.invalid", assistant.NewWebReader(assistant.DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	player := assistant.NewMusicPlayback(queue)
	want := []assistant.WebLink{
		{URL: "https://audio.example/one.mp3?version=1&token=one", Title: "First track"},
		{URL: "https://audio.example/two.mp3?version=2&token=two", Title: "Second track"},
	}
	report, err := resolveQuery(t.Context(), queryResolverFunc(func(ctx context.Context, _ string) (bridge.Resolution, error) {
		return bridge.Resolution{Route: "music", Answer: "Включаю музыку."}, player.Start(ctx, want)
	}), queue, "play music")
	if err != nil || !report.PlaybackSimulated || len(report.Playback) != 1 {
		t.Fatalf("missing playback: report=%+v error=%v", report, err)
	}
	played := report.Playback[0]
	if !strings.HasPrefix(played.URL, "https://query.invalid/music-stream/") || played.StreamID == "" || !slices.Equal(played.Tracks, want) {
		t.Fatalf("original ordered queue sources lost: %+v", played)
	}
}
