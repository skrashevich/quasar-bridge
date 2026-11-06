package bridge

import (
	"context"
	"encoding/hex"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	app "github.com/skrashevich/quasar-go-genproto/proto/alice/library/client/protos"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	slot "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/scenarios/frames/common"
	media "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/scenarios/frames/media_frame"
	device "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/common/device_state"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/wire"
)

// Captured from Midi's local PlaybackControlCapability PLAY request after pause.
const midiContinueFrame = "0a07aa03041a020801121310031a0f706c617965725f636f6e74696e7565"

type blockingSemanticControlProvider struct {
	soundCloudStub
	canHandle func(context.Context, string) bool
}

func (p blockingSemanticControlProvider) CanHandle(ctx context.Context, command string) bool {
	return p.canHandle(ctx, command)
}

func TestNativeSoundCloudSemanticArrivalBeforeBlockedOwnership(t *testing.T) {
	raw, _ := hex.DecodeString(midiContinueFrame)
	entered, release := make(chan struct{}), make(chan struct{})
	e := &Engine{SoundCloud: blockingSemanticControlProvider{soundCloudStub: soundCloudStub{handle: func(context.Context, string) (string, error) {
		t.Error("older native control executed after newer input")
		return "", nil
	}}, canHandle: func(ctx context.Context, _ string) bool {
		if sound.RequestSequence(ctx) != 0 {
			t.Error("ownership check used stale-request shortcut")
		}
		close(entered)
		<-release
		return true
	}}}
	n, up, down := soundSession(t, e)
	done := make(chan error, 1)
	go func() {
		done <- n.Client(t.Context(), websocket.MessageBinary, semanticControlInput("old-control", raw))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("ownership did not block")
	}
	newer := e.Prepare(dialogue.WithSpeaker(t.Context(), "device:control-speaker"))
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	n.wg.Wait()
	if !e.ComparisonCurrent(newer) || len(up) != 0 || len(down) != 0 {
		t.Fatal("old admission superseded or leaked after newer input")
	}
}

func semanticControlInput(id string, raw []byte) []byte {
	f := wire.Decode(soundCloudEventInput(id, "@@mm_semantic_frame", true, nil), wire.ClientToServer)
	r := f.Client.GetEvent().GetTextInput()
	r.Application = &app.TClientInfoProto{DeviceId: new("control-speaker")}
	r.Request.Event.PayloadType = &common.TEvent_PayloadRaw{PayloadRaw: raw}
	r.Request.DeviceStateType = &request.TAliceRequestProto_TRequest_DeviceState{DeviceState: &device.TDeviceState{AudioPlayer: &device.TDeviceState_TAudioPlayer{PlayerState: device.TDeviceState_TAudioPlayer_Paused.Enum(), OffsetMs: new(int32(12000)), CurrentlyPlaying: &device.TDeviceState_TAudioPlayer_TCurrentlyPlaying{StreamId: new("control-stream")}}}}
	return aa(f.Client)
}

func TestNativeSoundCloudSemanticDuplicateDoesNotCancelControl(t *testing.T) {
	raw, _ := hex.DecodeString(midiContinueFrame)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e := &Engine{SoundCloud: soundCloudStub{can: true, handle: func(ctx context.Context, _ string) (string, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "", sound.Play(ctx, sound.Request{URL: "https://audio.example/resume.m3u8"})
	}}}
	n, up, down := soundSession(t, e)
	input := semanticControlInput("duplicate", raw)
	if err := n.Client(t.Context(), websocket.MessageBinary, input); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("control did not start")
	}
	if err := n.Client(t.Context(), websocket.MessageBinary, input); err != nil {
		t.Fatal(err)
	}
	close(release)
	silentSoundCloudResponse(t, down)
	if len(soundCloudPushDirectives(t, down)) != 1 {
		t.Fatal("duplicate canceled pending control")
	}
	n.wg.Wait()
	if calls.Load() != 1 || len(up) != 0 {
		t.Fatal("duplicate executed or escaped")
	}
}

