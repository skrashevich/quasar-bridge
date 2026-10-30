package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"quasarbridge/internal/journal"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	directives "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	asr "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	cancellation "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/cancel_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	continuation "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/continue_request"
	spotter "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/spotter_validation"
	tts "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

type resolverFunc func(context.Context, string) (Resolution, error)

func (f resolverFunc) Resolve(ctx context.Context, s string) (Resolution, error) { return f(ctx, s) }
func aa(m proto.Message) []byte {
	data, _ := proto.Marshal(m)
	return append([]byte("AAPI"), data...)
}

func TestHomePipelineListenOverride(t *testing.T) {
	stop, keep := false, true
	for _, tc := range []struct {
		name    string
		session bool
		listen  *bool
		want    bool
	}{
		{"assist stops", true, &stop, false},
		{"assist continues", false, &keep, true},
		{"other local reply", true, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listen := tc.listen
			n, _, down := soundSession(t, resolverFunc(func(context.Context, string) (Resolution, error) {
				return Resolution{Route: "home", Answer: "Свет включён.", Listen: listen}, nil
			}))
			n.ShouldListen = tc.session
			if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("home-listen")); err != nil {
				t.Fatal(err)
			}
			for {
				f := nextSoundFrame(t, down)
				if r := f.Server.GetEvent().GetAliceResponse(); r != nil {
					if got := r.GetVoiceResponse().GetShouldListen(); got != tc.want {
						t.Fatalf("should_listen=%v, want %v", got, tc.want)
					}
					break
				}
			}
		})
	}
}

func TestNativeListenAfterReply(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  *bool
		want bool
	}{
		{"default", nil, true},
		{"enabled", new(true), true},
		{"disabled", new(false), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, _, down := soundSession(t, resolverFunc(func(context.Context, string) (Resolution, error) {
				return Resolution{Route: "llm", Answer: "Готово."}, nil
			}))
			if tc.set != nil {
				n.ShouldListen = *tc.set
			}
			input := wire.Decode(soundInput("follow-up"), wire.ClientToServer).Client
			e := input.GetEvent()
			e.Event = &client.TClientMessage_TEvent_VoiceInput{VoiceInput: e.GetTextInput()}
			if err := n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
				t.Fatal(err)
			}
			for {
				f := nextSoundFrame(t, down)
				if r := f.Server.GetEvent().GetAliceResponse(); r != nil {
					if got := r.GetVoiceResponse().GetShouldListen(); got != tc.want {
						t.Fatalf("should_listen=%v, want %v", got, tc.want)
					}
					break
				}
			}
		})
	}
}

