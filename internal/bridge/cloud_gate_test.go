package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	directive "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	asr "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	cancellation "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/cancel_request"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	stream "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/stream_control"
	tts "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	deferred "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/streaming"
	"google.golang.org/protobuf/encoding/protowire"
	"quasarbridge/internal/wire"
)

type cloudGateResolver struct {
	route       string
	resultRoute string
	deciding    chan context.Context
	decision    chan struct{}
	executing   chan context.Context
	answer      chan struct{}
}

func (r *cloudGateResolver) Decide(ctx context.Context, _ string) RouteDecision {
	r.deciding <- ctx
	select {
	case <-ctx.Done():
	case <-r.decision:
	}
	return RouteDecision{Route: r.route, Source: "classifier"}
}

func (r *cloudGateResolver) ResolveDecision(ctx context.Context, _ string, decision RouteDecision) (Resolution, error) {
	r.executing <- ctx
	select {
	case <-ctx.Done():
		return Resolution{}, ctx.Err()
	case <-r.answer:
		if r.resultRoute != "" {
			decision.Route = r.resultRoute
		}
		return Resolution{Route: decision.Route, Answer: "Local answer"}, nil
	}
}

func (r *cloudGateResolver) Fallback(context.Context, string) (Resolution, error) {
	return Resolution{Route: "llm", Answer: "Fallback answer"}, nil
}

func (r *cloudGateResolver) Resolve(ctx context.Context, text string) (Resolution, error) {
	return r.ResolveDecision(ctx, text, r.Decide(ctx, text))
}

func newCloudGateSession(t *testing.T, route string) (*Native, *cloudGateResolver, chan []byte, chan []byte) {
	t.Helper()
	r := &cloudGateResolver{route: route, deciding: make(chan context.Context, 1), decision: make(chan struct{}), executing: make(chan context.Context, 1), answer: make(chan struct{})}
	up, down := make(chan []byte, 64), make(chan []byte, 64)
	n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, data []byte) error {
		up <- bytes.Clone(data)
		return nil
	}, func(_ context.Context, _ websocket.MessageType, data []byte) error {
		down <- bytes.Clone(data)
		return nil
	}, r)
	t.Cleanup(n.Close)
	return n, r, up, down
}

func cloudGateInput(id, text string) []byte {
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: id},
		Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{
			Header:  &request.TAliceRequestProto_THeader{RequestId: new("application-" + id)},
			Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new(text)}},
		}},
	}}})
}

func cloudGateResponse(id string) []byte {
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
		Header: &header.TEventHeader{RefMessageId: id},
		Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{
			Response: &response.TAliceResponseProto_TResponse{IsStreaming: true, Directives: []*directive.TDirective{
				{Type: "client_action", Name: "draw_animation_directive"},
				{Type: "client_action", Name: "tts_play_placeholder"},
			}},
		}},
	}}})
}

func cloudGateSpeak(id string, streamID uint32) []byte {
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
		Header: &header.TEventHeader{RefMessageId: id, StreamId: new(int64(streamID))},
		Event:  &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &tts.TTtsSpeak{Format: "audio/opus"}},
	}}})
}

func cloudGateClose(id string, streamID uint32) []byte {
	return aa(&server.TServerMessage{Message: &server.TServerMessage_StreamControl{StreamControl: &stream.TStreamControl{
		MessageId: id, StreamId: int64(streamID), Action: &stream.TStreamControl_Close{Close: &stream.TStreamControl_TActionClose{}},
	}}})
}

func cloudGatePart(id, text string) []byte {
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
		Header: &header.TEventHeader{RefMessageId: id},
		Event: &server.TServerMessage_TEvent_DeferredAliceResponse{DeferredAliceResponse: &deferred.TDeferedAliceResponseProto{
			RequestId: "application-" + id, ResponsePartialNum: 1, IsLast: true,
			TDeferredResponse: &deferred.TDeferedAliceResponseProto_BaseResponse{BaseResponse: &deferred.TDeferedAliceResponseProto_TBaseResponse{Text: new(text)}},
		}},
	}}})
}

func assertCloudGateQuiet(t *testing.T, down <-chan []byte) {
	t.Helper()
	select {
	case data := <-down:
		t.Fatalf("unexpected frame reached speaker: %s", wire.Decode(data, wire.ServerToClient).Kind())
	default:
	}
}

