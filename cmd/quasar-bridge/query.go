package main

import (
	"context"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

type queryResolution struct {
	bridge.Resolution
	Playback          []queryPlayback `json:"playback,omitempty"`
	PlaybackSimulated bool            `json:"playback_simulated,omitzero"`
}

type queryPlayback struct {
	sound.Request
	Tracks []assistant.WebLink `json:"tracks,omitempty"`
}

// resolveQuery captures speaker audio requests without a physical player transport.
func resolveQuery(ctx context.Context, resolver bridge.Resolver, queue *assistant.MusicQueue, text string) (queryResolution, error) {
	var playback []queryPlayback
	ctx = sound.WithPlayer(dialogue.WithSpeaker(ctx, "query-diagnostics"), func(ctx context.Context, request sound.Request) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		captured := queryPlayback{Request: request}
		if request.URL != "" {
			captured.Tracks = []assistant.WebLink{{URL: request.URL, Title: request.Title}}
			if queue != nil {
				if tracks, err := queue.Tracks(request.URL); err == nil {
					captured.Tracks = tracks
				}
			}
		}
		playback = append(playback, captured)
		return nil
	})
	resolution, err := resolver.Resolve(ctx, text)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return queryResolution{Route: resolution.Route}, err
	}
	return queryResolution{Resolution: resolution, Playback: playback, PlaybackSimulated: len(playback) > 0}, nil
}