func TestNativeRoutesBeforeCloudExecutionAndCorrelatesTTS(t *testing.T) {
	for _, route := range []string{"yandex", "llm", "no_continue", "home_disabled"} {
		t.Run(route, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			requestID := "request-17"
			cont := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "continue-17"}, Event: &client.TClientMessage_TEvent_ContinueRequest{ContinueRequest: &continuation.TContinueRequest{RequestMessageId: requestID}}}}})
			cont = protowire.AppendTag(cont, 999, protowire.BytesType)
			cont = protowire.AppendString(cont, "future-control-field")
			errCh := make(chan string, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer ws.CloseNow()
				for {
					_, data, err := ws.Read(ctx)
					if err != nil {
						return
					}
					f := wire.Decode(data, wire.ClientToServer)
					switch f.Kind() {
					case "TextInput":
						if route == "llm" {
							errCh <- "local request reached cloud"
							return
						}
						if f.Client.GetEvent().GetTextInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
							errCh <- "cloud input changed"
							return
						}
						msg := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: "cloud-answer", RefMessageId: requestID}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new("Ответ Яндекса")}}}}}}}
						_ = ws.Write(ctx, websocket.MessageBinary, aa(msg))
					case "ContinueRequest":
						if !bytes.Equal(data, cont) {
							errCh <- "original continuation changed"
							return
						}

					case "TtsGenerate":
						if route != "llm" || f.Client.GetEvent().GetTtsGenerate().GetText() != "Ответ своей модели" {
							errCh <- "wrong TTS text"
							return
						}
						id := f.Client.GetEvent().GetHeader().GetMessageId()
						if len(id) != 35 || id[8] != '-' || id[17] != '-' || id[26] != '-' {
							errCh <- "invalid Yandex GUID"
							return
						}
						msg := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: "tts-answer", RefMessageId: id, StreamId: new(int64(72))}, Event: &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &tts.TTtsSpeak{Format: "audio/opus"}}}}}
						_ = ws.Write(ctx, websocket.MessageBinary, aa(msg))
						audio := binary.BigEndian.AppendUint32(nil, 72)
						audio = append(audio, []byte("fixture-audio")...)
						_ = ws.Write(ctx, websocket.MessageBinary, audio)
					}
				}
			}))
			defer upstream.Close()
			var resolver Resolver = resolverFunc(func(ctx context.Context, text string) (Resolution, error) {
				if text != "Тестовый вопрос" {
					return Resolution{}, context.Canceled
				}
				if route == "no_continue" {
					return Resolution{Route: "yandex"}, nil
				}
				return Resolution{Route: route, Answer: "Ответ своей модели"}, nil
			})
			if route == "home_disabled" {
				engine := new(Engine)
				cfg := DefaultRoutingConfig()
				cfg.DefaultRoute = "home"
				if err := engine.SetRouting(cfg); err != nil {
					t.Fatal(err)
				}
				resolver = engine
			}
			log := new(journal.Writer)
			defer log.Close()
			logConfig := journal.DefaultConfig()
			logConfig.Enabled = true
			logConfig.Path = filepath.Join(t.TempDir(), "turns.jsonl")
			if err := log.Configure(logConfig); err != nil {
				t.Fatal(err)
			}
			proxy := &Proxy{Journal: log, Upstream: "ws" + strings.TrimPrefix(upstream.URL, "http"), NewSession: func(ctx context.Context, up, down Sender) Session {
				n := NewNative(ctx, up, down, resolver)

				return n
			}}
			s := httptest.NewServer(proxy)
			defer s.Close()
			ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ws.CloseNow()
			input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: requestID}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_text_input.Enum(), Text: new("Тестовый вопрос")}}}}}}}
			_ = ws.Write(ctx, websocket.MessageBinary, aa(input))
			if route != "no_continue" {
				_ = ws.Write(ctx, websocket.MessageBinary, cont)
			}
			gotAnswer, gotTTS, gotAudio := false, false, false
			for !gotAnswer || (route == "llm" && (!gotTTS || !gotAudio)) {
				_, data, err := ws.Read(ctx)
				if err != nil {
					select {
					case msg := <-errCh:
						t.Fatal(msg)
					default:
						t.Fatal(err)
					}
				}
				f := wire.Decode(data, wire.ServerToClient)
				if f.Kind() == "AliceResponse" {
					gotAnswer = true
					if got := f.Server.GetEvent().GetAliceResponse().GetVoiceResponse().GetShouldListen(); got != (route == "llm") {
						t.Fatalf("should_listen=%v for route %s", got, route)
					}
					text := f.Server.GetEvent().GetAliceResponse().GetVoiceResponse().GetOutputSpeech().GetText()
					want := "Ответ своей модели"
					if route != "llm" {
						want = "Ответ Яндекса"
					}
					if text != want {
						t.Fatalf("answer=%q", text)
					}
				}
				if f.Kind() == "TtsSpeak" {
					gotTTS = true
					if f.Server.GetEvent().GetHeader().GetRefMessageId() != requestID {
						t.Fatal("TTS correlated to internal request instead of speaker request")
					}
				}
				if len(data) > 4 && binary.BigEndian.Uint32(data) == 72 {
					gotAudio = true
					if string(data[4:]) != "fixture-audio" {
						t.Fatal("audio altered")
					}
				}
			}
			// The actual downstream sender must record both cloud and generated replies.
			var logged string
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				raw, _ := os.ReadFile(logConfig.Path)
				logged = string(raw)
				if strings.Contains(logged, `"event":"response"`) {
					break
				}
				time.Sleep(time.Millisecond)
			}
			wantRoute := route
			if route == "no_continue" || route == "home_disabled" {
				wantRoute = "yandex"
			}
			if !strings.Contains(logged, `"event":"request"`) || !strings.Contains(logged, `"route":"`+wantRoute+`"`) || !strings.Contains(logged, `"event":"response"`) || strings.Contains(logged, "fixture-audio") {
				t.Fatalf("missing conversation or audio leaked: %s", logged)
			}
			select {
			case msg := <-errCh:
				t.Fatal(msg)
			default:
			}
		})
	}
}