func sendCloudGate(t *testing.T, n *Native, frames ...[]byte) {
	t.Helper()
	for _, frame := range frames {
		if err := n.Server(t.Context(), websocket.MessageBinary, frame); err != nil {
			t.Fatal(err)
		}
	}
}

func nextCloudGate[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("missing expected protocol event")
		var zero T
		return zero
	}
}

func TestCloudGateYandexReplaysOriginalFramesInOrder(t *testing.T) {
	for _, route := range []string{"yandex", "airplay"} {
		t.Run(route, func(t *testing.T) {
			n, resolver, up, down := newCloudGateSession(t, route)
			resolver.resultRoute = "yandex"
			if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput("cloud", "question")); err != nil {
				t.Fatal(err)
			}
			nextCloudGate(t, up)
			nextCloudGate(t, resolver.deciding)
			initial := protowire.AppendString(protowire.AppendTag(cloudGateResponse("cloud"), 991, protowire.BytesType), "unknown future field")
			frames := [][]byte{initial, cloudGatePart("cloud", "Cloud answer"), cloudGateSpeak("cloud", 91), append(binary.BigEndian.AppendUint32(nil, 91), []byte("cloud audio")...), cloudGateClose("", 91)}
			sendCloudGate(t, n, frames[:3]...)
			assertCloudGateQuiet(t, down)
			// Recognition still reaches the speaker while response output is held.
			asrFrame := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "cloud"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{}}}}})
			sendCloudGate(t, n, asrFrame)
			if !bytes.Equal(nextCloudGate(t, down), asrFrame) {
				t.Fatal("ASR did not pass through unchanged")
			}
			close(resolver.decision)
			nextCloudGate(t, resolver.executing)
			sendCloudGate(t, n, frames[3:]...)
			assertCloudGateQuiet(t, down)
			close(resolver.answer)
			if frame := wire.Decode(nextCloudGate(t, up), wire.ClientToServer); frame.Kind() != "ContinueRequest" {
				t.Fatalf("cloud request replayed instead of continued: %s", frame.Kind())
			}
			for i, want := range frames {
				if got := nextCloudGate(t, down); !bytes.Equal(got, want) {
					t.Fatalf("replayed frame %d changed or reordered", i)
				}
			}
			assertCloudGateQuiet(t, up)
			n.mu.Lock()
			defer n.mu.Unlock()
			if n.cloudBytes != 0 || len(n.cloudStreams) != 0 {
				t.Fatal("replayed cloud output retained memory or stream ownership")
			}
		})
	}
}

func TestCloudGateLocalReplySuppressesHeldAndLateCloudAudio(t *testing.T) {
	n, resolver, up, down := newCloudGateSession(t, "llm")
	if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput("local", "question")); err != nil {
		t.Fatal(err)
	}
	nextCloudGate(t, up)
	nextCloudGate(t, resolver.deciding)
	oldAudio := append(binary.BigEndian.AppendUint32(nil, 91), []byte("old cloud audio")...)
	sendCloudGate(t, n, cloudGateResponse("local"), cloudGateSpeak("local", 91), oldAudio)
	close(resolver.decision)
	ctx := nextCloudGate(t, resolver.executing)
	sendCloudGate(t, n, cloudGatePart("local", "Old answer"), oldAudio)
	if ctx.Err() != nil {
		t.Fatal("cloud output canceled local answer generation")
	}
	assertCloudGateQuiet(t, down)
	close(resolver.answer)
	if frame := wire.Decode(nextCloudGate(t, down), wire.ServerToClient); frame.Server.GetEvent().GetAliceResponse().GetVoiceResponse().GetOutputSpeech().GetText() != "Local answer" {
		t.Fatalf("unexpected final response: %s", frame.Kind())
	}
	if frame := wire.Decode(nextCloudGate(t, up), wire.ClientToServer); frame.Kind() != "CancelRequest" {
		t.Fatalf("cloud was not canceled: %s", frame.Kind())
	}
	synthesis := wire.Decode(nextCloudGate(t, up), wire.ClientToServer)
	if synthesis.Kind() != "TtsGenerate" {
		t.Fatalf("missing local TTS: %s", synthesis.Kind())
	}
	lazy := aa(&server.TServerMessage{Message: &server.TServerMessage_StreamControl{StreamControl: &stream.TStreamControl{
		MessageId: "local", StreamId: 91, Action: &stream.TStreamControl_LazyTTS{LazyTTS: &stream.TStreamControl_TActionLazyTTS{}},
	}}})
	sendCloudGate(t, n, lazy, oldAudio, cloudGateClose("", 91), cloudGateSpeak("local", 91), oldAudio)
	assertCloudGateQuiet(t, down)
	// The local synthesizer can reuse that same stream without inheriting a block.
	feedSpeech(t, n, synthesis.Client.GetEvent().GetHeader().GetMessageId(), 91, false)
	if frame := wire.Decode(nextCloudGate(t, down), wire.ServerToClient); frame.Kind() != "TtsSpeak" {
		t.Fatalf("local speech was suppressed: %s", frame.Kind())
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cloudBytes != 0 || len(n.cloudStreams) != 0 {
		t.Fatal("discarded cloud output retained memory")
	}
}

