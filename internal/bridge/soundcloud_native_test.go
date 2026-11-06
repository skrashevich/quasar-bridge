package bridge

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	app "github.com/skrashevich/quasar-go-genproto/proto/alice/library/client/protos"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	device "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/common/device_state"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	asr "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	cancellation "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/cancel_request"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/wire"
)

type soundCloudEventResolver struct {
	resolverFunc
	handle              func(context.Context, string, map[string]any) (bool, error)
	prepared, delivered atomic.Int32
}

func (r *soundCloudEventResolver) Prepare(ctx context.Context) context.Context {
	r.prepared.Add(1)
	return ctx
}
func (r *soundCloudEventResolver) RecordDelivered(context.Context, string, string, bool, bool) {
	r.delivered.Add(1)
}
func (r *soundCloudEventResolver) HandleEvent(ctx context.Context, name string, payload map[string]any) (bool, error) {
	return r.handle(ctx, name, payload)
}

func soundCloudEventInput(id, name string, raw bool, payload map[string]any) []byte {
	e := &common.TEvent{Name: new(name)}
	if raw {
		b, _ := json.Marshal(payload)
		e.PayloadType = &common.TEvent_PayloadRaw{PayloadRaw: b}
	} else {
		p, _ := structpb.NewStruct(payload)
		e.PayloadType = &common.TEvent_Payload{Payload: p}
	}
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: e}}}}}})
}
func soundCloudPayload() map[string]any {
	return map[string]any{"session_id": "session", "stream_id": "stream", "token": "token"}
}
func silentSoundCloudResponse(t *testing.T, down <-chan wire.Frame) *response.TAliceResponseProto {
	t.Helper()
	if f := nextSoundFrame(t, down); f.Kind() != "InputStartAck" {
		t.Fatalf("ack kind=%s", f.Kind())
	}
	f := nextSoundFrame(t, down)
	r := f.Server.GetEvent().GetAliceResponse()
	if r == nil || r.GetVoiceResponse().GetShouldListen() || r.GetVoiceResponse().GetHasVoiceResponse() || r.GetVoiceResponse().GetOutputSpeech().GetText() != "" || len(r.GetResponse().GetCards()) != 0 {
		t.Fatalf("callback was not silent: %v", r)
	}
	return r
}
func TestNativeSoundCloudCallbackFreshRequest(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "struct", true: "raw"}[raw], func(t *testing.T) {
			calls := new(atomic.Int32)
			var original context.Context
			r := &soundCloudEventResolver{resolverFunc: func(ctx context.Context, _ string) (Resolution, error) {
				original = ctx
				return Resolution{Route: "soundcloud"}, sound.Play(ctx, sound.Request{URL: "https://audio.example/first.m3u8"})
			}}
			r.handle = func(ctx context.Context, name string, payload map[string]any) (bool, error) {
				calls.Add(1)
				if name != "quasar_soundcloud_finished" || payload["session_id"] != "session" {
					t.Error("event normalization failed")
				}
				if dialogue.SpeakerKey(ctx) != dialogue.SpeakerKey(original) {
					t.Error("speaker scope changed")
				}
				return true, sound.Play(ctx, sound.Request{URL: "https://audio.example/next.m3u8"})
			}
			n, up, down := soundSession(t, r)
			if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("original")); err != nil {
				t.Fatal(err)
			}
			if ds := finalSoundDirectives(t, down); len(ds) != 1 {
				t.Fatal("initial sound missing")
			}
			n.wg.Wait()
			n.mu.Lock()
			p := n.requests["original"]
			p.cancel()
			p.canceled = true
			p.expires.Stop()
			delete(n.requests, p.id)
			n.mu.Unlock()
			if err := sound.Play(original, sound.Request{URL: "https://audio.example/forbidden.mp3"}); err == nil {
				t.Fatal("expired original player accepted sound")
			}
			b := soundCloudEventInput("callback", "quasar_soundcloud_finished", raw, soundCloudPayload())
			if err := n.Client(t.Context(), websocket.MessageBinary, b); err != nil {
				t.Fatal(err)
			}
			answer := silentSoundCloudResponse(t, down)
			if len(answer.GetResponse().GetDirectives()) != 0 {
				t.Fatal("callback audio must not be attached to ignored AliceResponse")
			}
			if ds := soundCloudPushDirectives(t, down); len(ds) != 1 || ds[0].GetPayload().GetFields()["stream"].GetStructValue().GetFields()["url"].GetStringValue() != "https://audio.example/next.m3u8" {
				t.Fatal("next was not queued on fresh callback")
			}
			n.wg.Wait()
			if err := n.Client(t.Context(), websocket.MessageBinary, b); err != nil {
				t.Fatal(err)
			}
			n.wg.Wait()
			if calls.Load() != 1 || r.prepared.Load() != 1 || r.delivered.Load() != 1 || len(up) != 0 || len(down) != 0 {
				t.Fatalf("callback side effects calls=%d prepare=%d delivered=%d up=%d down=%d", calls.Load(), r.prepared.Load(), r.delivered.Load(), len(up), len(down))
			}
		})
	}
}
func TestNativeSoundCloudSilentMalformedStaleAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		payload   map[string]any
		raw, fail bool
		malformed string
	}{
		{name: "state", payload: soundCloudPayload()}, {name: "stale", payload: soundCloudPayload(), raw: true}, {name: "failure", payload: soundCloudPayload(), fail: true}, {name: "missing", payload: map[string]any{}}, {name: "wrong-type", payload: map[string]any{"session_id": 12, "stream_id": "x", "token": "y"}}, {name: "nonobject", malformed: `[]`, raw: true}, {name: "invalid-json", malformed: `{`, raw: true}, {name: "oversize", malformed: strings.Repeat(" ", 65537), raw: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := new(atomic.Int32)
			r := &soundCloudEventResolver{resolverFunc: func(context.Context, string) (Resolution, error) {
				t.Error("callback reached Resolve")
				return Resolution{}, nil
			}, handle: func(ctx context.Context, _ string, _ map[string]any) (bool, error) {
				calls.Add(1)
				if tc.fail {
					if err := sound.Play(ctx, sound.Request{URL: "https://audio.example/next.m3u8"}); err != nil {
						t.Error(err)
					}
					return true, errors.New("failed")
				}
				return true, nil
			}}
			n, up, down := soundSession(t, r)
			n.GateText = true
			b := soundCloudEventInput("event", "quasar_soundcloud_started", tc.raw, tc.payload)
			if tc.malformed != "" {
				f := wire.Decode(b, wire.ClientToServer)
				f.Client.GetEvent().GetTextInput().GetRequest().GetEvent().PayloadType = &common.TEvent_PayloadRaw{PayloadRaw: []byte(tc.malformed)}
				b = aa(f.Client)
			}
			if err := n.Client(t.Context(), websocket.MessageBinary, b); err != nil {
				t.Fatal(err)
			}
			if r := silentSoundCloudResponse(t, down); len(r.GetResponse().GetDirectives()) != 0 {
				t.Fatal("invalid event emitted sound")
			}
			n.wg.Wait()
			want := int32(0)
			if tc.name == "state" || tc.name == "stale" || tc.fail {
				want = 1
			}
			if calls.Load() != want || r.prepared.Load() != 0 || r.delivered.Load() != 0 || len(up) != 0 {
				t.Fatalf("callback side effects calls=%d want=%d prepare=%d delivered=%d up=%d", calls.Load(), want, r.prepared.Load(), r.delivered.Load(), len(up))
			}
		})
	}
}
func TestNativeSoundCloudMissingIDAndUnrelatedEvent(t *testing.T) {
	r := &soundCloudEventResolver{handle: func(context.Context, string, map[string]any) (bool, error) {
		t.Error("invalid callback called handler")
		return true, nil
	}}
	n, up, down := soundSession(t, r)
	for _, tc := range []struct{ id, name string }{{"", "quasar_soundcloud_finished"}, {" ", "quasar_soundcloud_finished"}, {strings.Repeat("x", 257), "quasar_soundcloud_finished"}, {"ordinary", "music_thin_client_on_finished"}} {
		if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudEventInput(tc.id, tc.name, false, soundCloudPayload())); err != nil {
			t.Fatal(err)
		}
	}
	n.wg.Wait()
	if len(up) != 1 || len(down) != 0 || nextSoundFrame(t, up).Client.GetEvent().GetHeader().GetMessageId() != "ordinary" {
		t.Fatal("missing-id/unrelated event handling failed")
	}
}
func TestNativeSoundCloudCanceledCallback(t *testing.T) {
	entered := make(chan struct{})
	r := &soundCloudEventResolver{handle: func(ctx context.Context, _ string, _ map[string]any) (bool, error) {
		close(entered)
		<-ctx.Done()
		if err := sound.Play(ctx, sound.Request{URL: "https://audio.example/next.m3u8"}); err == nil {
			t.Error("canceled event accepted sound")
		}
		return true, ctx.Err()
	}}
	n, up, down := soundSession(t, r)
	if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudEventInput("event", "quasar_soundcloud_finished", false, soundCloudPayload())); err != nil {
		t.Fatal(err)
	}
	<-entered
	b := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "cancel"}, Event: &client.TClientMessage_TEvent_CancelRequest{CancelRequest: &cancellation.TCancelRequest{RequestMessageId: "event"}}}}})
	if err := n.Client(t.Context(), websocket.MessageBinary, b); err != nil {
		t.Fatal(err)
	}
	n.wg.Wait()
	if len(down) != 0 || len(up) != 0 {
		t.Fatal("canceled local callback escaped")
	}
}
func TestSoundCloudDirectiveMetadataRoundTrip(t *testing.T) {
	r := sound.Request{URL: "http://localhost:8080/soundcloud/resource", StreamID: "stream", Title: "Title", Subtitle: "Artist", Format: "HLS", Kind: "Track", Provider: "SoundCloud", ArtworkURL: "http://localhost:8080/soundcloud/art", TrackID: "soundcloud:tracks:123", PermalinkURL: "https://soundcloud.com/artist/title", DurationMS: 123000, OffsetMS: 5000, SessionID: "session", CallbackToken: "token", PreviousTrackID: "prev", NextTrackID: "next", RepeatMode: "All", Shuffle: true}
	d, err := makeSoundDirective(r)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	roundtrip := new(soundDirective)
	if err = proto.Unmarshal(encoded, roundtrip); err != nil {
		t.Fatal(err)
	}
	p := roundtrip.GetPayload().AsMap()
	s := p["stream"].(map[string]any)
	if s["format"] != "HLS" || s["type"] != "Track" || s["duration_ms"] != float64(123000) || p["screen_type"] != "Music" || p["provider_name"] != "SoundCloud" {
		t.Fatalf("stream metadata: %v", p)
	}
	m := p["metadata"].(map[string]any)
	if m["title"] != "Title" || m["subtitle"] != "Artist" || m["art_image_url"] != r.ArtworkURL || m["hide_progress_bar"] != false {
		t.Fatal("display metadata missing")
	}
	gm := m["glagol_metadata"].(map[string]any)["music_metadata"].(map[string]any)
	for k, v := range map[string]any{"id": r.TrackID, "type": "Track", "description": r.PermalinkURL, "repeat_mode": "All", "shuffled": true} {
		if gm[k] != v {
			t.Errorf("Glagol %s=%v want=%v", k, gm[k], v)
		}
	}
	for key, id := range map[string]string{"prev_track_info": r.PreviousTrackID, "next_track_info": r.NextTrackID} {
		track, ok := gm[key].(map[string]any)
		if !ok || track["id"] != id || track["type"] != "Track" {
			t.Errorf("Glagol %s must identify a native track: %v", key, gm[key])
		}
	}
	scenario := p["scenario_meta"].(map[string]any)
	if scenario["@scenario_name"] != "SoundCloud" || scenario["session_id"] != "session" || scenario["queue_item"] != nil || scenario["playback_context"] != nil {
		t.Fatal("foreign/missing scenario metadata")
	}
	for _, name := range []string{"started", "stopped", "finished", "failed"} {
		cb := p["callbacks"].(map[string]any)["on_"+name].(map[string]any)
		if cb["name"] != "quasar_soundcloud_"+name || cb["type"] != "server_action" || cb["ignore_answer"] != false || cb["is_led_silent"] != true {
			t.Errorf("callback response disabled or shape wrong: %v", cb)
		}
		for k, v := range soundCloudPayload() {
			if cb["payload"].(map[string]any)[k] != v {
				t.Errorf("callback %s mismatch", k)
			}
		}
	}
}

