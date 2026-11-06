package bridge

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/soundcloud"
	"quasarbridge/internal/wire"
)

func TestNativeImplicitSongUsesObservedArtistAfterQueueEnded(t *testing.T) {
	var mu sync.Mutex
	queries := map[string]bool{}
	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/streams") {
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/fixture.m3u8"}`)
			return
		}
		q := r.URL.Query().Get("q")
		mu.Lock()
		queries[q] = true
		mu.Unlock()
		switch q {
		case "гусары":
			fmt.Fprint(w, `{"collection":[{"id":1,"title":"Гусары","duration":170000,"metadata_artist":"svk"}]}`)
		case "нас нет":
			fmt.Fprint(w, `{"collection":[{"id":2,"title":"Нас нет","duration":180000,"metadata_artist":"Other"}]}`)
		case "нас нет svk":
			fmt.Fprint(w, `{"collection":[{"id":3,"title":"Нас нет","duration":190000,"metadata_artist":"svk"}]}`)
		default:
			t.Errorf("unexpected catalog query %q", q)
			fmt.Fprint(w, `{"collection":[]}`)
		}
	}))
	t.Cleanup(catalogServer.Close)
	origin, _ := url.Parse(catalogServer.URL)
	hc := &http.Client{Transport: soundCloudHTTPTransport(func(r *http.Request) (*http.Response, error) {
		req := r.Clone(r.Context())
		req.URL = r.URL.Clone()
		req.URL.Scheme = origin.Scheme
		req.URL.Host = origin.Host
		return http.DefaultTransport.RoundTrip(req)
	})}
	cfg := soundcloud.Config{Enabled: true, CompareYandex: true, API: "official", AccessToken: "fixture"}
	catalog, err := soundcloud.NewClient(cfg, hc)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := soundcloud.NewRelay(catalogServer.URL, hc)
	if err != nil {
		t.Fatal(err)
	}
	provider := soundcloud.NewProvider(cfg, catalog, relay)
	var seed sound.Request
	seedCtx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "device:implicit-artist"), func(_ context.Context, r sound.Request) error { seed = r; return nil })
	if _, err := provider.Handle(seedCtx, "включи песню гусары на SoundCloud"); err != nil || seed.StreamID == "" {
		t.Fatal("seed failed", err)
	}
	identity := map[string]any{"session_id": seed.SessionID, "stream_id": seed.StreamID, "token": seed.CallbackToken}
	playing := sound.WithPlaybackState(seedCtx, sound.PlaybackState{Present: true, StreamID: seed.StreamID, PlayerState: "Playing"})
	provider.HandleEvent(playing, "music_thin_client_on_started", identity)
	stopped := sound.WithPlaybackState(seedCtx, sound.PlaybackState{Present: true, StreamID: seed.StreamID, PlayerState: "Stopped", OffsetKnown: true, OffsetMS: 170000})
	provider.HandleEvent(stopped, "music_thin_client_on_stopped", identity)
	var classified atomic.Int32
	e := &Engine{SoundCloud: provider, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		classified.Add(1)
		return `{"route":"yandex"}`, nil
	})}
	n, up, down := soundSession(t, e)
	// A no-audio clarification waits for the comparison deadline.
	n.ComparisonTimeout = 100 * time.Millisecond
	input := wire.Decode(soundCloudTextInput("implicit-artist", "включи нас нет"), wire.ClientToServer)
	req := input.Client.GetEvent().GetTextInput()
	req.Application = &app.TClientInfoProto{DeviceId: new("implicit-artist")}
	req.Request.DeviceStateType = &request.TAliceRequestProto_TRequest_DeviceState{DeviceState: &device.TDeviceState{AudioPlayer: &device.TDeviceState_TAudioPlayer{PlayerState: device.TDeviceState_TAudioPlayer_Stopped.Enum(), OffsetMs: new(int32(170000)), CurrentlyPlaying: &device.TDeviceState_TAudioPlayer_TCurrentlyPlaying{StreamId: new(seed.StreamID)}}}}
	if err := n.Client(t.Context(), websocket.MessageBinary, aa(input.Client)); err != nil {
		t.Fatal(err)
	}
	nextSoundFrame(t, up)
	clarification := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "implicit-artist"}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new("Что именно нужно включить?")}}}}}}})
	if err := n.Server(t.Context(), websocket.MessageBinary, clarification); err != nil {
		t.Fatal(err)
	}
	ds := finalSoundDirectives(t, down)
	if len(ds) != 2 || ds[0].GetName() != "audio_play" || ds[1].GetName() != "@@mm_stack_engine_get_next" {
		t.Fatalf("implicit music did not replace clarification with native queue: %v", directiveNames(ds))
	}
	md := ds[0].GetPayload().AsMap()["metadata"].(map[string]any)
	if md["title"] != "Нас нет" || md["subtitle"] != "svk" {
		t.Fatalf("wrong same-title artist: %v", md)
	}
	n.wg.Wait()
	mu.Lock()
	both := queries["нас нет"] && queries["нас нет svk"]
	mu.Unlock()
	if classified.Load() != 1 || !both {
		t.Fatal("implicit comparison bypassed classification or artist lookup", classified.Load(), both)
	}
}

