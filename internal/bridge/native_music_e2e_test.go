package bridge

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/coder/websocket"
	app "github.com/skrashevich/quasar-go-genproto/proto/alice/library/client/protos"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	media "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/scenarios/frames/media_frame"
	device "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/common/device_state"
	directive "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	"google.golang.org/protobuf/proto"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/soundcloud"
	"quasarbridge/internal/wire"
)

// Exercise the actual provider, relay and wire server together. A warm-up must
// stage the next track without advancing the current one or sending a Push.
func TestNativeMusicServerQueueRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			fmt.Fprint(w, "fixture-cover")
		case strings.HasSuffix(r.URL.Path, "/streams"):
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/fixture.m3u8"}`)
		default:
			fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist","artwork_url":"https://i1.sndcdn.com/first.jpg"},{"id":2,"title":"Second","duration":120000,"metadata_artist":"Artist","artwork_url":"https://i1.sndcdn.com/second.jpg"}]}`)
		}
	}))
	t.Cleanup(upstream.Close)
	origin, _ := url.Parse(upstream.URL)
	hc := &http.Client{Transport: soundCloudHTTPTransport(func(r *http.Request) (*http.Response, error) {
		req := r.Clone(r.Context())
		req.URL = r.URL.Clone()
		req.URL.Scheme = origin.Scheme
		req.URL.Host = origin.Host
		return http.DefaultTransport.RoundTrip(req)
	})}
	var relay *soundcloud.Relay
	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { relay.ServeHTTP(w, r) }))
	t.Cleanup(relayServer.Close)
	var err error
	relay, err = soundcloud.NewRelay(relayServer.URL, hc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := soundcloud.Config{Enabled: true, Default: true, API: "official", AccessToken: "fixture"}
	catalog, err := soundcloud.NewClient(cfg, hc)
	if err != nil {
		t.Fatal(err)
	}
	provider := soundcloud.NewProvider(cfg, catalog, relay)
	n, up, down := soundSession(t, &Engine{SoundCloud: provider, Dialogue: dialogue.New(dialogue.DefaultConfig())})
	input := wire.Decode(soundCloudTextInput("initial", "включи First SoundCloud"), wire.ClientToServer)
	input.Client.GetEvent().GetTextInput().Application = &app.TClientInfoProto{DeviceId: new("native-e2e")}
	if err := n.Client(t.Context(), websocket.MessageBinary, aa(input.Client)); err != nil {
		t.Fatal(err)
	}
	initial := finalSoundDirectives(t, down)
	if len(initial) != 2 || initial[0].GetName() != "audio_play" || initial[1].GetName() != "@@mm_stack_engine_get_next" {
		t.Fatalf("native initial queue directives=%v", directiveNames(initial))
	}
	first := initial[0].GetPayload().AsMap()
	firstID := first["stream"].(map[string]any)["id"].(string)
	callbacks := first["callbacks"].(map[string]any)
	started := callbacks["on_started"].(map[string]any)
	if started["name"] != "music_thin_client_on_started" || started["ignore_answer"] != true {
		t.Fatal("non-native lifecycle contract")
	}

	send := func(id, name string, payload map[string]any, stream, state string, offset int32, warm bool) {
		t.Helper()
		b := nativeMusicRoundTripInput(id, name, payload, stream, state, offset, warm)
		if err := n.Client(t.Context(), websocket.MessageBinary, b); err != nil {
			t.Fatal(err)
		}
		n.wg.Wait()
	}
	send("started", started["name"].(string), started["payload"].(map[string]any), firstID, "Playing", 0, false)
	silentSoundCloudResponse(t, down)
	if len(down) != 0 {
		t.Fatal("started unexpectedly issued playback")
	}

	send("warmup", "@@mm_stack_engine_get_next", initial[1].GetPayload().AsMap(), firstID, "Playing", 30000, true)
	if f := nextSoundFrame(t, down); f.Kind() != "InputStartAck" {
		t.Fatalf("warmup ack=%s", f.Kind())
	}
	f := nextSoundFrame(t, down)
	response := f.Server.GetEvent().GetAliceResponse()
	if response == nil || f.Server.GetEvent().GetHeader().GetRefMessageId() != "warmup" {
		t.Fatal("warm-up reply correlation lost")
	}
	staged := response.GetResponse().GetDirectives()
	if len(staged) != 2 || staged[0].GetName() != "audio_play" || staged[1].GetName() != "@@mm_stack_engine_get_next" {
		t.Fatalf("warmup directives=%v", directiveNames(staged))
	}
	second := staged[0].GetPayload().AsMap()
	secondID := second["stream"].(map[string]any)["id"].(string)
	if secondID == firstID || second["metadata"].(map[string]any)["title"] != "Second" || len(down) != 0 {
		t.Fatal("prefetch restarted current audio or returned wrong track")
	}
	ctx := sound.WithPlaybackState(dialogue.WithSpeaker(t.Context(), "device:native-e2e"), sound.PlaybackState{Present: true, StreamID: firstID, PlayerState: "Playing", OffsetKnown: true, OffsetMS: 30000})
	info, err := provider.Handle(ctx, "что играет в SoundCloud")
	if err != nil || !strings.Contains(info, "First") {
		t.Fatalf("warmup advanced queue early: %q %v", info, err)
	}

	art := second["metadata"].(map[string]any)["art_image_url"].(string)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, art, nil)
	cover, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(cover.Body)
	cover.Body.Close()
	if err != nil || cover.StatusCode != 200 || cover.Header.Get("Content-Type") != "image/jpeg" || string(body) != "fixture-cover" {
		t.Fatal("next-track artwork relay unavailable")
	}

	stopped := callbacks["on_stopped"].(map[string]any)
	send("eof", stopped["name"].(string), stopped["payload"].(map[string]any), firstID, "Stopped", 120000, false)
	silentSoundCloudResponse(t, down)
	if len(down) != 0 {
		t.Fatal("EOF pushed a duplicate while next audio was already staged")
	}
	secondStarted := second["callbacks"].(map[string]any)["on_started"].(map[string]any)
	send("next-started", secondStarted["name"].(string), secondStarted["payload"].(map[string]any), secondID, "Playing", 0, false)
	silentSoundCloudResponse(t, down)
	if len(down) != 0 {
		t.Fatal("staged start unexpectedly restarted playback")
	}
	ctx = sound.WithPlaybackState(dialogue.WithSpeaker(t.Context(), "device:native-e2e"), sound.PlaybackState{Present: true, StreamID: secondID, PlayerState: "Playing"})
	info, err = provider.Handle(ctx, "что играет в SoundCloud")
	if err != nil || !strings.Contains(info, "Second") {
		t.Fatalf("native start failed to promote next track: %q %v", info, err)
	}

	send("queue-end", "@@mm_stack_engine_get_next", staged[1].GetPayload().AsMap(), secondID, "Playing", 30000, true)
	end := silentSoundCloudResponse(t, down)
	if len(end.GetResponse().GetDirectives()) != 0 || len(down) != 0 {
		t.Fatal("last track produced nonexistent next audio")
	}
	raw, err := proto.Marshal(&common.TSemanticFrameRequestData{TypedSemanticFrame: &common.TTypedSemanticFrame{Type: &common.TTypedSemanticFrame_PlayerPrevTrackSemanticFrame{PlayerPrevTrackSemanticFrame: &media.TPlayerPrevTrackSemanticFrame{}}}})
	if err != nil {
		t.Fatal(err)
	}
	prev := wire.Decode(nativeMusicRoundTripInput("previous", "@@mm_semantic_frame", nil, secondID, "Playing", 30000, false), wire.ClientToServer)
	prev.Client.GetEvent().GetTextInput().Request.Event.PayloadType = &common.TEvent_PayloadRaw{PayloadRaw: raw}
	if err := n.Client(t.Context(), websocket.MessageBinary, aa(prev.Client)); err != nil {
		t.Fatal(err)
	}
	n.wg.Wait()
	silentSoundCloudResponse(t, down)
	push := soundCloudPushDirectives(t, down)
	if len(push) != 2 || push[0].GetPayload().AsMap()["metadata"].(map[string]any)["title"] != "First" {
		t.Fatal("structured previous lost promoted native queue")
	}
	// A follow-up control can arrive before the just-issued track has started.
	// Its issued stream identity still belongs to this queue while loading.
	loadingID := push[0].GetPayload().AsMap()["stream"].(map[string]any)["id"].(string)
	raw, err = proto.Marshal(&common.TSemanticFrameRequestData{TypedSemanticFrame: &common.TTypedSemanticFrame{Type: &common.TTypedSemanticFrame_PlayerNextTrackSemanticFrame{PlayerNextTrackSemanticFrame: &media.TPlayerNextTrackSemanticFrame{}}}})
	if err != nil {
		t.Fatal(err)
	}
	loading := wire.Decode(nativeMusicRoundTripInput("loading-next", "@@mm_semantic_frame", nil, loadingID, "Idle", 0, false), wire.ClientToServer)
	loading.Client.GetEvent().GetTextInput().Request.Event.PayloadType = &common.TEvent_PayloadRaw{PayloadRaw: raw}
	if err := n.Client(t.Context(), websocket.MessageBinary, aa(loading.Client)); err != nil {
		t.Fatal(err)
	}
	n.wg.Wait()
	if len(up) != 0 {
		t.Fatal("a native control for an owned loading track escaped to Yandex")
	}
	silentSoundCloudResponse(t, down)
	loadingPush := soundCloudPushDirectives(t, down)
	if len(loadingPush) != 2 || loadingPush[0].GetPayload().AsMap()["metadata"].(map[string]any)["title"] != "Second" {
		t.Fatal("native control did not advance the loading queue")
	}
	if len(up) != 0 {
		t.Fatal("owned music protocol leaked to Yandex")
	}
}