func TestNativeApprovalHoldsExecutionUntilDecision(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	sent := make(chan []byte, 4)
	release := make(chan struct{})
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { sent <- b; return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, resolverFunc(func(context.Context, string) (Resolution, error) { <-release; return Resolution{Route: "yandex"}, nil }))
	defer n.Close()
	n.GateText = true
	input := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "gate-id"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("погода")}}}}}}})
	if err := n.Client(ctx, websocket.MessageBinary, input); err != nil {
		t.Fatal(err)
	}
	first := wire.Decode(<-sent, wire.ClientToServer)
	if !first.Client.GetEvent().GetTextInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
		t.Fatal("approval was not requested")
	}
	select {
	case <-sent:
		t.Fatal("executed before routing")
	default:
	}
	close(release)
	select {
	case b := <-sent:
		if wire.Decode(b, wire.ClientToServer).Kind() != "ContinueRequest" {
			t.Fatal("missing approval")
		}
	case <-ctx.Done():
		t.Fatal("no approval")
	}
}

type fallbackResolver struct{}

func (fallbackResolver) Resolve(context.Context, string) (Resolution, error) {
	return Resolution{Route: "yandex"}, nil
}
func (fallbackResolver) Fallback(context.Context, string) (Resolution, error) {
	return Resolution{Route: "llm", Answer: "Ответ после отказа"}, nil
}

func TestNativeRefusalUsesLLMAndSuppressesOldCloudAudio(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	up := make(chan []byte, 8)
	down := make(chan []byte, 8)
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, fallbackResolver{})
	defer n.Close()
	input := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "refusal"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("запрос")}}}}}}})
	if err := n.Client(ctx, websocket.MessageBinary, input); err != nil {
		t.Fatal(err)
	}
	select {
	case <-up:
	case <-ctx.Done():
		t.Fatal("no cloud input")
	}
	refusal := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "refusal"}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{Response: &response.TAliceResponseProto_TResponse{Directives: []*directives.TDirective{{Type: "client_action", Name: "tts_play_placeholder"}}}, VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new("Я не могу ответить на этот вопрос.")}}}}}}})
	if err := n.Server(ctx, websocket.MessageBinary, refusal); err != nil {
		t.Fatal(err)
	}
	oldtts := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "refusal", StreamId: new(int64(22))}, Event: &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &tts.TTtsSpeak{Format: "audio/opus"}}}}})
	_ = n.Server(ctx, websocket.MessageBinary, oldtts)
	_ = n.Server(ctx, websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, 22), []byte("old audio")...))
	for {
		select {
		case b := <-down:
			f := wire.Decode(b, wire.ServerToClient)
			if f.Kind() == "opaque" || f.Kind() == "TtsSpeak" {
				t.Fatal("cloud refusal audio leaked")
			}
			if f.Kind() == "AliceResponse" {
				if f.Server.GetEvent().GetAliceResponse().GetVoiceResponse().GetOutputSpeech().GetText() != "Ответ после отказа" {
					t.Fatal("old refusal was delivered")
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("no fallback answer")
		}
	}
}

func TestNativeCancellationDropsLateApprovalAndLocalReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	up := make(chan []byte, 8)
	down := make(chan []byte, 8)
	started := make(chan struct{})
	finished := make(chan struct{})
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, resolverFunc(func(ctx context.Context, _ string) (Resolution, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return Resolution{Route: "llm", Answer: "late"}, ctx.Err()
	}))
	defer n.Close()
	input := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "cancel-me"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("вопрос")}}}}}}})
	_ = n.Client(ctx, websocket.MessageBinary, input)
	<-started
	c := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "cancel-control"}, Event: &client.TClientMessage_TEvent_CancelRequest{CancelRequest: &cancellation.TCancelRequest{RequestMessageId: "cancel-me"}}}}})
	_ = n.Client(ctx, websocket.MessageBinary, c)
	cont := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "late"}, Event: &client.TClientMessage_TEvent_ContinueRequest{ContinueRequest: &continuation.TContinueRequest{RequestMessageId: "cancel-me"}}}}})
	_ = n.Client(ctx, websocket.MessageBinary, cont)
	<-finished
	if len(up) != 1 || wire.Decode(<-up, wire.ClientToServer).Kind() != "CancelRequest" {
		t.Fatal("late continuation executed canceled request")
	}
	select {
	case b := <-down:
		if wire.Decode(b, wire.ServerToClient).Kind() != "InputStartAck" {
			t.Fatal("reply after cancellation")
		}
	default:
	}
}