func TestCloudGateReplayStillInterceptsRefusal(t *testing.T) {
	n, resolver, up, down := newCloudGateSession(t, "yandex")
	if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput("refusal-gate", "question")); err != nil {
		t.Fatal(err)
	}
	nextCloudGate(t, up)
	nextCloudGate(t, resolver.deciding)
	refusal := wire.Decode(cloudGateResponse("refusal-gate"), wire.ServerToClient)
	cloudResponse := refusal.Server.GetEvent().GetAliceResponse()
	cloudResponse.Response.Directives = cloudResponse.Response.Directives[1:]
	cloudResponse.VoiceResponse = &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new("Я не могу ответить на этот вопрос.")}}
	data, err := refusal.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	sendCloudGate(t, n, data, cloudGateSpeak("refusal-gate", 92), append(binary.BigEndian.AppendUint32(nil, 92), []byte("refusal audio")...))
	close(resolver.decision)
	nextCloudGate(t, resolver.executing)
	close(resolver.answer)
	got := wire.Decode(nextCloudGate(t, down), wire.ServerToClient).Server.GetEvent().GetAliceResponse()
	if got.GetVoiceResponse().GetOutputSpeech().GetText() != "Fallback answer" {
		t.Fatalf("queued refusal bypassed fallback: %v", got)
	}
	assertCloudGateQuiet(t, down)
}

func TestCloudGateCancellationReleasesPendingOutput(t *testing.T) {
	n, resolver, up, down := newCloudGateSession(t, "llm")
	if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput("cancel-gate", "question")); err != nil {
		t.Fatal(err)
	}
	nextCloudGate(t, up)
	ctx := nextCloudGate(t, resolver.deciding)
	sendCloudGate(t, n, cloudGateResponse("cancel-gate"), cloudGateSpeak("cancel-gate", 94))
	cancel := cloudGateCancel("cancel-gate")
	if err := n.Client(t.Context(), websocket.MessageBinary, cancel); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("client cancellation did not cancel classification")
	}
	sendCloudGate(t, n, append(binary.BigEndian.AppendUint32(nil, 94), []byte("late old audio")...), cloudGateSpeak("cancel-gate", 95), append(binary.BigEndian.AppendUint32(nil, 95), []byte("late new audio")...))
	assertCloudGateQuiet(t, down)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cloudBytes != 0 || len(n.cloudStreams) != 0 {
		t.Fatal("cancellation retained buffered output")
	}
}

func cloudGateCancel(id string) []byte {
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "cancel-" + id}, Event: &client.TClientMessage_TEvent_CancelRequest{CancelRequest: &cancellation.TCancelRequest{RequestMessageId: id}}}}})
}

func TestCloudGateCancelAfterYandexReplayBlocksLiveAudio(t *testing.T) {
	n, resolver, up, down := newCloudGateSession(t, "yandex")
	if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput("live-cancel", "question")); err != nil {
		t.Fatal(err)
	}
	nextCloudGate(t, up)
	nextCloudGate(t, resolver.deciding)
	sendCloudGate(t, n, cloudGateResponse("live-cancel"), cloudGateSpeak("live-cancel", 97))
	close(resolver.decision)
	nextCloudGate(t, resolver.executing)
	close(resolver.answer)
	nextCloudGate(t, down)
	nextCloudGate(t, down)
	if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateCancel("live-cancel")); err != nil {
		t.Fatal(err)
	}
	sendCloudGate(t, n, append(binary.BigEndian.AppendUint32(nil, 97), []byte("canceled cloud speech")...), cloudGateClose("", 97))
	assertCloudGateQuiet(t, down)
}

