package bridge

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"testing"

	"github.com/coder/websocket"
	device "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/common/device_state"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	"google.golang.org/protobuf/proto"

	"quasarbridge/internal/sound"
)

type musicLogWriter chan string

func (w musicLogWriter) Write(data []byte) (int, error) {
	select {
	case w <- string(data):
	default:
	}
	return len(data), nil
}

func TestNativeMusicFailureDoesNotBlameModelOrPlayQueuedAudio(t *testing.T) {
	logs := make(musicLogWriter, 64)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	n, _, down := soundSession(t, resolverFunc(func(ctx context.Context, _ string) (Resolution, error) {
		if err := sound.Play(ctx, sound.Request{URL: "https://audio.example/song.mp3"}); err != nil {
			t.Fatal(err)
		}
		return Resolution{Route: "music"}, errors.New("cannotfetchpage")
	}))
	defer n.Close()
	if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("music-failure")); err != nil {
		t.Fatal(err)
	}
	for {
		f := nextSoundFrame(t, down)
		if r := f.Server.GetEvent().GetAliceResponse(); r != nil {
			if len(r.GetResponse().GetDirectives()) != 0 || r.GetVoiceResponse().GetOutputSpeech().GetText() != "Не удалось включить музыку с этой страницы." {
				t.Fatalf("response=%v", r)
			}
			break
		}
	}
	for len(logs) > 0 {
		var event struct {
			Message string `json:"msg"`
			Request string `json:"request"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal([]byte(<-logs), &event); err != nil {
			t.Fatal(err)
		}
		if event.Message == "native_music_failed" && event.Error == "cannotfetchpage" && event.Request != "" && event.Request != "music-failure" {
			return
		}
	}
	t.Fatal("music failure cause is missing from diagnostics")
}

func TestPlayerStateTypedAndRaw(t *testing.T) {
	state := &device.TDeviceState{AudioPlayer: &device.TDeviceState_TAudioPlayer{PlayerState: device.TDeviceState_TAudioPlayer_Playing.Enum(), OffsetMs: new(int32(12345)), CurrentlyPlaying: &device.TDeviceState_TAudioPlayer_TCurrentlyPlaying{StreamId: new("stream-1")}}}
	raw, err := proto.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []*request.TAliceRequestProto{{Request: &request.TAliceRequestProto_TRequest{DeviceStateType: &request.TAliceRequestProto_TRequest_DeviceState{DeviceState: state}}}, {Request: &request.TAliceRequestProto_TRequest{DeviceStateType: &request.TAliceRequestProto_TRequest_DeviceStateRaw{DeviceStateRaw: raw}}}} {
		got := nativePlaybackState(req)
		if !got.Present || !got.OffsetKnown || got.StreamID != "stream-1" || got.OffsetMS != 12345 || got.PlayerState != "Playing" {
			t.Fatalf("state=%+v", got)
		}
	}
	absent := nativePlaybackState(&request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{DeviceStateType: &request.TAliceRequestProto_TRequest_DeviceState{DeviceState: &device.TDeviceState{AudioPlayer: &device.TDeviceState_TAudioPlayer{CurrentlyPlaying: &device.TDeviceState_TAudioPlayer_TCurrentlyPlaying{StreamId: new("stream-1")}}}}}})
	if !absent.Present || absent.OffsetKnown {
		t.Fatalf("absent position became zero: %+v", absent)
	}
	if got := nativePlaybackState(&request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{DeviceStateType: &request.TAliceRequestProto_TRequest_DeviceStateRaw{DeviceStateRaw: []byte{255}}}}); got.Present {
		t.Fatal("malformed state treated asvalid")
	}
}

func TestNativePlaybackControlsAreSilent(t *testing.T) {
	for _, command := range []string{"pause", "stop"} {
		t.Run(command, func(t *testing.T) {
			n, up, down := soundSession(t, resolverFunc(func(ctx context.Context, _ string) (Resolution, error) {
				return Resolution{Route: "playback", Answer: "Пауза."}, sound.Play(ctx, sound.Request{Command: command})
			}))
			defer n.Close()
			if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("pause")); err != nil {
				t.Fatal(err)
			}
			for {
				f := nextSoundFrame(t, down)
				if r := f.Server.GetEvent().GetAliceResponse(); r != nil {
					if r.GetVoiceResponse().GetShouldListen() {
						t.Fatal("playback control reopened listening")
					}
					if r.GetVoiceResponse().GetHasVoiceResponse() {
						t.Fatal("playercontrol synthesizedspeech")
					}
					d := r.GetResponse().GetDirectives()
					if len(d) != 1 || d[0].GetName() != "audio_stop" {
						t.Fatalf("directives=%v", d)
					}
					break
				}
			}
			for len(up) > 0 {
				if f := <-up; f.Kind() == "TtsGenerate" {
					t.Fatal("control causedTTS")
				}
			}
		})
	}
}

func TestPlaybackDirectivePreservesIdentityAndOffset(t *testing.T) {
	d, err := makeSoundDirective(sound.Request{URL: "https://audio.example/song.mp3", StreamID: "test-stream", Title: "Song", OffsetMS: 12000})
	if err != nil {
		t.Fatal(err)
	}
	p := d.GetPayload().AsMap()
	stream := p["stream"].(map[string]any)
	if stream["id"] != "test-stream" || stream["offset_ms"] != float64(12000) || p["set_pause"] != false || p["metadata"].(map[string]any)["title"] != "Song" {
		t.Fatalf("payload=%v", p)
	}
}