func TestNativeSoundCloudIdentityAndPlaybackState(t *testing.T) {
	for _, tc := range []struct{ device, uuid, want string }{{"speaker", "uuid", "device:speaker"}, {"", "uuid", "uuid:uuid"}, {"", "", ""}} {
		t.Run(tc.want, func(t *testing.T) {
			var scope string
			r := &soundCloudEventResolver{handle: func(ctx context.Context, _ string, _ map[string]any) (bool, error) {
				want := tc.want
				if want == "" {
					want = scope
				}
				if dialogue.SpeakerKey(ctx) != dialogue.SpeakerKey(dialogue.WithSpeaker(t.Context(), want)) {
					t.Error("wrong speaker identity")
				}
				state := sound.CurrentPlayback(ctx)
				if !state.Present || state.StreamID != "reported" || state.OffsetMS != 12345 || !state.OffsetKnown {
					t.Errorf("state=%+v", state)
				}
				return true, nil
			}}
			n, up, down := soundSession(t, r)
			scope = "connection:" + n.connectionScope
			frame := wire.Decode(soundCloudEventInput("event", "quasar_soundcloud_started", false, soundCloudPayload()), wire.ClientToServer)
			req := frame.Client.GetEvent().GetTextInput()
			req.Application = &app.TClientInfoProto{DeviceId: new(tc.device), Uuid: new(tc.uuid)}
			req.Request.DeviceStateType = &request.TAliceRequestProto_TRequest_DeviceState{DeviceState: &device.TDeviceState{AudioPlayer: &device.TDeviceState_TAudioPlayer{OffsetMs: new(int32(12345)), CurrentlyPlaying: &device.TDeviceState_TAudioPlayer_TCurrentlyPlaying{StreamId: new("reported")}}}}
			if err := n.Client(t.Context(), websocket.MessageBinary, aa(frame.Client)); err != nil {
				t.Fatal(err)
			}
			silentSoundCloudResponse(t, down)
			n.wg.Wait()
			if len(up) != 0 {
				t.Fatal("callback escaped")
			}
		})
	}
}

