package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"quasarbridge/internal/journal"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	directives "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	asr "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	cancellation "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/cancel_request"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	continuation "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/continue_request"
	tts "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
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

func TestNativeFinalASRLogsOnlyAggregateMetadata(t *testing.T) {
	for _, tc := range []struct {
		name      string
		aggregate *recognition.TAggregatedRecognition
		present   bool
		index     float64
		textBytes float64
	}{
		{name: "absent"},
		{name: "present", aggregate: &recognition.TAggregatedRecognition{Index: new(uint64(7)), RecognizedText: new("личный текст"), IsFinal: new(true)}, present: true, index: 7, textBytes: 23},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			var forwarded []byte
			n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(_ context.Context, _ websocket.MessageType, data []byte) error {
				forwarded = bytes.Clone(data)
				return nil
			}, nil)
			t.Cleanup(n.Close)
			voice := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "asr-log"}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: new(common.EEventType_voice_input)}}}}}}}
			if err := n.Client(t.Context(), websocket.MessageBinary, aa(voice)); err != nil {
				t.Fatal(err)
			}
			result := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "asr-log"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), AggregatedRecognition: tc.aggregate}}}}}
			raw := protowire.AppendString(protowire.AppendTag(aa(result), 777, protowire.BytesType), "future-asr-field")
			if err := n.Server(t.Context(), websocket.MessageBinary, raw); err != nil {
				t.Fatal(err)
			}
			n.Close()
			if !bytes.Equal(forwarded, raw) {
				t.Fatal("ASR frame or unknown fields changed in transit")
			}
			if strings.Contains(logs.String(), "личный текст") {
				t.Fatal("aggregate transcript leaked into diagnostics")
			}
			found := false
			for line := range strings.SplitSeq(logs.String(), "\n") {
				if line == "" {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatal(err)
				}
				if event["msg"] == "native_asr_final" {
					found = true
					if event["aggregate_present"] != tc.present || event["aggregate_final"] != tc.present || event["aggregate_index"] != tc.index || event["aggregate_text_bytes"] != tc.textBytes {
						t.Fatalf("aggregate metadata=%v", event)
					}
				}
				if event["msg"] == "native_asr_aggregate" && event["end_of_utt"] != true {
					t.Fatalf("completed ASR aggregate boundary=%v", event["end_of_utt"])
				}
			}
			if !found {
				t.Fatal("final ASR diagnostic missing")
			}
		})
	}
}

func TestNativeAggregateDiagnosticsAreBoundedPerRequest(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
	t.Cleanup(n.Close)
	for _, id := range []string{"aggregate-a", "aggregate-b"} {
		voice := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: new(common.EEventType_voice_input)}}}}}}}
		if err := n.Client(t.Context(), websocket.MessageBinary, aa(voice)); err != nil {
			t.Fatal(err)
		}
	}
	for _, packet := range []struct {
		id    string
		index uint64
		final bool
	}{
		{"aggregate-a", 0, false},
		{"aggregate-a", 1, false},
		{"aggregate-a", 2, true},
		{"aggregate-a", 3, true},
		{"aggregate-b", 7, true},
	} {
		result := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: packet.id}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{AggregatedRecognition: &recognition.TAggregatedRecognition{Index: new(packet.index), RecognizedText: new("do not log this"), IsFinal: new(packet.final)}}}}}}
		if err := n.Server(t.Context(), websocket.MessageBinary, aa(result)); err != nil {
			t.Fatal(err)
		}
	}
	n.Close()
	if strings.Contains(logs.String(), "do not log this") {
		t.Fatal("aggregate transcript leaked into diagnostics")
	}
	var indexes []float64
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["msg"] == "native_asr_aggregate" {
			if event["end_of_utt"] != false {
				t.Fatalf("partial ASR aggregate boundary=%v", event["end_of_utt"])
			}
			indexes = append(indexes, event["aggregate_index"].(float64))
		}
	}
	if !slices.Equal(indexes, []float64{0, 2, 7}) {
		t.Fatalf("aggregate observations=%v; want first presence/final only per request", indexes)
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