func TestNativeStreamReuseDoesNotKeepCanceledLocalOwner(t *testing.T) {
	down := make(chan []byte, 4)
	n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, nil)
	defer n.Close()
	n.streams[6] = &nativeRequest{canceled: true}
	msg := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "new-cloud-request", StreamId: new(int64(6))}, Event: &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &tts.TTtsSpeak{Format: "audio/opus"}}}}})
	_ = n.Server(t.Context(), websocket.MessageBinary, msg)
	audio := append(binary.BigEndian.AppendUint32(nil, 6), []byte("new audio")...)
	_ = n.Server(t.Context(), websocket.MessageBinary, audio)
	if len(down) != 2 {
		t.Fatal("valid reused stream suppressed by old request")
	}
}

func TestVoiceRoutesOnlyAfterFinalASR(t *testing.T) {
	for _, route := range []string{"yandex", "home", "empty", "home_disabled"} {
		t.Run(route, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			up := make(chan []byte, 16)
			var resolver Resolver = resolverFunc(func(_ context.Context, text string) (Resolution, error) {
				if text != "включи свет" {
					t.Errorf("text=%q", text)
				}
				return Resolution{Route: route, Answer: "Готово"}, nil
			})
			if route == "home_disabled" {
				resolver = new(Engine)
			}
			n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, resolver)
			defer n.Close()
			voice := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "voice", StreamId: new(int64(1))}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_voice_input.Enum()}}, Format: new("audio/opus")}}}}})
			_ = n.Client(ctx, websocket.MessageBinary, voice)
			first := wire.Decode(<-up, wire.ClientToServer)
			if !first.Client.GetEvent().GetVoiceInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
				t.Fatal("ungated voice")
			}
			audio := append(binary.BigEndian.AppendUint32(nil, 1), []byte("mic bytes")...)
			_ = n.Client(ctx, websocket.MessageBinary, audio)
			if !bytes.Equal(<-up, audio) {
				t.Fatal("microphone audio changed")
			}
			normalized := "включи свет"
			if route == "empty" {
				normalized = ""
			}
			result := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "voice"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new(normalized)}}}}}}})
			_ = n.Server(ctx, websocket.MessageBinary, result)
			select {
			case b := <-up:
				kind := wire.Decode(b, wire.ClientToServer).Kind()
				want := "ContinueRequest"
				if route == "home" {
					want = "CancelRequest"
				}
				if kind != want {
					t.Fatalf("kind=%s want=%s", kind, want)
				}
			case <-ctx.Done():
				t.Fatal("no decision")
			}
		})
	}
}