func TestNativeImplicitVoiceKeepsPausedOwnTrackWhenArtistLookupTimesOut(t *testing.T) {
	for _, ownExact := range []bool{true, false} {
		t.Run(map[bool]string{true: "paused-current-exact", false: "no-own-exact-preserves-yandex"}[ownExact], func(t *testing.T) {
			seedQuery, seedTitle := "нас нет svk", "Нас нет"
			if !ownExact {
				seedQuery, seedTitle = "гусары", "Гусары"
			}
			var liveLookup atomic.Bool
			var global, scoped, canceled, foreignStreams, classified atomic.Int32
			hc := &http.Client{Transport: soundCloudHTTPTransport(func(r *http.Request) (*http.Response, error) {
				var body string
				if strings.HasSuffix(r.URL.Path, "/streams") {
					track := strings.TrimSuffix(r.URL.Path, "/streams")
					if strings.HasSuffix(track, ":2") || strings.HasSuffix(track, "/2") {
						foreignStreams.Add(1)
					}
					body = `{"hls_aac_160_url":"https://cf-media.sndcdn.com/fixture.m3u8"}`
				} else {
					q := r.URL.Query().Get("q")
					if !liveLookup.Load() {
						if q != seedQuery {
							t.Error("wrong seed query", q)
						}
						body = fmt.Sprintf(`{"collection":[{"id":1,"title":%q,"duration":170000,"metadata_artist":"svk"}]}`, seedTitle)
					} else {
						switch q {
						case "нас нет":
							global.Add(1)
							body = `{"collection":[{"id":2,"title":"Нас нет","duration":180000,"metadata_artist":"Other"}]}`
						case "нас нет svk":
							scoped.Add(1)
							if r.URL.Query().Get("limit") != "10" {
								t.Error("scoped lookup did not use its bounded limit", r.URL.Query().Get("limit"))
							}
							if _, ok := r.Context().Deadline(); !ok {
								t.Error("scoped query has no cancellation deadline")
							}
							<-r.Context().Done()
							canceled.Add(1)
							return nil, r.Context().Err()
						default:
							t.Error("unexpected comparison query", q)
							body = `{"collection":[]}`
						}
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			cfg := soundcloud.Config{Enabled: true, CompareYandex: true, API: "official", AccessToken: "fixture"}
			catalog, err := soundcloud.NewClient(cfg, hc)
			if err != nil {
				t.Fatal(err)
			}
			relay, err := soundcloud.NewRelay("http://bridge.invalid", hc)
			if err != nil {
				t.Fatal(err)
			}
			provider := soundcloud.NewProvider(cfg, catalog, relay)
			var seed sound.Request
			seedCtx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "device:implicit-voice-artist"), func(_ context.Context, r sound.Request) error { seed = r; return nil })
			if answer, err := provider.Handle(seedCtx, "включи песню "+seedQuery+" на SoundCloud"); err != nil || seed.StreamID == "" {
				t.Fatal("seed failed", answer, err)
			}
			identity := map[string]any{"session_id": seed.SessionID, "stream_id": seed.StreamID, "token": seed.CallbackToken}
			playing := sound.WithPlaybackState(seedCtx, sound.PlaybackState{Present: true, StreamID: seed.StreamID, PlayerState: "Playing"})
			if _, err := provider.HandleEvent(playing, nativeMusicCallbackPrefix+"started", identity); err != nil {
				t.Fatal(err)
			}
			paused := sound.WithPlaybackState(seedCtx, sound.PlaybackState{Present: true, StreamID: seed.StreamID, PlayerState: "Paused", OffsetKnown: true, OffsetMS: 12000})
			if _, err := provider.HandleEvent(paused, nativeMusicCallbackPrefix+"stopped", identity); err != nil {
				t.Fatal(err)
			}
			liveLookup.Store(true)
			e := &Engine{SoundCloud: provider, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
				classified.Add(1)
				return `{"route":"yandex"}`, nil
			})}
			n, up, down := soundSession(t, e)
			n.ComparisonTimeout = 300 * time.Millisecond
			input := wire.Decode(soundCloudTextInput("implicit-voice", ""), wire.ClientToServer)
			incoming := input.Client.GetEvent()
			req := incoming.GetTextInput()
			req.Application = &app.TClientInfoProto{DeviceId: new("implicit-voice-artist")}
			req.Format = new("audio/opus")
			req.Request.Event.Type = common.EEventType_voice_input.Enum()
			req.Request.DeviceStateType = &request.TAliceRequestProto_TRequest_DeviceState{DeviceState: &device.TDeviceState{AudioPlayer: &device.TDeviceState_TAudioPlayer{PlayerState: device.TDeviceState_TAudioPlayer_Paused.Enum(), OffsetMs: new(int32(12000)), CurrentlyPlaying: &device.TDeviceState_TAudioPlayer_TCurrentlyPlaying{StreamId: new(seed.StreamID)}}}}
			incoming.Header.StreamId = new(int64(1))
			incoming.Event = &client.TClientMessage_TEvent_VoiceInput{VoiceInput: req}
			if err := n.Client(t.Context(), websocket.MessageBinary, aa(input.Client)); err != nil {
				t.Fatal(err)
			}
			if first := nextSoundFrame(t, up); first.Kind() != "VoiceInput" || !first.Client.GetEvent().GetVoiceInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
				t.Fatal("microphone did not wait for recognition approval")
			}
			if classified.Load() != 0 || global.Load() != 0 || scoped.Load() != 0 {
				t.Fatal("bare-title lookup or classification started before ASR")
			}
			result := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "implicit-voice"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("Включи нас нет")}}}}}}})
			if err := n.Server(t.Context(), websocket.MessageBinary, result); err != nil {
				t.Fatal(err)
			}
			if f := nextSoundFrame(t, down); f.Kind() != "AsrResult" {
				t.Fatal("recognition was not preserved")
			}
			if f := nextSoundFrame(t, up); f.Kind() != "ContinueRequest" {
				t.Fatal("recognized title did not start its parallel Yandex branch")
			}
			clarification := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "implicit-voice"}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new("Что именно нужно включить?")}}}}}}})
			if err := n.Server(t.Context(), websocket.MessageBinary, clarification); err != nil {
				t.Fatal(err)
			}
			if ownExact {
				ds := finalSoundDirectives(t, down)
				if len(ds) != 2 || ds[0].GetName() != "audio_play" || ds[1].GetName() != nativeMusicGetNext {
					t.Fatal("paused own exact candidate did not enter the native queue")
				}
				md := ds[0].GetPayload().GetFields()["metadata"].GetStructValue().GetFields()
				if md["title"].GetStringValue() != "Нас нет" || md["subtitle"].GetStringValue() != "svk" {
					t.Fatal("timed-out artist search selected a foreign homonym")
				}
			} else if preserved := nextSoundFrame(t, down); string(preserved.Raw) != string(clarification) {
				t.Fatal("failed artist lookup replaced Yandex without an own exact candidate")
			}
			n.wg.Wait()
			if classified.Load() != 1 || global.Load() != 1 || scoped.Load() != 1 || canceled.Load() != 1 || foreignStreams.Load() != 0 || len(down) != 0 {
				t.Fatalf("lookup boundary: classifier=%d global=%d scoped=%d canceled=%d foreign-streams=%d downstream=%d", classified.Load(), global.Load(), scoped.Load(), canceled.Load(), foreignStreams.Load(), len(down))
			}
			for len(up) != 0 {
				if extra := <-up; !ownExact || extra.Kind() != "CancelRequest" {
					t.Fatal("voice comparison delegated or executed more than one branch", extra.Kind())
				}
			}
		})
	}
}