func TestCloudGateExpiryDoesNotReleaseOrphanedAudio(t *testing.T) {
	n, resolver, up, down := newCloudGateSession(t, "llm")
	if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput("expires", "question")); err != nil {
		t.Fatal(err)
	}
	nextCloudGate(t, up)
	nextCloudGate(t, resolver.deciding)
	sendCloudGate(t, n, cloudGateResponse("expires"), cloudGateSpeak("expires", 98))
	n.mu.Lock()
	n.requests["expires"].expires.Reset(0)
	n.mu.Unlock()
	select {
	case <-n.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("expiry left a connection with orphaned pending cloud output")
	}
	if err := n.Server(t.Context(), websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, 98), []byte("expired speech")...)); err == nil {
		t.Fatal("expired connection accepted orphaned audio")
	}
	assertCloudGateQuiet(t, down)
}

func TestCloudGateYandexRevisionAfterLocalProgressFailsClosed(t *testing.T) {
	n, resolver, up, down := newCloudGateSession(t, "airplay")
	resolver.resultRoute = "yandex"
	n.ProgressAfter = time.Millisecond
	if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput("committed", "question")); err != nil {
		t.Fatal(err)
	}
	nextCloudGate(t, up)
	nextCloudGate(t, resolver.deciding)
	sendCloudGate(t, n, cloudGateResponse("committed"))
	close(resolver.decision)
	nextCloudGate(t, resolver.executing)
	initial := wire.Decode(nextCloudGate(t, down), wire.ServerToClient)
	if !initial.Server.GetEvent().GetAliceResponse().GetResponse().GetIsStreaming() {
		t.Fatal("local progress did not open its own response stream")
	}
	close(resolver.answer)
	select {
	case <-n.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("route revision resumed the canceled cloud executor")
	}
	for len(up) > 0 {
		if frame := wire.Decode(<-up, wire.ClientToServer); frame.Kind() == "ContinueRequest" {
			t.Fatal("route revision continued cloud after local streaming began")
		}
	}
	for len(down) > 0 {
		if frame := wire.Decode(<-down, wire.ServerToClient); frame.Kind() == "AliceResponse" {
			t.Fatal("route revision opened a second response stream")
		}
	}
}

func TestCloudGateRejectsUnsafeEarlyExecutor(t *testing.T) {
	n, resolver, up, down := newCloudGateSession(t, "home")
	if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput("unsafe", "question")); err != nil {
		t.Fatal(err)
	}
	nextCloudGate(t, up)
	ctx := nextCloudGate(t, resolver.deciding)
	frame := wire.Decode(cloudGateResponse("unsafe"), wire.ServerToClient)
	frame.Server.GetEvent().GetAliceResponse().Response.Directives = []*directive.TDirective{{Type: "server_action", Name: "execute"}}
	data, err := frame.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Server(t.Context(), websocket.MessageBinary, data); err == nil {
		t.Fatal("unsafe cloud executor did not stop the session")
	}
	if ctx.Err() == nil {
		t.Fatal("local executor could run after an unsafe cloud response")
	}
	assertCloudGateQuiet(t, down)
}

func TestCloudGateBufferLimitsReleaseMemory(t *testing.T) {
	for _, limit := range []string{"bytes", "frames", "session"} {
		t.Run(limit, func(t *testing.T) {
			n, resolver, up, down := newCloudGateSession(t, "llm")
			if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput("limit", "question")); err != nil {
				t.Fatal(err)
			}
			nextCloudGate(t, up)
			ctx := nextCloudGate(t, resolver.deciding)
			data := cloudGateResponse("limit")
			switch limit {
			case "bytes":
				data = protowire.AppendBytes(protowire.AppendTag(data, 992, protowire.BytesType), make([]byte, maxCloudRequestBytes))
			case "frames":
				for range maxCloudFrames {
					sendCloudGate(t, n, data)
				}
			case "session":
				// Distinct pending voice requests share one connection's memory bound.
				for i := range 4 {
					id := fmt.Sprintf("other-%d", i)
					if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput(id, "")); err != nil {
						t.Fatal(err)
					}
					nextCloudGate(t, up)
					large := cloudGateResponse(id)
					large = protowire.AppendBytes(protowire.AppendTag(large, 992, protowire.BytesType), make([]byte, maxCloudRequestBytes-len(large)-5))
					sendCloudGate(t, n, large)
				}
			}
			if err := n.Server(t.Context(), websocket.MessageBinary, data); err == nil {
				t.Fatal("pending cloud buffer limit was not enforced")
			}
			if ctx.Err() == nil {
				t.Fatal("buffer overflow left local classification running")
			}
			assertCloudGateQuiet(t, down)
			n.Close()
			n.mu.Lock()
			defer n.mu.Unlock()
			if n.cloudBytes != 0 || len(n.cloudStreams) != 0 {
				t.Fatal("closing an overflowing session retained buffered output")
			}
		})
	}
}

