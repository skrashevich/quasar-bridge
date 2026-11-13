package assistant

import (
	"context"
	"fmt"
	"testing"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

// Simulated streams cannot be acknowledged by speakers and must not evict any
// real playback binding, even when the speaker registry is already full.
func TestDiagnosticPlaybackPreservesLiveSpeakerBindings(t *testing.T) {
	for _, extension := range []string{"mp3", "m3u8"} {
		t.Run(extension, func(t *testing.T) {
			var queue *MusicQueue
			if extension == "mp3" {
				var err error
				queue, err = NewMusicQueue("https://bridge.example", NewWebReader(DefaultConfig()))
				if err != nil {
					t.Fatal(err)
				}
			}
			player := NewMusicPlayback(queue)
			var live []context.Context
			for i := range 128 {
				var request sound.Request
				ctx := playbackContext(t, fmt.Sprintf("speaker-%d", i), sound.PlaybackState{}, func(r sound.Request) error {
					request = r
					return nil
				})
				if err := player.Start(ctx, []WebLink{{URL: "https://music.example/live.m3u8"}}); err != nil {
					t.Fatal(err)
				}
				ctx = sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: request.StreamID, PlayerState: "Playing"})
				if !player.Active(ctx) {
					t.Fatalf("speaker %d did not bind its reported stream", i)
				}
				live = append(live, ctx)
			}
			ctx, _ := queue.WithIsolatedPlayback(dialogue.WithSpeaker(t.Context(), "quasar-bridge-dashboard"))
			var simulated sound.Request
			ctx = sound.WithPlayer(ctx, func(_ context.Context, request sound.Request) error {
				simulated = request
				return nil
			})
			if err := player.Start(ctx, []WebLink{{URL: "https://music.example/test." + extension}}); err != nil {
				t.Fatal(err)
			}
			if simulated.URL == "" || simulated.StreamID == "" {
				t.Fatalf("missing simulated request: %+v", simulated)
			}
			if player.Active(sound.WithPlaybackState(ctx, sound.PlaybackState{Present: true, StreamID: simulated.StreamID})) {
				t.Error("simulated playback created a live speaker binding")
			}
			for i, ctx := range live {
				if !player.Active(ctx) {
					t.Errorf("diagnostic playback evicted speaker %d", i)
				}
			}
		})
	}
}
