package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	app "github.com/skrashevich/quasar-go-genproto/proto/alice/library/client/protos"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/wire"
)

func soundCloudProtocolTestMarker() string {
	return "quasar-bridge/soundcloud/v1." + base64.RawURLEncoding.EncodeToString([]byte(`{"session_id":"session","stream_id":"stream","token":"token"}`))
}

func soundCloudProtocolTestCallback(marker string) map[string]any {
	return map[string]any{"@scenario_name": "HollywoodMusic", "@request_id": "issued", "callback_flow_type": "Old", "events": []any{map[string]any{"playAudioEvent": map[string]any{"context": marker, "contextItem": "soundcloud:tracks:1", "playId": "stream", "trackId": "soundcloud:tracks:1", "aliceRequestId": "issued"}}}}
}

func TestNativeSoundCloudProtocolCallbackNormalization(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "struct", true: "raw"}[raw], func(t *testing.T) {
			var calls atomic.Int32
			r := &soundCloudEventResolver{handle: func(ctx context.Context, name string, payload map[string]any) (bool, error) {
				calls.Add(1)
				if name != "music_thin_client_on_started" || payload["session_id"] != "session" || payload["stream_id"] != "stream" || payload["token"] != "token" || sound.RequestSequence(ctx) != 0 {
					t.Errorf("native callback was not normalized safely: %q %v", name, payload)
				}
				return true, nil
			}}
			n, up, down := soundSession(t, r)
			input := soundCloudEventInput("native-started", "music_thin_client_on_started", raw, soundCloudProtocolTestCallback(soundCloudProtocolTestMarker()))
			if err := n.Client(t.Context(), websocket.MessageBinary, input); err != nil {
				t.Fatal(err)
			}
			n.wg.Wait()
			if calls.Load() != 1 || len(up) != 0 {
				t.Fatal("own native callback escaped the musical server adapter")
			}
			silentSoundCloudResponse(t, down)
			if len(down) != 0 || r.prepared.Load() != 0 || r.delivered.Load() != 0 {
				t.Fatal("lifecycle generated playback or a conversational turn")
			}
		})
	}
}

func TestNativeSoundCloudProtocolForeignAndMalformedCallbacks(t *testing.T) {
	for _, tc := range []struct {
		name, marker string
		owned        bool
	}{
		{"foreign", "opaque-yandex-context", false},
		{"malformed-own", "quasar-bridge/soundcloud/v1.!", true},
		{"unknown-version-own", "quasar-bridge/soundcloud/v2.invalid", true},
		{"invalid-identity", "quasar-bridge/soundcloud/v1." + base64.RawURLEncoding.EncodeToString([]byte(`{"session_id":"session","stream_id":"","token":"token"}`)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &soundCloudEventResolver{handle: func(context.Context, string, map[string]any) (bool, error) {
				t.Error("unvalidated callback reached provider")
				return true, nil
			}}
			n, up, down := soundSession(t, r)
			input := soundCloudEventInput(tc.name, "music_thin_client_on_finished", false, soundCloudProtocolTestCallback(tc.marker))
			if err := n.Client(t.Context(), websocket.MessageBinary, input); err != nil {
				t.Fatal(err)
			}
			n.wg.Wait()
			if !tc.owned {
				if got := nextSoundFrame(t, up); string(got.Raw) != string(input) || len(down) != 0 {
					t.Fatal("foreign native callback changed")
				}
				return
			}
			if len(up) != 0 {
				t.Fatal("malformed own callback leaked to Yandex")
			}
			silentSoundCloudResponse(t, down)
		})
	}
}

