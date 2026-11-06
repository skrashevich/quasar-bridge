// Package sound scopes playback to the speaker handling the current request.
package sound

import (
	"context"
	"errors"
	"strings"
	"uuid"
)

type Request struct {
	Prefetch        bool   `json:"prefetch,omitzero"`
	AfterStreamID   string `json:"after_stream_id,omitempty"`
	Sound           string `json:"sound,omitempty"`
	URL             string `json:"url,omitempty"`
	Command         string `json:"command,omitempty"`
	StreamID        string `json:"stream_id,omitempty"`
	Title           string `json:"title,omitempty"`
	OffsetMS        int64  `json:"offset_ms,omitzero"`
	Format          string `json:"format,omitempty"`
	Kind            string `json:"kind,omitempty"`
	Provider        string `json:"provider,omitempty"`
	Subtitle        string `json:"subtitle,omitempty"`
	ArtworkURL      string `json:"artwork_url,omitempty"`
	TrackID         string `json:"track_id,omitempty"`
	PermalinkURL    string `json:"permalink_url,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	Generation      uint64 `json:"generation,omitzero"`
	CallbackToken   string `json:"callback_token,omitempty"`
	RepeatMode      string `json:"repeat_mode,omitempty"`
	PreviousTrackID string `json:"previous_track_id,omitempty"`
	NextTrackID     string `json:"next_track_id,omitempty"`
	DurationMS      int64  `json:"duration_ms,omitzero"`
	Shuffle         bool   `json:"shuffle,omitzero"`
}

type PlaybackState struct {
	Present     bool
	OffsetKnown bool
	StreamID    string
	OffsetMS    int64
	PlayerState string
}
type playbackKey struct{}

func WithPlaybackState(ctx context.Context, state PlaybackState) context.Context {
	return context.WithValue(ctx, playbackKey{}, state)
}
func CurrentPlayback(ctx context.Context) PlaybackState {
	s, _ := ctx.Value(playbackKey{}).(PlaybackState)
	return s
}

// Quasar stream identifiers use Yandex TGUID spelling (8-8-8-8).
func NewStreamID() string {
	h := strings.ReplaceAll(uuid.NewV4().String(), "-", "")
	return h[:8] + "-" + h[8:16] + "-" + h[16:24] + "-" + h[24:]
}

type playerKey struct{}

func HasPlayer(ctx context.Context) bool {
	player, ok := ctx.Value(playerKey{}).(func(context.Context, Request) error)
	return ok && player != nil
}

func WithPlayer(ctx context.Context, player func(context.Context, Request) error) context.Context {
	return context.WithValue(ctx, playerKey{}, player)
}

func Play(ctx context.Context, request Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	player, ok := ctx.Value(playerKey{}).(func(context.Context, Request) error)
	if !ok || player == nil {
		return errors.New("play_sound requires a current speaker request")
	}
	return player(ctx, request)
}