func TestNativeSoundCloudVoiceASRDeferredAndCallbackCycle(t *testing.T) {
	release := make(chan struct{})
	handled := new(atomic.Int32)
	track := sound.Request{URL: "http://localhost:8080/soundcloud/first.m3u8", StreamID: "stream", Format: "HLS", Kind: "Track", Provider: "SoundCloud", Title: "Title", Subtitle: "Artist", DurationMS: 120000, TrackID: "soundcloud:tracks:1", SessionID: "session", CallbackToken: "token"}
	provider := soundCloudStub{can: true, handle: func(ctx context.Context, text string) (string, error) {
		handled.Add(1)
		if text != "включи SoundCloud" {
			t.Error("ASR text lost")
		}
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "Включаю Title", sound.Play(ctx, track)
	}, event: func(ctx context.Context, _ string, _ map[string]any) (bool, error) {
		next := track
		next.StreamID = "next-stream"
		next.Title = "Next"
		return true, sound.Play(ctx, next)
	}}
	engine := &Engine{SoundCloud: provider}
	n, up, down := soundSession(t, engine)
	n.ProgressAfter = time.Millisecond
	voice := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "voice", StreamId: new(int64(1))}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_voice_input.Enum()}}, Format: new("audio/opus")}}}}})
	if err := n.Client(t.Context(), websocket.MessageBinary, voice); err != nil {
		t.Fatal(err)
	}
	if first := nextSoundFrame(t, up); !first.Client.GetEvent().GetVoiceInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
		t.Fatal("voice bypassed ASR approval")
	}
	if handled.Load() != 0 {
		t.Fatal("provider invoked before ASR")
	}
	result := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "voice"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("включи SoundCloud")}}}}}}})
	if err := n.Server(t.Context(), websocket.MessageBinary, result); err != nil {
		t.Fatal(err)
	}
	for {
		f := nextSoundFrame(t, up)
		if f.Kind() == "ContinueRequest" {
			t.Fatal("local music resumed cloud")
		}
		if f.Kind() == "TtsGenerate" {
			feedSpeech(t, n, f.Client.GetEvent().GetHeader().GetMessageId(), 72, false)
			break
		}
	}
	close(release)
	sawEOS, sawClose := false, false
	for {
		f := nextSoundFrame(t, down)
		if f.Kind() == "opaque" && len(f.Raw) > 31 && string(f.Raw[4:8]) == "OggS" && f.Raw[9]&4 != 0 && binary.BigEndian.Uint32(f.Raw[:4]) == 72 {
			sawEOS = true
		}
		if c := f.Server.GetStreamControl(); c != nil && c.GetClose() != nil {
			if !sawEOS {
				t.Fatal("close preceded EOS")
			}
			sawClose = true
		}
		if r := f.Server.GetEvent().GetAliceResponse(); r != nil {
			for _, d := range r.GetResponse().GetDirectives() {
				if d.GetName() == "audio_play" {
					t.Fatal("track before final deferred answer")
				}
			}
		}
		if r := f.Server.GetEvent().GetDeferredAliceResponse(); r != nil && r.GetIsLast() {
			if !sawClose || r.GetBaseResponse().GetShouldListen() {
				t.Fatal("focus not released before music")
			}
			ds := r.GetBaseResponse().GetDirectives()
			if len(ds) != 1 || ds[0].GetPayload().GetFields()["screen_type"].GetStringValue() != "Music" {
				t.Fatal("deferred SoundCloud metadata lost")
			}
			break
		}
	}
	n.wg.Wait()
	n.mu.Lock()
	p := n.requests["voice"]
	if !p.soundSealed {
		t.Error("original request not sealed")
	}
	p.cancel()
	p.canceled = true
	p.expires.Stop()
	delete(n.requests, p.id)
	n.mu.Unlock()
	for len(up) > 0 {
		f := <-up
		if f.Kind() == "TtsGenerate" || f.Kind() == "ContinueRequest" {
			t.Fatalf("extra upstream frame: %s", f.Kind())
		}
	}
	if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudEventInput("next", "quasar_soundcloud_finished", true, soundCloudPayload())); err != nil {
		t.Fatal(err)
	}
	next := silentSoundCloudResponse(t, down)
	push := soundCloudPushDirectives(t, down)
	n.wg.Wait()
	if len(next.GetResponse().GetDirectives()) != 0 || len(push) != 1 || len(up) != 0 || handled.Load() != 1 {
		t.Fatal("callback next cycle failed")
	}
}