func TestSoundCloudSemanticAllowedFrames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame *common.TTypedSemanticFrame
		want  string
	}{
		{"next", &common.TTypedSemanticFrame{Type: &common.TTypedSemanticFrame_PlayerNextTrackSemanticFrame{PlayerNextTrackSemanticFrame: &media.TPlayerNextTrackSemanticFrame{}}}, "следующий трек"},
		{"previous", &common.TTypedSemanticFrame{Type: &common.TTypedSemanticFrame_PlayerPrevTrackSemanticFrame{PlayerPrevTrackSemanticFrame: &media.TPlayerPrevTrackSemanticFrame{}}}, "предыдущий трек"},
		{"next-paused", &common.TTypedSemanticFrame{Type: &common.TTypedSemanticFrame_PlayerNextTrackSemanticFrame{PlayerNextTrackSemanticFrame: &media.TPlayerNextTrackSemanticFrame{SetPause: &slot.TBoolSlot{Value: &slot.TBoolSlot_BoolValue{BoolValue: true}}}}}, ""},
		{"unsupported-shuffle", &common.TTypedSemanticFrame{Type: &common.TTypedSemanticFrame_PlayerShuffleSemanticFrame{PlayerShuffleSemanticFrame: &media.TPlayerShuffleSemanticFrame{}}}, ""},
		{"video-target", &common.TTypedSemanticFrame{Type: &common.TTypedSemanticFrame_PlayerContinueSemanticFrame{PlayerContinueSemanticFrame: &media.TPlayerContinueSemanticFrame{PlayerType: &media.TPlayerTypeSlot{Value: &media.TPlayerTypeSlot_PlayerTypeValue{PlayerTypeValue: media.TPlayerTypeSlot_video}}}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := proto.Marshal(&common.TSemanticFrameRequestData{TypedSemanticFrame: tc.frame})
			if err != nil {
				t.Fatal(err)
			}
			event := &common.TEvent{Name: new("@@mm_semantic_frame"), PayloadType: &common.TEvent_PayloadRaw{PayloadRaw: raw}}
			if got := soundCloudSemanticControl(event); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestNativeSoundCloudSemanticContinueSilentLocal(t *testing.T) {
	raw, _ := hex.DecodeString(midiContinueFrame)
	var calls atomic.Int32
	e := &Engine{SoundCloud: soundCloudStub{can: true, handle: func(ctx context.Context, text string) (string, error) {
		calls.Add(1)
		if text != "продолжи музыку" || dialogue.SpeakerKey(ctx) != dialogue.SpeakerKey(dialogue.WithSpeaker(t.Context(), "device:control-speaker")) || sound.CurrentPlayback(ctx).StreamID != "control-stream" || sound.RequestSequence(ctx) == 0 {
			t.Error("native control lost its command, speaker, observation or ordering")
		}
		return "Must not be spoken", sound.Play(ctx, sound.Request{URL: "https://audio.example/resumed.m3u8"})
	}}}
	n, up, down := soundSession(t, e)
	dialogueCtx, _, ticket := e.dialogue().Start(dialogue.WithSpeaker(t.Context(), "device:control-speaker"))
	if err := n.Client(t.Context(), websocket.MessageBinary, semanticControlInput("continue-control", raw)); err != nil {
		t.Fatal(err)
	}
	if answer := silentSoundCloudResponse(t, down); len(answer.GetResponse().GetDirectives()) != 0 {
		t.Fatal("semantic control used an ignored response for audio")
	}
	if ds := soundCloudPushDirectives(t, down); len(ds) != 1 {
		t.Fatal("semantic control did not deliver one push")
	}
	n.wg.Wait()
	if calls.Load() != 1 || len(up) != 0 {
		t.Fatal("semantic control escaped upstream or ran twice")
	}
	if !e.dialogue().Current(dialogueCtx, ticket) {
		t.Fatal("semantic control started a conversational turn")
	}
}

func TestNativeSoundCloudSemanticControlSupersessionAndAirPlay(t *testing.T) {
	for _, takeover := range []string{"new-input", "airplay"} {
		t.Run(takeover, func(t *testing.T) {
			raw, _ := hex.DecodeString(midiContinueFrame)
			entered, release := make(chan struct{}), make(chan struct{})
			airplay := &airPlayMediaFake{}
			e := &Engine{AirPlay: airplay, SoundCloud: soundCloudStub{can: true, handle: func(ctx context.Context, _ string) (string, error) {
				close(entered)
				<-release
				return "", sound.Play(ctx, sound.Request{URL: "https://audio.example/resumed.m3u8", Provider: "SoundCloud", SessionID: "session", StreamID: "stream", CallbackToken: "token", Generation: 1})
			}}}
			n, up, down := soundSession(t, e)
			if err := n.Client(t.Context(), websocket.MessageBinary, semanticControlInput("superseded", raw)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("control did not begin")
			}
			if takeover == "new-input" {
				e.Prepare(dialogue.WithSpeaker(t.Context(), "device:control-speaker"))
			} else {
				airplay.active = true
			}
			close(release)
			if takeover == "airplay" {
				silentSoundCloudResponse(t, down)
			}
			n.wg.Wait()
			if len(up) != 0 || len(down) != 0 {
				t.Fatal("superseded native control leaked output")
			}
		})
	}
}

func TestNativeSoundCloudSemanticControlPassThrough(t *testing.T) {
	valid, _ := hex.DecodeString(midiContinueFrame)
	for _, tc := range []struct {
		name           string
		raw            []byte
		owned, airplay bool
	}{
		{"not-owned", valid, false, false},
		{"airplay", valid, true, true},
		{"malformed", []byte{0xff}, true, false},
		{"oversized", []byte(strings.Repeat("x", 65537)), true, false},
		{"empty", nil, true, false},
		{"unknown-envelope", protowire.AppendVarint(protowire.AppendTag(append([]byte(nil), valid...), 1000, protowire.VarintType), 1), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{SoundCloud: soundCloudStub{can: tc.owned, handle: func(context.Context, string) (string, error) {
				t.Fatal("unclaimed semantic control reached SoundCloud")
				return "", nil
			}}}
			if tc.airplay {
				e.AirPlay = &airPlayMediaFake{active: true}
			}
			n, up, down := soundSession(t, e)
			input := semanticControlInput("pass", tc.raw)
			if err := n.Client(t.Context(), websocket.MessageBinary, input); err != nil {
				t.Fatal(err)
			}
			f := nextSoundFrame(t, up)
			if string(f.Raw) != string(input) || len(down) != 0 {
				t.Fatal("unclaimed semantic frame changed")
			}
		})
	}
}

func TestNativeSoundCloudSemanticRequiresPlaybackObservation(t *testing.T) {
	raw, _ := hex.DecodeString(midiContinueFrame)
	for _, missing := range []string{"state", "stream"} {
		t.Run(missing, func(t *testing.T) {
			e := &Engine{SoundCloud: soundCloudStub{can: true, handle: func(context.Context, string) (string, error) {
				t.Fatal("missing playback observation was claimed")
				return "", nil
			}}}
			n, up, down := soundSession(t, e)
			f := wire.Decode(semanticControlInput("missing-"+missing, raw), wire.ClientToServer)
			if missing == "state" {
				f.Client.GetEvent().GetTextInput().Request.DeviceStateType = nil
			} else {
				f.Client.GetEvent().GetTextInput().GetRequest().GetDeviceState().AudioPlayer.CurrentlyPlaying = nil
			}
			original := aa(f.Client)
			if err := n.Client(t.Context(), websocket.MessageBinary, original); err != nil {
				t.Fatal(err)
			}
			if got := nextSoundFrame(t, up); string(got.Raw) != string(original) || len(down) != 0 {
				t.Fatal("missing observation changed cloud input")
			}
		})
	}
}

func TestNativeSoundCloudSemanticRealProviderScope(t *testing.T) {
	for _, scope := range []string{"matching", "foreign-stream", "other-speaker"} {
		t.Run(scope, func(t *testing.T) {
			provider := soundCloudIntegrationProvider(t, soundCloudCatalogFixture)
			var initial sound.Request
			seed := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "device:control-speaker"), func(_ context.Context, request sound.Request) error { initial = request; return nil })
			if _, err := provider.Handle(seed, "включи First SoundCloud"); err != nil {
				t.Fatal(err)
			}
			if initial.StreamID == "" {
				t.Fatal("fixture did not start a SoundCloud session")
			}
			n, up, down := soundSession(t, &Engine{SoundCloud: provider})
			raw, _ := hex.DecodeString(midiContinueFrame)
			f := wire.Decode(semanticControlInput("real-control", raw), wire.ClientToServer)
			req := f.Client.GetEvent().GetTextInput()
			req.GetRequest().GetDeviceState().GetAudioPlayer().GetCurrentlyPlaying().StreamId = new(initial.StreamID)
			if scope == "foreign-stream" {
				req.GetRequest().GetDeviceState().GetAudioPlayer().GetCurrentlyPlaying().StreamId = new("yandex-stream")
			}
			if scope == "other-speaker" {
				req.GetApplication().DeviceId = new("other-speaker")
			}
			original := aa(f.Client)
			if err := n.Client(t.Context(), websocket.MessageBinary, original); err != nil {
				t.Fatal(err)
			}
			if scope != "matching" {
				if got := nextSoundFrame(t, up); string(got.Raw) != string(original) || len(down) != 0 {
					t.Fatal("foreign control was not preserved")
				}
				return
			}
			silentSoundCloudResponse(t, down)
			ds := soundCloudPushDirectives(t, down)
			if len(ds) != 1 || ds[0].GetPayload().GetFields()["provider_name"].GetStringValue() != "SoundCloud" || ds[0].GetPayload().GetFields()["stream"].GetStructValue().GetFields()["offset_ms"].GetNumberValue() != 12000 {
				t.Fatal("owned control did not resume exact SoundCloud position")
			}
			n.wg.Wait()
			if len(up) != 0 {
				t.Fatal("owned control escaped to Yandex")
			}
		})
	}
}