func TestSoundCloudNativeProtocolCallbackDirectiveContract(t *testing.T) {
	r := sound.Request{URL: "https://audio.example/sc.m3u8", StreamID: "stream", Provider: "SoundCloud", SessionID: "session", CallbackToken: "token", TrackID: "soundcloud:tracks:1", DurationMS: 120000}
	d, err := makeSoundDirective(r)
	if err != nil {
		t.Fatal(err)
	}
	payload := d.GetPayload().AsMap()
	for _, action := range []string{"started", "stopped", "finished", "failed"} {
		cb := payload["callbacks"].(map[string]any)["on_"+action].(map[string]any)
		if cb["name"] != "music_thin_client_on_"+action || cb["ignore_answer"] != true || cb["is_led_silent"] != true || cb["type"] != "server_action" {
			t.Fatalf("callback does not use the native contract: %v", cb)
		}
		p := cb["payload"].(map[string]any)
		e := p["events"].([]any)[0].(map[string]any)["playAudioEvent"].(map[string]any)
		if p["@scenario_name"] != "HollywoodMusic" || p["callback_flow_type"] != "Old" || e["playId"] != r.StreamID || e["trackId"] != r.TrackID || !strings.HasPrefix(e["context"].(string), "quasar-bridge/soundcloud/v1.") {
			t.Fatal("native callback lost issued identity")
		}
		encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(e["context"].(string), "quasar-bridge/soundcloud/v1."))
		if err != nil {
			t.Fatal(err)
		}
		var identity map[string]any
		if json.Unmarshal(encoded, &identity) != nil || identity["session_id"] != r.SessionID || identity["stream_id"] != r.StreamID || identity["token"] != r.CallbackToken {
			t.Fatal("opaque context failed to roundtrip")
		}
	}
}

func TestNativeSoundCloudProtocolEscapedRawContextIsOwned(t *testing.T) {
	for _, escaped := range []string{`quasar-bridge\/soundcloud\/`, `quasar\u002dbridge\u002fsoundcloud\u002f`} {
		t.Run(escaped, func(t *testing.T) {
			var calls atomic.Int32
			r := &soundCloudEventResolver{handle: func(context.Context, string, map[string]any) (bool, error) {
				calls.Add(1)
				return true, nil
			}}
			n, up, down := soundSession(t, r)
			f := wire.Decode(soundCloudEventInput("escaped", "music_thin_client_on_started", true, soundCloudProtocolTestCallback(soundCloudProtocolTestMarker())), wire.ClientToServer)
			event := f.Client.GetEvent().GetTextInput().GetRequest().GetEvent()
			event.PayloadType = &common.TEvent_PayloadRaw{PayloadRaw: []byte(strings.ReplaceAll(string(event.GetPayloadRaw()), "quasar-bridge/soundcloud/", escaped))}
			if err := n.Client(t.Context(), websocket.MessageBinary, aa(f.Client)); err != nil {
				t.Fatal(err)
			}
			n.wg.Wait()
			if calls.Load() != 1 || len(up) != 0 {
				t.Fatal("escaped own context escaped to Yandex")
			}
			silentSoundCloudResponse(t, down)
		})
	}
}

type soundCloudWarmUpResolver struct {
	*soundCloudEventResolver
	prefetch  func(context.Context, string, string, string) (bool, error)
	delivered func(context.Context, sound.Request)
	dispatch  func(context.Context, func() error) error
	valid     func(context.Context, sound.Request) bool
}

func (r *soundCloudWarmUpResolver) PrefetchSoundCloud(ctx context.Context, session, stream, token string) (bool, error) {
	return r.prefetch(ctx, session, stream, token)
}
func (r *soundCloudWarmUpResolver) SoundCloudPrefetchDelivered(ctx context.Context, request sound.Request) {
	if r.delivered != nil {
		r.delivered(ctx, request)
	}
}
func (r *soundCloudWarmUpResolver) DispatchSoundCloud(ctx context.Context, send func() error) error {
	if r.dispatch != nil {
		return r.dispatch(ctx, send)
	}
	return send()
}
func (r *soundCloudWarmUpResolver) CanDeliver(ctx context.Context, request sound.Request) bool {
	return r.valid == nil || r.valid(ctx, request)
}

func soundCloudProtocolInput(id, name string, payload map[string]any, warm bool) []byte {
	f := wire.Decode(soundCloudEventInput(id, name, false, payload), wire.ClientToServer)
	r := f.Client.GetEvent().GetTextInput()
	r.Application = &app.TClientInfoProto{DeviceId: new("protocol-speaker")}
	r.Header = &request.TAliceRequestProto_THeader{RequestId: new("request-" + id)}
	r.Request.Event.IsWarmUp = new(warm)
	return aa(f.Client)
}