func TestSoundCloudLongMixResumeUsesTrackDuration(t *testing.T) {
	const hour = int64(60 * 60 * 1000)
	base := sound.Request{URL: "http://localhost:8443/soundcloud/mix.m3u8", StreamID: "mix", Format: "HLS", Kind: "Track", Provider: "SoundCloud", Title: "Three hour mix", DurationMS: 3 * hour, OffsetMS: 2*hour + 30000, SessionID: "session", CallbackToken: "token"}
	for _, tc := range []struct {
		name             string
		offset, duration int64
		provider         string
		wantErr          bool
	}{
		{"long mix", base.OffsetMS, base.DurationMS, "SoundCloud", false},
		{"beyond duration", 4 * hour, 3 * hour, "SoundCloud", true},
		{"negative", -1, 3 * hour, "SoundCloud", true},
		{"24 hour bound", 25 * hour, 26 * hour, "SoundCloud", true},
		{"generic retains two hour limit", base.OffsetMS, base.DurationMS, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			req.OffsetMS = tc.offset
			req.DurationMS = tc.duration
			req.Provider = tc.provider
			d, err := makeSoundDirective(req)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err == nil && d.GetPayload().GetFields()["stream"].GetStructValue().GetFields()["offset_ms"].GetNumberValue() != float64(tc.offset) {
				t.Fatal("offset changed")
			}
		})
	}
}