func TestNativeStopPhraseCancelsWithoutReply(t *testing.T) {
	for _, tc := range []struct {
		name, id, text string
		voice          bool
	}{
		{"voice", "stop-voice", "Алиса, хватит включи свет", true},
		{"text", "stop-text", "АЛИСА ХВАТИТ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			up := make(chan []byte, 8)
			down := make(chan []byte, 8)
			called := make(chan struct{}, 1)
			n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, resolverFunc(func(context.Context, string) (Resolution, error) {
				called <- struct{}{}
				return Resolution{Route: "home", Answer: "нельзя"}, nil
			}))
			defer n.Close()
			n.GateText = true
			var input []byte
			if tc.voice {
				input = aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: tc.id, StreamId: new(int64(1))}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_voice_input.Enum()}}, Format: new("audio/opus")}}}}})
			} else {
				input = aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: tc.id}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new(tc.text)}}}}}}})
			}
			if err := n.Client(ctx, websocket.MessageBinary, input); err != nil {
				t.Fatal(err)
			}
			first := wire.Decode(<-up, wire.ClientToServer)
			approved := first.Client.GetEvent().GetVoiceInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval() || first.Client.GetEvent().GetTextInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval()
			if !approved {
				t.Fatal("ungated input")
			}
			if tc.voice {
				asrResult := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: tc.id}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new(tc.text)}}}}}}})
				if err := n.Server(ctx, websocket.MessageBinary, asrResult); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case b := <-up:
				f := wire.Decode(b, wire.ClientToServer)
				cancel := f.Client.GetEvent().GetCancelRequest()
				if f.Kind() != "CancelRequest" || cancel.GetRequestMessageId() != tc.id || cancel.GetUserStop() == nil || cancel.GetLocalExecution() != nil {
					t.Fatalf("kind=%s id=%s user_stop=%v", f.Kind(), cancel.GetRequestMessageId(), cancel.GetUserStop() != nil)
				}
			case <-ctx.Done():
				t.Fatal("no cancel")
			}
			n.Close()
			select {
			case <-called:
				t.Fatal("stop phrase reached a handler")
			default:
			}
			select {
			case b := <-up:
				t.Fatalf("extra upstream %s", wire.Decode(b, wire.ClientToServer).Kind())
			default:
			}
			silent := false
			for {
				select {
				case b := <-down:
					f := wire.Decode(b, wire.ServerToClient)
					switch f.Kind() {
					case "TtsSpeak":
						t.Fatal("stop phrase started playback")
					case "AsrResult":
					case "AliceResponse":
						if silent {
							t.Fatal("extra alice response")
						}
						voice := f.Server.GetEvent().GetAliceResponse().GetVoiceResponse()
						if voice.GetHasVoiceResponse() || voice.GetShouldListen() || voice.GetOutputSpeech().GetText() != "" {
							t.Fatalf("spoken stop response: has_voice=%v listen=%v text=%q", voice.GetHasVoiceResponse(), voice.GetShouldListen(), voice.GetOutputSpeech().GetText())
						}
						if f.Server.GetEvent().GetAliceResponse().GetError() != nil {
							t.Fatal("stop response carries an error")
						}
						silent = true
					default:
						t.Fatalf("unexpected downstream %s", f.Kind())
					}
				default:
					if !silent {
						t.Fatal("dialog was not finished")
					}
					return
				}
			}
		})
	}
}

func TestNativeStandalonePlaybackStopReachesYandex(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	up := make(chan []byte, 8)
	down := make(chan []byte, 8)
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, &Engine{})
	defer n.Close()
	n.GateText = true
	input := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "play-stop"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("Алиса, хватит!")}}}}}}})
	if err := n.Client(ctx, websocket.MessageBinary, input); err != nil {
		t.Fatal(err)
	}
	if kind := wire.Decode(<-up, wire.ClientToServer).Kind(); kind == "" {
		t.Fatal("no gated input")
	}
	select {
	case b := <-up:
		if kind := wire.Decode(b, wire.ClientToServer).Kind(); kind != "ContinueRequest" {
			t.Fatalf("kind=%s", kind)
		}
	case <-ctx.Done():
		t.Fatal("standalone playback stop was not sent to Yandex")
	}
	select {
	case b := <-down:
		t.Fatalf("unexpected downstream %s", wire.Decode(b, wire.ServerToClient).Kind())
	default:
	}
}

func TestRefusalReplacementKeepsExecutorDirectives(t *testing.T) {
	for _, tc := range []struct {
		name      string
		directive *directives.TDirective
		want      bool
	}{
		{"speech", &directives.TDirective{Type: "client_action", Name: "tts_play_placeholder"}, true},
		{"home", &directives.TDirective{Type: "client_action", Name: "devices_action"}, false},
		{"unknown", &directives.TDirective{Type: "future", Name: "unknown"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &response.TAliceResponseProto{Response: &response.TAliceResponseProto_TResponse{Directives: []*directives.TDirective{tc.directive}}}
			if canReplaceResponse(r) != tc.want {
				t.Fatal("incorrect directive substitution policy")
			}
		})
	}
}

