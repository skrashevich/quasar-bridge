package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	airplay "github.com/skrashevich/station-airplay"
)

type fakeAirPlayReceiver struct {
	startCtx      context.Context
	starts, stops int
	state         airplay.State
	err           error
	connected     bool
	media         string
	volume        float64
}

func (r *fakeAirPlayReceiver) Start(ctx context.Context) error {
	r.startCtx = ctx
	r.starts++
	if r.err == nil {
		r.state = airplay.StateRunning
	}
	return r.err
}
func (r *fakeAirPlayReceiver) Stop(ctx context.Context) error {
	r.stops++
	r.state = airplay.StateStopped
	return r.err
}
func (r *fakeAirPlayReceiver) Status() airplay.Status {
	return airplay.Status{State: r.state, Name: "Test AirPlay", Connected: r.connected, VolumeDB: r.volume}
}

func TestAirPlayVoiceUsesApplicationLifetime(t *testing.T) {
	app, cancelApp := context.WithCancel(t.Context())
	defer cancelApp()
	request, cancelRequest := context.WithCancel(t.Context())
	receiver := &fakeAirPlayReceiver{state: airplay.StateStopped}
	c := &airPlayControl{receiver: receiver, app: app}
	if receiver.starts != 0 || receiver.stops != 0 {
		t.Fatal("startup activated receiver")
	}
	if _, err := c.Control(request, "start"); err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	if receiver.startCtx != app || receiver.startCtx.Err() != nil {
		t.Fatal("voice request owns receiver lifetime")
	}
	if _, err := c.Control(t.Context(), "status"); err != nil {
		t.Fatal(err)
	}
	if receiver.starts != 1 || receiver.stops != 0 {
		t.Fatal("status mutated receiver")
	}
	if _, err := c.Control(t.Context(), "stop"); err != nil || receiver.stops != 1 {
		t.Fatalf("stop=%d %v", receiver.stops, err)
	}
	cancelApp()
	if receiver.startCtx.Err() == nil {
		t.Fatal("receiver outlives bridge")
	}
}

func TestAirPlayStartFailureDoesNotConfirmSuccess(t *testing.T) {
	c := &airPlayControl{receiver: &fakeAirPlayReceiver{err: errors.New("port in use")}, app: t.Context()}
	answer, err := c.Control(t.Context(), "start")
	if err == nil || answer != "" {
		t.Fatalf("false success: %q %v", answer, err)
	}
}

func TestAirPlayConfigurationIsPassive(t *testing.T) {
	state := filepath.Join(t.TempDir(), "not-created")
	t.Setenv("QUASAR_DATA_DIR", state)
	cfg, err := readConfig(configFile(t, `{"mode":"hybrid","llm_base_url":"http://127.0.0.1:1/v1","llm_model":"offline"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AirPlay.Disabled || cfg.AirPlay.DataDir != filepath.Join(state, "airplay") {
		t.Fatalf("defaults=%+v", cfg.AirPlay)
	}
	c, err := newAirPlayControl(t.Context(), cfg.AirPlay)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.receiver.Status().State; got != airplay.StateStopped {
		t.Fatalf("startup state=%s", got)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("constructor created state directory: %v", err)
	}
	for _, body := range []string{`{"airplay":{"listen_address":"bad"}}`, `{"airplay":{"output_delay_ms":-1}}`, `{"airplay":{"output_delay_ms":5001}}`, `{"airplay":{"initial_volume_db":100}}`} {
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	disabled, err := newAirPlayControl(t.Context(), airPlayConfig{Disabled: true})
	if err != nil || disabled != nil {
		t.Fatalf("disabled=%+v %v", disabled, err)
	}
}

func TestAirPlayStatusSpeech(t *testing.T) {
	for _, state := range []airplay.State{airplay.StateStopped, airplay.StateStarting, airplay.StateRunning, airplay.StateStopping} {
		c := &airPlayControl{receiver: &fakeAirPlayReceiver{state: state}, app: t.Context()}
		answer, err := c.Control(t.Context(), "status")
		if err != nil || !strings.Contains(answer, "AirPlay") {
			t.Fatalf("state=%s %q %v", state, answer, err)
		}
	}
}

func (r *fakeAirPlayReceiver) Control(_ context.Context, action string) error {
	r.media = action
	return r.err
}
func (r *fakeAirPlayReceiver) SetVolume(_ context.Context, db float64) error {
	r.volume = db
	return r.err
}

func TestAirPlayMediaAndVolumeKeepReceiverEnabled(t *testing.T) {
	r := &fakeAirPlayReceiver{state: airplay.StateRunning, connected: true}
	c := &airPlayControl{receiver: r, app: t.Context()}
	if !c.Active(t.Context()) {
		t.Fatal("connected sender not active")
	}
	for _, action := range []string{"pause", "resume", "stop", "next", "previous", "restart", "volume_up", "volume_down"} {
		if _, err := c.ControlMedia(t.Context(), action); err != nil || r.media != action {
			t.Fatalf("action=%s media=%s err=%v", action, r.media, err)
		}
	}
	for _, tc := range []struct {
		action string
		db     float64
	}{{"volume:50", -15}, {"volume:100", 0}, {"volume:0", -144}} {
		if _, err := c.ControlMedia(t.Context(), tc.action); err != nil || math.Abs(r.volume-tc.db) > 1e-9 {
			t.Fatalf("action=%s db=%f want=%f err=%v", tc.action, r.volume, tc.db, err)
		}
	}
	if r.starts != 0 || r.stops != 0 || r.state != airplay.StateRunning {
		t.Fatal("media control stopped receiver")
	}
	r.err = errors.New("sender does not support control")
	if answer, err := c.ControlMedia(t.Context(), "next"); err == nil || answer != "" {
		t.Fatalf("unsupported control claimed success: %q %v", answer, err)
	}
	r.connected = false
	if c.Active(t.Context()) {
		t.Fatal("disconnected receiver owns media")
	}
}

func TestListenerFailureStopsAirPlayBeforeReturning(t *testing.T) {
	app, cancel := context.WithCancel(t.Context())
	r := &fakeAirPlayReceiver{state: airplay.StateRunning, connected: true, startCtx: app}
	control := &airPlayControl{receiver: r, app: app}
	err := serveBridgeHTTP(&http.Server{Addr: "invalid listen address"}, "", "", func() { shutdownAirPlay(cancel, control) })
	if err == nil {
		t.Fatal("invalid listener succeeded")
	}
	if app.Err() == nil || r.stops != 1 || r.state != airplay.StateStopped {
		t.Fatalf("listener error bypassed cleanup: appErr=%v receiver=%+v", app.Err(), r)
	}
}