func nativeMusicRoundTripInput(id, name string, payload map[string]any, stream, state string, offset int32, warm bool) []byte {
	f := wire.Decode(soundCloudEventInput(id, name, false, payload), wire.ClientToServer)
	r := f.Client.GetEvent().GetTextInput()
	r.Application = &app.TClientInfoProto{DeviceId: new("native-e2e")}
	r.Request.Event.IsWarmUp = new(warm)
	ps := device.TDeviceState_TAudioPlayer_Playing
	if state == "Stopped" {
		ps = device.TDeviceState_TAudioPlayer_Stopped
	} else if state == "Idle" {
		ps = device.TDeviceState_TAudioPlayer_Idle
	}
	r.Request.DeviceStateType = &request.TAliceRequestProto_TRequest_DeviceState{DeviceState: &device.TDeviceState{AudioPlayer: &device.TDeviceState_TAudioPlayer{PlayerState: ps.Enum(), OffsetMs: new(offset), CurrentlyPlaying: &device.TDeviceState_TAudioPlayer_TCurrentlyPlaying{StreamId: new(stream)}}}}
	return aa(f.Client)
}
func directiveNames(ds []*directive.TDirective) []string {
	var names []string
	for _, d := range ds {
		names = append(names, d.GetName())
	}
	return names
}