func TestExistingNativeSessionUsesReloadedRefusalPolicy(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	up := make(chan []byte, 16)
	down := make(chan []byte, 16)
	engine := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return "Локальный ответ", nil
	})}
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, engine)
	defer n.Close()
	for i, enabled := range []bool{false, true} {
		cfg := DefaultRoutingConfig()
		cfg.Rules = []RoutingRule{}
		cfg.DefaultRoute = "yandex"
		cfg.Refusal.Enabled = enabled
		if err := engine.SetRouting(cfg); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("policy-%d", i)
		input := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("вопрос")}}}}}}})
		if err := n.Client(ctx, websocket.MessageBinary, input); err != nil {
			t.Fatal(err)
		}
		select {
		case <-up:
		case <-ctx.Done():
			t.Fatal("no cloud request")
		}
		original := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: id}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new("Я не знаю")}}}}}}})
		if err := n.Server(ctx, websocket.MessageBinary, original); err != nil {
			t.Fatal(err)
		}
		select {
		case b := <-down:
			if !enabled {
				if !bytes.Equal(b, original) {
					t.Fatal("disabled fallback replaced answer")
				}
			} else if wire.Decode(b, wire.ServerToClient).Server.GetEvent().GetAliceResponse().GetVoiceResponse().GetOutputSpeech().GetText() != "Локальный ответ" {
				t.Fatal("existing session did not pick up policy")
			}
		case <-ctx.Done():
			t.Fatal("no response")
		}
	}
}

func TestExistingNativeSessionUsesReloadedRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	up := make(chan []byte, 16)
	down := make(chan []byte, 16)
	engine := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) { return "Новый маршрут", nil })}
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, engine)
	defer n.Close()
	cfg := DefaultRoutingConfig()
	cfg.Rules = []RoutingRule{}
	cfg.DefaultRoute = "yandex"
	if err := engine.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	input := func(id string) []byte {
		return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("один вопрос")}}}}}}})
	}
	first := input("before")
	if err := n.Client(ctx, websocket.MessageBinary, first); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-up:
		if !bytes.Equal(b, first) {
			t.Fatal("original cloud request changed")
		}
	case <-ctx.Done():
		t.Fatal("no cloud request")
	}
	cfg.DefaultRoute = "llm"
	if err := engine.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	if err := n.Client(ctx, websocket.MessageBinary, input("after")); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case b := <-down:
			f := wire.Decode(b, wire.ServerToClient)
			if f.Kind() == "AliceResponse" {
				if f.Server.GetEvent().GetAliceResponse().GetVoiceResponse().GetOutputSpeech().GetText() != "Новый маршрут" {
					t.Fatal("old route reused")
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("existing session did not change route")
		}
	}
}

type electionResolver struct {
	route, answer string
	called        chan struct{}
	entered       chan struct{}
	block         bool
}

func (e *electionResolver) Decide(context.Context, string) RouteDecision {
	return RouteDecision{Route: e.route, Source: "rule"}
}
func (e *electionResolver) Resolve(ctx context.Context, text string) (Resolution, error) {
	return e.ResolveDecision(ctx, text, e.Decide(ctx, text))
}
func (e *electionResolver) ResolveDecision(ctx context.Context, _ string, d RouteDecision) (Resolution, error) {
	if e.entered != nil {
		e.entered <- struct{}{}
	}
	if e.block {
		<-ctx.Done()
		return Resolution{}, ctx.Err()
	}
	if e.called != nil {
		e.called <- struct{}{}
	}
	return Resolution{Route: d.Route, Answer: e.answer}, nil
}

func voiceRequest(id string) []byte {
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id, StreamId: new(int64(1))}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_voice_input.Enum()}}, Format: new("audio/opus")}}}}})
}

func asrFinal(id, text string) []byte {
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: id}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new(text)}}}}}}})
}

