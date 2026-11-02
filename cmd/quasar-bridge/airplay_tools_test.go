package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"

	airplay "github.com/skrashevich/station-airplay"
	"quasarbridge/internal/assistant"
)

func TestAirPlayToolsUseLiveReceiver(t *testing.T) {
	app, cancelApp := context.WithCancel(t.Context())
	defer cancelApp()
	request, cancelRequest := context.WithCancel(t.Context())
	r := &fakeAirPlayReceiver{state: airplay.StateStopped, volume: -15}
	c := &airPlayControl{receiver: r, app: app}
	tools := &assistant.Tools{AirPlay: c}
	if _, err := tools.Call(request, "control_airplay", `{"action":"start"}`); err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	if r.startCtx != app || r.startCtx.Err() != nil {
		t.Fatal("LLM request owns receiver lifetime")
	}
	result, err := tools.Call(t.Context(), "get_airplay_status", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var state assistant.AirPlayStatus
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if !state.Enabled || state.State != "running" || state.Connected || state.Playing || state.Name != "Test AirPlay" || state.VolumeDB != -15 {
		t.Fatalf("receiver readiness confused with playback: %+v", state)
	}
	if _, err := tools.Call(t.Context(), "control_airplay_playback", `{"action":"next"}`); err == nil || r.media != "" {
		t.Fatal("media command without a sender succeeded")
	}
	r.connected = true
	if _, err := tools.Call(t.Context(), "control_airplay_playback", `{"action":"stop"}`); err != nil {
		t.Fatal(err)
	}
	if r.media != "stop" || r.stops != 0 || r.state != airplay.StateRunning {
		t.Fatal("playback stop disabled receiver")
	}
	if _, err := tools.Call(t.Context(), "set_airplay_volume", `{"volume":35}`); err != nil || r.volume != -19.5 {
		t.Fatalf("volume=%v err=%v", r.volume, err)
	}
	if _, err := tools.Call(t.Context(), "control_airplay", `{"action":"stop"}`); err != nil || r.stops != 1 {
		t.Fatalf("receiver stop=%d err=%v", r.stops, err)
	}
}

func TestAirPlayToolStatusHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := &airPlayControl{receiver: &fakeAirPlayReceiver{}, app: t.Context()}
	if _, err := c.Status(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("status ignored cancellation: %v", err)
	}
}
