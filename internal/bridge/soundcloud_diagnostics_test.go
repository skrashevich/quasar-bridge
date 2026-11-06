package bridge

import (
	"bytes"
	"encoding/json/v2"
	"log/slog"
	"strings"
	"testing"

	"github.com/coder/websocket"
	device "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/common/device_state"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	"google.golang.org/protobuf/proto"
	"quasarbridge/internal/wire"
)

func TestNativeSoundCloudCallbackSafeDiagnostics(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "struct", true: "raw"}[raw], func(t *testing.T) {
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			defer slog.SetDefault(previous)
			payload := map[string]any{"session_id": "private-session", "stream_id": "private-stream", "token": "private-token", "reason": "private-untrusted-reason", "position_ms": 140625, "duration_ms": 141000, "finished": true, "url": "https://private.invalid/signed", "https://private.invalid/key": "private-value"}
			f := wire.Decode(soundCloudEventInput("private-message", "music_thin_client_on_stopped", raw, payload), wire.ClientToServer)
			req := f.Client.GetEvent().GetTextInput()
			req.Request.Event.IgnoreAnswer = new(true)
			state := &device.TDeviceState{AudioPlayer: &device.TDeviceState_TAudioPlayer{PlayerState: device.TDeviceState_TAudioPlayer_Stopped.Enum(), OffsetMs: new(int32(140625)), DurationMs: new(int32(141000)), CurrentlyPlaying: &device.TDeviceState_TAudioPlayer_TCurrentlyPlaying{StreamId: new("private-stream")}}}
			if raw {
				b, _ := proto.Marshal(state)
				req.Request.DeviceStateType = &request.TAliceRequestProto_TRequest_DeviceStateRaw{DeviceStateRaw: b}
			} else {
				req.Request.DeviceStateType = &request.TAliceRequestProto_TRequest_DeviceState{DeviceState: state}
			}
			n, _, down := soundSession(t, new(Engine))
			if err := n.Client(t.Context(), websocket.MessageBinary, aa(f.Client)); err != nil {
				t.Fatal(err)
			}
			silentSoundCloudResponse(t, down)
			n.wg.Wait()
			found := false
			for line := range strings.SplitSeq(strings.TrimSpace(output.String()), "\n") {
				var d map[string]any
				if json.Unmarshal([]byte(line), &d) != nil {
					continue
				}
				if d["msg"] != "native_soundcloud_callback" {
					continue
				}
				found = true
				for k, want := range map[string]any{"scope_kind": "connection", "action": "stopped", "ignore_answer": true, "payload_valid": true, "player_state": "Stopped", "offset_known": true, "offset_ms": float64(140625), "duration_known": true, "duration_ms": float64(141000), "has_stream": true} {
					if d[k] != want {
						t.Errorf("%s=%v want=%v", k, d[k], want)
					}
				}
				keys, ok := d["payload_keys"].([]any)
				if !ok || len(keys) > 24 {
					t.Fatalf("unbounded/missing key diagnostics: %v", d["payload_keys"])
				}
			}
			if !found {
				t.Fatal("missing callback diagnostics")
			}
			if strings.Contains(output.String(), "private-") || strings.Contains(output.String(), "private.invalid") {
				t.Fatal("diagnostics exposed callback data")
			}
		})
	}
}