func TestNativeSoundCloudProtocolWarmUpWriteCommitsBeforeEOF(t *testing.T) {
	var prefetched, delivered atomic.Bool
	var eventCalls, dispatchCalls atomic.Int32
	responseEntered, lifecycleGateAttempted := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	gate := new(Engine)
	r := &soundCloudWarmUpResolver{soundCloudEventResolver: &soundCloudEventResolver{}}
	r.prefetch = func(ctx context.Context, session, stream, token string) (bool, error) {
		if session != "session" || stream != "stream" || token != "token" || sound.RequestSequence(ctx) != 0 {
			t.Error("warm-up lost issued queue identity or became a conversation")
		}
		if prefetched.Swap(true) {
			t.Error("duplicate warm-up was executed")
		}
		return true, sound.Play(ctx, sound.Request{URL: "https://audio.example/next.m3u8", Provider: "SoundCloud", SessionID: session, StreamID: "next", CallbackToken: "next-token", TrackID: "soundcloud:tracks:2", Prefetch: true, AfterStreamID: stream})
	}
	r.delivered = func(_ context.Context, next sound.Request) {
		if !next.Prefetch || next.AfterStreamID != "stream" || next.StreamID != "next" {
			t.Error("delivered hook lost staged identity")
		}
		delivered.Store(true)
	}
	r.handle = func(ctx context.Context, name string, _ map[string]any) (bool, error) {
		eventCalls.Add(1)
		if name != "music_thin_client_on_finished" {
			t.Error("unexpected lifecycle event", name)
		}
		if !delivered.Load() {
			return true, sound.Play(ctx, sound.Request{URL: "https://audio.example/duplicate.m3u8"})
		}
		return true, nil
	}
	r.dispatch = func(ctx context.Context, send func() error) error {
		if dispatchCalls.Add(1) == 2 {
			close(lifecycleGateAttempted)
		}
		return gate.DispatchSoundCloud(ctx, send)
	}
	warm, up, down := soundSession(t, r)
	peer, peerUp, peerDown := soundSession(t, r)
	originalDown := warm.down
	warm.down = func(ctx context.Context, kind websocket.MessageType, b []byte) error {
		f := wire.Decode(b, wire.ServerToClient)
		if f.Kind() == "AliceResponse" {
			close(responseEntered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return originalDown(ctx, kind, b)
	}
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	input := soundCloudProtocolInput("warm", "@@mm_stack_engine_get_next", map[string]any{"stack_session_id": soundCloudProtocolTestMarker()}, true)
	if err := warm.Client(t.Context(), websocket.MessageBinary, input); err != nil {
		t.Fatal(err)
	}
	select {
	case <-responseEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("warm-up did not reach its final response")
	}
	if err := warm.Client(t.Context(), websocket.MessageBinary, input); err != nil {
		t.Fatal(err)
	}
	if err := peer.Client(t.Context(), websocket.MessageBinary, soundCloudProtocolInput("eof", "music_thin_client_on_finished", soundCloudProtocolTestCallback(soundCloudProtocolTestMarker()), false)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lifecycleGateAttempted:
	case <-time.After(2 * time.Second):
		t.Fatal("EOF did not reach the shared speaker gate")
	}
	if eventCalls.Load() != 0 || delivered.Load() {
		t.Fatal("EOF mutated the queue before the warm-up write and delivered hook completed")
	}
	releaseOnce.Do(func() { close(release) })
	warm.wg.Wait()
	peer.wg.Wait()
	if f := nextSoundFrame(t, down); f.Kind() != "InputStartAck" {
		t.Fatal("missing correlated warm-up ACK")
	}
	f := nextSoundFrame(t, down)
	answer := f.Server.GetEvent().GetAliceResponse()
	ds := answer.GetResponse().GetDirectives()
	if answer == nil || f.Server.GetEvent().GetHeader().GetRefMessageId() != "warm" || answer.GetHeader().GetRequestId() != "request-warm" || len(ds) != 2 || ds[0].GetName() != "audio_play" || ds[1].GetName() != "@@mm_stack_engine_get_next" {
		t.Fatal("warm-up did not return a correlated deferred audio and continuation")
	}
	silentSoundCloudResponse(t, peerDown)
	if eventCalls.Load() != 1 || !delivered.Load() || len(down) != 0 || len(peerDown) != 0 || len(up) != 0 || len(peerUp) != 0 || r.prepared.Load() != 0 {
		t.Fatal("warm-up or EOF generated a duplicate Push or conversational input")
	}
}

func TestNativeSoundCloudProtocolWarmUpRevalidatesAtDelivery(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var valid, delivered atomic.Bool
	valid.Store(true)
	r := &soundCloudWarmUpResolver{soundCloudEventResolver: &soundCloudEventResolver{}, valid: func(context.Context, sound.Request) bool { return valid.Load() }, delivered: func(context.Context, sound.Request) { delivered.Store(true) }}
	r.prefetch = func(ctx context.Context, _, _, _ string) (bool, error) {
		if err := sound.Play(ctx, sound.Request{URL: "https://audio.example/next.m3u8", Provider: "SoundCloud", SessionID: "session", StreamID: "next", CallbackToken: "next-token", TrackID: "soundcloud:tracks:2", Prefetch: true, AfterStreamID: "stream"}); err != nil {
			return true, err
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return true, ctx.Err()
		}
		return true, nil
	}
	n, up, down := soundSession(t, r)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudProtocolInput("stale-warm", "@@mm_stack_engine_get_next", map[string]any{"stack_session_id": soundCloudProtocolTestMarker()}, true)); err != nil {
		t.Fatal(err)
	}
	<-entered
	valid.Store(false)
	releaseOnce.Do(func() { close(release) })
	n.wg.Wait()
	answer := silentSoundCloudResponse(t, down)
	if len(answer.GetResponse().GetDirectives()) != 0 || delivered.Load() || len(up) != 0 || len(down) != 0 {
		t.Fatal("superseded warm-up staged audio, marked delivery, or escaped")
	}
}

func TestNativeSoundCloudProtocolOversizedEscapedContextIsConsumed(t *testing.T) {
	r := &soundCloudEventResolver{handle: func(context.Context, string, map[string]any) (bool, error) {
		t.Error("oversized callback reached provider")
		return true, nil
	}}
	n, up, down := soundSession(t, r)
	payload := soundCloudProtocolTestCallback(soundCloudProtocolTestMarker())
	payload["padding"] = strings.Repeat("x", 65537)
	f := wire.Decode(soundCloudEventInput("large-escaped", "music_thin_client_on_started", true, payload), wire.ClientToServer)
	e := f.Client.GetEvent().GetTextInput().GetRequest().GetEvent()
	e.PayloadType = &common.TEvent_PayloadRaw{PayloadRaw: []byte(strings.ReplaceAll(string(e.GetPayloadRaw()), "quasar-bridge/soundcloud/", `quasar-bridge\/soundcloud\/`))}
	if err := n.Client(t.Context(), websocket.MessageBinary, aa(f.Client)); err != nil {
		t.Fatal(err)
	}
	n.wg.Wait()
	if len(up) != 0 {
		t.Fatal("oversized escaped own marker leaked to Yandex")
	}
	silentSoundCloudResponse(t, down)
}

func TestNativeSoundCloudObsoleteCallbackIsNotAdmitted(t *testing.T) {
	var calls atomic.Int32
	r := &soundCloudEventResolver{handle: func(context.Context, string, map[string]any) (bool, error) {
		calls.Add(1)
		return true, nil
	}}
	n, up, down := soundSession(t, r)
	obsoleteName := "quasar_soundcloud_finished"
	input := soundCloudEventInput("obsolete", obsoleteName, false, soundCloudPayload())
	if err := n.Client(t.Context(), websocket.MessageBinary, input); err != nil {
		t.Fatal(err)
	}
	n.wg.Wait()
	select {
	case frame := <-up:
		if string(frame.Raw) != string(input) {
			t.Fatal("obsolete callback was modified")
		}
	default:
		t.Fatal("obsolete callback was admitted")
	}
	if calls.Load() != 0 || len(down) != 0 {
		t.Fatal("obsolete callback reached the musical server")
	}
}

func TestNativeSoundCloudUnmarkedMalformedEventsPassThrough(t *testing.T) {
	for _, raw := range []string{`[]`, `{`, `{"events":[]}`, strings.Repeat(" ", 65537)} {
		t.Run(raw[:min(len(raw), 20)], func(t *testing.T) {
			r := &soundCloudEventResolver{handle: func(context.Context, string, map[string]any) (bool, error) {
				t.Error("unmarked malformed event reached provider")
				return true, nil
			}}
			n, up, down := soundSession(t, r)
			f := wire.Decode(soundCloudEventInput("unmarked", "music_thin_client_on_started", false, nil), wire.ClientToServer)
			f.Client.GetEvent().GetTextInput().GetRequest().GetEvent().PayloadType = &common.TEvent_PayloadRaw{PayloadRaw: []byte(raw)}
			input := aa(f.Client)
			if err := n.Client(t.Context(), websocket.MessageBinary, input); err != nil {
				t.Fatal(err)
			}
			n.wg.Wait()
			if got := nextSoundFrame(t, up); string(got.Raw) != string(input) || len(down) != 0 {
				t.Fatal("unmarked malformed native event was claimed or modified")
			}
		})
	}
}