func TestCloudGateEarlyResponsePreservesLocalClassification(t *testing.T) {
	n, resolver, up, down := newCloudGateSession(t, "llm")
	if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput("early", "question")); err != nil {
		t.Fatal(err)
	}
	nextCloudGate(t, up)
	ctx := nextCloudGate(t, resolver.deciding)
	if err := n.Server(t.Context(), websocket.MessageBinary, cloudGateResponse("early")); err != nil {
		t.Fatal(err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("early cloud initializer canceled local classification: %v", err)
	}
	select {
	case data := <-down:
		t.Fatalf("cloud response reached speaker before routing: %s", wire.Decode(data, wire.ServerToClient).Kind())
	default:
	}
	close(resolver.decision)
	nextCloudGate(t, resolver.executing)
	close(resolver.answer)
	answer := wire.Decode(nextCloudGate(t, down), wire.ServerToClient).Server.GetEvent().GetAliceResponse()
	if answer.GetVoiceResponse().GetOutputSpeech().GetText() != "Local answer" {
		t.Fatalf("local response missing: %v", answer)
	}
}

func TestCloudGateResponseBeforeFinalASRStillRoutesLocally(t *testing.T) {
	n, resolver, up, down := newCloudGateSession(t, "llm")
	if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput("before-asr", "")); err != nil {
		t.Fatal(err)
	}
	nextCloudGate(t, up)
	sendCloudGate(t, n, cloudGateResponse("before-asr"))
	assertCloudGateQuiet(t, down)
	finalASR := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
		Header: &header.TEventHeader{RefMessageId: "before-asr"},
		Event:  &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("question")}}}},
	}}})
	sendCloudGate(t, n, finalASR)
	if !bytes.Equal(nextCloudGate(t, down), finalASR) {
		t.Fatal("final recognition was held behind the cloud response")
	}
	ctx := nextCloudGate(t, resolver.deciding)
	if ctx.Err() != nil {
		t.Fatal("early initializer prevented classification after ASR")
	}
	close(resolver.decision)
	nextCloudGate(t, resolver.executing)
	close(resolver.answer)
	if frame := wire.Decode(nextCloudGate(t, down), wire.ServerToClient); frame.Server.GetEvent().GetAliceResponse().GetVoiceResponse().GetOutputSpeech().GetText() != "Local answer" {
		t.Fatal("local reply missing after final ASR")
	}
}

func TestCloudGateLiveOutputCannotOvertakeReplay(t *testing.T) {
	n, resolver, up, down := newCloudGateSession(t, "yandex")
	initial := cloudGateResponse("ordered")
	entered, release := make(chan struct{}), make(chan struct{})
	originalDown := n.down
	n.down = func(ctx context.Context, kind websocket.MessageType, data []byte) error {
		if bytes.Equal(data, initial) {
			close(entered)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
			}
		}
		return originalDown(ctx, kind, data)
	}
	if err := n.Client(t.Context(), websocket.MessageBinary, cloudGateInput("ordered", "question")); err != nil {
		t.Fatal(err)
	}
	nextCloudGate(t, up)
	nextCloudGate(t, resolver.deciding)
	part := cloudGatePart("ordered", "queued")
	sendCloudGate(t, n, initial, part)
	close(resolver.decision)
	nextCloudGate(t, resolver.executing)
	close(resolver.answer)
	nextCloudGate(t, entered)
	live := cloudGatePart("ordered", "live")
	done := make(chan error, 1)
	go func() { done <- n.Server(t.Context(), websocket.MessageBinary, live) }()
	close(release)
	for i, want := range [][]byte{initial, part, live} {
		if got := nextCloudGate(t, down); !bytes.Equal(got, want) {
			t.Fatalf("live cloud output overtook queued frame %d", i)
		}
	}
	if err := nextCloudGate(t, done); err != nil {
		t.Fatal(err)
	}
}