func TestNativeSoundCloudFailureSpeechStaysLocal(t *testing.T) {
	n, up, down := soundSession(t, &Engine{SoundCloud: soundCloudStub{can: true, handle: func(context.Context, string) (string, error) { return "", errors.New("private upstream detail") }}})
	if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("failure")); err != nil {
		t.Fatal(err)
	}
	for {
		f := nextSoundFrame(t, down)
		if r := f.Server.GetEvent().GetAliceResponse(); r != nil {
			text := r.GetVoiceResponse().GetOutputSpeech().GetText()
			if !strings.Contains(text, "SoundCloud") || strings.Contains(text, "модел") || strings.Contains(text, "private") {
				t.Fatalf("incorrect failure speech: %s", text)
			}
			break
		}
	}
	n.wg.Wait()
	for len(up) > 0 {
		f := <-up
		if f.Kind() != "TtsGenerate" {
			t.Fatalf("failure escaped to cloud executor: %s", f.Kind())
		}
	}
}

func TestNativeSoundCloudRequestTrackingIsBounded(t *testing.T) {
	calls := new(atomic.Int32)
	r := &soundCloudEventResolver{handle: func(context.Context, string, map[string]any) (bool, error) { calls.Add(1); return true, nil }}
	sender := func(context.Context, websocket.MessageType, []byte) error { return nil }
	n := NewNative(t.Context(), sender, sender, r)
	t.Cleanup(n.Close)
	for range 130 {
		if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudEventInput(newID(), "quasar_soundcloud_started", false, soundCloudPayload())); err != nil {
			t.Fatal(err)
		}
	}
	n.wg.Wait()
	if len(n.requests) != 128 || calls.Load() != 128 {
		t.Fatalf("requests=%d calls=%d", len(n.requests), calls.Load())
	}
}

func soundCloudPushDirectives(t *testing.T, frames <-chan wire.Frame) []*soundDirective {
	t.Helper()
	f := nextSoundFrame(t, frames)
	push := f.Server.GetEvent().GetPush()
	if f.Kind() != "Push" || push == nil || f.Server.GetEvent().GetHeader().GetRefMessageId() != "" {
		t.Fatalf("expected unsolicited Push, got %s", f.Kind())
	}
	if len(push.GetPushIds()) != 1 || push.GetPushIds()[0] == "" {
		t.Fatal("Push lacks unique identity")
	}
	return push.GetDirectives()
}