func spotterFrame(id string, result spotter.TSpotterValidation_EResult, multi *bool) []byte {
	v := &spotter.TSpotterValidation{Result: result}
	if multi != nil {
		v.CanceledCauseOfMultiactivation = multi
	}
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: "spotter-" + id, RefMessageId: id}, Event: &server.TServerMessage_TEvent_SpotterValidation{SpotterValidation: v}}}})
}

func TestNativeSpotterElection(t *testing.T) {
	rejected := spotter.TSpotterValidation_SpottingRejected
	okResult := spotter.TSpotterValidation_Ok
	multi := true
	for _, tc := range []struct {
		name    string
		result  spotter.TSpotterValidation_EResult
		multi   *bool
		speak   bool
		resolve bool
	}{
		{"rejected", rejected, nil, false, false},
		{"multiactivation", okResult, &multi, false, false},
		{"winner", okResult, nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			up := make(chan []byte, 8)
			down := make(chan []byte, 8)
			called := make(chan struct{}, 1)
			n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, &electionResolver{route: "home", answer: "Свет включён.", called: called})
			defer n.Close()
			n.SpotterWait = time.Second
			frame := spotterFrame("elect", tc.result, tc.multi)
			if err := n.Client(ctx, websocket.MessageBinary, voiceRequest("elect")); err != nil {
				t.Fatal(err)
			}
			if _, err := readUpstream(ctx, up); err != nil {
				t.Fatal(err)
			}
			if err := n.Server(ctx, websocket.MessageBinary, frame); err != nil {
				t.Fatal(err)
			}
			if err := n.Server(ctx, websocket.MessageBinary, asrFinal("elect", "включи свет")); err != nil {
				t.Fatal(err)
			}
			seenSpotter := false
			spoken := false
			silent := false
			for !spoken && !silent {
				select {
				case b := <-down:
					f := wire.Decode(b, wire.ServerToClient)
					switch f.Kind() {
					case "SpotterValidation":
						seenSpotter = true
					case "TtsSpeak":
						t.Fatal("election started cloud playback")
					case "AliceResponse":
						voice := f.Server.GetEvent().GetAliceResponse().GetVoiceResponse()
						text := voice.GetOutputSpeech().GetText()
						if tc.speak {
							if text != "Свет включён." || !voice.GetHasVoiceResponse() {
								t.Fatalf("winner speech %q has_voice=%v", text, voice.GetHasVoiceResponse())
							}
							spoken = true
						} else if voice.GetHasVoiceResponse() || voice.GetShouldListen() || text != "" {
							t.Fatalf("loser spoke %q", text)
						} else {
							silent = true
						}
					}
				case <-ctx.Done():
					t.Fatal("no speaker completion")
				}
			}
			if seenSpotter {
				t.Fatal("local route forwarded SpotterValidation")
			}
			select {
			case b := <-up:
				cancel := wire.Decode(b, wire.ClientToServer).Client.GetEvent().GetCancelRequest()
				if tc.speak {
					if cancel.GetLocalExecution() == nil || cancel.GetUserStop() != nil {
						t.Fatal("winner did not take local execution")
					}
				} else if cancel.GetUserStop() == nil || cancel.GetLocalExecution() != nil {
					t.Fatal("loser did not send user stop")
				}
			case <-ctx.Done():
				t.Fatal("no cloud cancel")
			}
			select {
			case <-called:
				if !tc.resolve {
					t.Fatal("loser executed the command")
				}
			default:
				if tc.resolve {
					t.Fatal("winner did not execute the command")
				}
			}
		})
	}

	t.Run("timeout speaks", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		called := make(chan struct{}, 1)
		up := make(chan []byte, 4)
		down := make(chan []byte, 4)
		n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, &electionResolver{route: "home", answer: "Свет включён.", called: called})
		defer n.Close()
		n.SpotterWait = 20 * time.Millisecond
		if err := n.Client(ctx, websocket.MessageBinary, voiceRequest("solo")); err != nil {
			t.Fatal(err)
		}
		if err := n.Server(ctx, websocket.MessageBinary, asrFinal("solo", "включи свет")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-called:
		case <-ctx.Done():
			t.Fatal("single speaker did not answer without a verdict")
		}
	})

	t.Run("text does not wait", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		called := make(chan struct{}, 1)
		n := NewNative(ctx, func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, &electionResolver{route: "home", answer: "Свет включён.", called: called})
		defer n.Close()
		n.SpotterWait = 400 * time.Millisecond
		input := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "typed"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("включи свет")}}}}}}})
		start := time.Now()
		if err := n.Client(ctx, websocket.MessageBinary, input); err != nil {
			t.Fatal(err)
		}
		select {
		case <-called:
			if time.Since(start) > 150*time.Millisecond {
				t.Fatal("text input waited for spotter validation")
			}
		case <-ctx.Done():
			t.Fatal("text input was not handled")
		}
	})

	t.Run("yandex forwards held frame", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		up := make(chan []byte, 4)
		down := make(chan []byte, 4)
		n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, &electionResolver{route: "yandex"})
		defer n.Close()
		n.SpotterWait = time.Second
		frame := spotterFrame("cloud", okResult, nil)
		if err := n.Client(ctx, websocket.MessageBinary, voiceRequest("cloud")); err != nil {
			t.Fatal(err)
		}
		if err := n.Server(ctx, websocket.MessageBinary, frame); err != nil {
			t.Fatal(err)
		}
		if err := n.Server(ctx, websocket.MessageBinary, asrFinal("cloud", "какая погода")); err != nil {
			t.Fatal(err)
		}
		continued := false
		forwarded := false
		deadline := time.After(time.Second)
		for !continued || !forwarded {
			select {
			case b := <-up:
				if wire.Decode(b, wire.ClientToServer).Kind() == "ContinueRequest" {
					continued = true
				}
			case b := <-down:
				if bytes.Equal(b, frame) {
					forwarded = true
				}
			case <-deadline:
				t.Fatalf("yandex route continued=%v forwarded=%v", continued, forwarded)
			}
		}
	})

	t.Run("late rejection cancels local work", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		up := make(chan []byte, 4)
		down := make(chan []byte, 4)
		entered := make(chan struct{}, 1)
		var canceledSend atomic.Bool
		check := func(c context.Context, ch chan []byte, b []byte) error {
			if c.Err() != nil {
				canceledSend.Store(true)
			}
			ch <- b
			return nil
		}
		n := NewNative(ctx, func(c context.Context, _ websocket.MessageType, b []byte) error { return check(c, up, b) }, func(c context.Context, _ websocket.MessageType, b []byte) error { return check(c, down, b) }, &electionResolver{route: "home", answer: "Свет включён.", entered: entered, block: true})
		defer n.Close()
		n.SpotterWait = 20 * time.Millisecond
		if err := n.Client(ctx, websocket.MessageBinary, voiceRequest("late")); err != nil {
			t.Fatal(err)
		}
		if err := n.Server(ctx, websocket.MessageBinary, asrFinal("late", "включи свет")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("local work did not start")
		}
		if err := n.Server(ctx, websocket.MessageBinary, spotterFrame("late", rejected, nil)); err != nil {
			t.Fatal(err)
		}
		silent := false
		stopped := false
		for !silent || !stopped {
			select {
			case b := <-up:
				f := wire.Decode(b, wire.ClientToServer)
				if f.Kind() == "CancelRequest" && f.Client.GetEvent().GetCancelRequest().GetUserStop() != nil {
					stopped = true
				}
			case b := <-down:
				f := wire.Decode(b, wire.ServerToClient)
				if f.Kind() == "AliceResponse" {
					voice := f.Server.GetEvent().GetAliceResponse().GetVoiceResponse()
					if voice.GetOutputSpeech().GetText() != "" || voice.GetHasVoiceResponse() {
						t.Fatal("late loser spoke")
					}
					silent = true
				}
			case <-ctx.Done():
				t.Fatal("late rejection did not finish the dialog")
			}
		}
		if canceledSend.Load() {
			t.Fatal("closing messages used a canceled context")
		}
	})
}

func readUpstream(ctx context.Context, up <-chan []byte) ([]byte, error) {
	select {
	case b := <-up:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
