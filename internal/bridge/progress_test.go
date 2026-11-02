package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"
	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	stream "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/stream_control"
	tts "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/encoding/protowire"
	"quasarbridge/internal/wire"
)

type delayedRoute struct {
	release <-chan struct{}
	listen  *bool
}

func (d delayedRoute) Decide(context.Context, string) RouteDecision {
	return RouteDecision{Route: "llm"}
}
func (d delayedRoute) Resolve(ctx context.Context, s string) (Resolution, error) {
	return d.ResolveDecision(ctx, s, d.Decide(ctx, s))
}
func (d delayedRoute) ResolveDecision(ctx context.Context, s string, r RouteDecision) (Resolution, error) {
	select {
	case <-ctx.Done():
		return Resolution{}, ctx.Err()
	case <-d.release:
		return Resolution{Route: "llm", Answer: "Готовый ответ", Listen: d.listen}, nil
	}
}
func TestSlowLocalRequestUsesDeferredResponses(t *testing.T) {
	for _, finalStream := range []uint32{72, 88} {
		for _, omitID := range []bool{false, true} {
			for _, voice := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/omit_id=%v/voice=%v", finalStream, omitID, voice), func(t *testing.T) { testSlowLocalRequest(t, finalStream, omitID, voice, true) })
			}
		}
	}
	t.Run("listening_disabled", func(t *testing.T) { testSlowLocalRequest(t, 72, false, true, false) })
	t.Run("assist_closes_microphone", func(t *testing.T) { testSlowLocalRequestClosed(t) })
}
func testSlowLocalRequest(t *testing.T, finalStream uint32, omitID, voice, shouldListen bool) {
	release := make(chan struct{})
	down := make(chan wire.Frame, 20)
	up := make(chan wire.Frame, 20)
	n := NewNative(t.Context(), func(ctx context.Context, k websocket.MessageType, b []byte) error {
		up <- wire.Decode(b, wire.ClientToServer)
		return nil
	}, func(ctx context.Context, k websocket.MessageType, b []byte) error {
		down <- wire.Decode(b, wire.ServerToClient)
		return nil
	}, delayedRoute{release: release})
	defer n.Close()
	n.ShouldListen = shouldListen
	n.ProgressAfter = 5 * time.Millisecond
	n.ProgressInterval = 10 * time.Millisecond
	input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "slow-request"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Header: &request.TAliceRequestProto_THeader{RequestId: new("application-request")}, Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("история")}}}}}}}
	if voice {
		input.GetEvent().Event = &client.TClientMessage_TEvent_VoiceInput{VoiceInput: input.GetEvent().GetTextInput()}
	}
	if err := n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
		t.Fatal(err)
	}
	next := func() wire.Frame {
		t.Helper()
		select {
		case f := <-down:
			return f
		case <-time.After(time.Second):
			t.Fatal("missing progress frame")
			return wire.Frame{}
		}
	}
	if !voice {
		if f := next(); f.Kind() != "InputStartAck" {
			t.Fatalf("first=%s", f.Kind())
		}
	}
	initial := next()
	if initial.Kind() != "AliceResponse" || !initial.Server.GetEvent().GetAliceResponse().GetResponse().GetIsStreaming() {
		t.Fatal("missing initial streaming response")
	}
	if initial.Server.GetEvent().GetAliceResponse().GetVoiceResponse().GetShouldListen() {
		t.Fatal("initial streaming response reopened listening")
	}
	part := next().Server.GetEvent().GetDeferredAliceResponse()
	if part == nil || part.GetIsLast() || part.GetRequestId() != "application-request" || part.GetBaseResponse().GetText() != "" {
		t.Fatal("invalid keepalive")
	}
	if part.GetBaseResponse().GetShouldListen() {
		t.Fatal("keepalive reopened listening")
	}
	var firstTTS wire.Frame
	canceledCloud := false
	for {
		select {
		case firstTTS = <-up:
			if firstTTS.Kind() == "CancelRequest" {
				canceledCloud = true
			}
		case <-time.After(time.Second):
			t.Fatal("missing initial synthesis")
		}
		if firstTTS.Kind() == "TtsGenerate" {
			break
		}
	}
	if voice && !canceledCloud {
		t.Fatal("voice request was not canceled in cloud")
	}
	if firstTTS.Kind() != "TtsGenerate" || firstTTS.Client.GetEvent().GetTtsGenerate().GetText() == "Сейчас проверю." {
		t.Fatal("spoken filler was synthesized")
	}
	setupSpeech := []byte{0xf8, 0x12, 0x34, 0x56}
	feedSpeech(t, n, firstTTS.Client.GetEvent().GetHeader().GetMessageId(), 72, omitID, setupSpeech)
	for len(down) > 0 {
		initial := <-down
		if initial.Kind() == "opaque" && bytes.Contains(initial.Raw, setupSpeech) {
			t.Fatal("setup speech was forwarded to speaker")
		}
		if initial.Kind() == "StreamControl" {
			t.Fatal("initial audio closed")
		}
	}
	close(release)
	for {
		f := next()
		r := f.Server.GetEvent().GetDeferredAliceResponse()
		if r != nil && r.GetIsLast() {
			if got := r.GetBaseResponse().GetShouldListen(); got != shouldListen {
				t.Fatalf("final answer should_listen=%v, want %v", got, shouldListen)
			}
			unknown := r.GetBaseResponse().ProtoReflect().GetUnknown()
			num, typ, count := protowire.ConsumeTag(unknown)
			voice, _ := protowire.ConsumeString(unknown[count:])
			if num != 2 || typ != protowire.BytesType || voice != "Готовый ответ" {
				t.Fatal("missing legacy Voice speech delta")
			}
			if r.GetBaseResponse().GetText() != "Готовый ответ" {
				t.Fatal("wrong final text")
			}
			break
		}
	}
	select {
	case f := <-up:
		if f.Kind() != "TtsGenerate" || f.Client.GetEvent().GetTtsGenerate().GetText() != "Готовый ответ" || f.Client.GetEvent().GetHeader().GetMessageId() == firstTTS.Client.GetEvent().GetHeader().GetMessageId() {
			t.Fatalf("unexpected up=%s", f.Kind())
		}
		feedSpeech(t, n, f.Client.GetEvent().GetHeader().GetMessageId(), finalStream, omitID)
		for {
			out := next()
			if out.Kind() == "TtsSpeak" {
				t.Fatal("final TTS started a second player")
			}
			if close := out.Server.GetStreamControl(); close != nil {
				if close.GetStreamId() != 72 || close.GetMessageId() != "slow-request" {
					t.Fatal("final audio was not correlated")
				}
				break
			}
			if out.Kind() == "opaque" && binary.BigEndian.Uint32(out.Raw) != 72 {
				t.Fatal("final audio did not use original stream")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("no final TTS")
	}
}

func testSlowLocalRequestClosed(t *testing.T) {
	release := make(chan struct{})
	down := make(chan wire.Frame, 20)
	up := make(chan wire.Frame, 20)
	closed := false
	n := NewNative(t.Context(), func(ctx context.Context, k websocket.MessageType, b []byte) error {
		up <- wire.Decode(b, wire.ClientToServer)
		return nil
	}, func(ctx context.Context, k websocket.MessageType, b []byte) error {
		down <- wire.Decode(b, wire.ServerToClient)
		return nil
	}, delayedRoute{release: release, listen: &closed})
	defer n.Close()
	n.ShouldListen = true
	n.ProgressAfter = 5 * time.Millisecond
	n.ProgressInterval = 10 * time.Millisecond
	input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "assist-closed"}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Header: &request.TAliceRequestProto_THeader{RequestId: new("application-request")}, Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("включи свет")}}}}}}}
	if err := n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
		t.Fatal(err)
	}
	var firstTTS wire.Frame
	for {
		select {
		case firstTTS = <-up:
		case <-time.After(time.Second):
			t.Fatal("missing initial synthesis")
		}
		if firstTTS.Kind() == "TtsGenerate" {
			break
		}
	}
	feedSpeech(t, n, firstTTS.Client.GetEvent().GetHeader().GetMessageId(), 72, false)
	close(release)
	for {
		select {
		case f := <-down:
			r := f.Server.GetEvent().GetDeferredAliceResponse()
			if r != nil && r.GetIsLast() {
				if r.GetBaseResponse().GetShouldListen() {
					t.Fatal("assist answer reopened the microphone")
				}
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("missing final answer")
		}
	}
}

func speechFixture(serial uint32) []byte {
	return speechPacketFixture(serial, []byte{0xf8, 0xff, 0xfe})
}
func speechPacketFixture(serial uint32, packet []byte) []byte {
	o := oggSpeech{serial: serial}
	head := append([]byte("OpusHead"), []byte{1, 1, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0}...)
	tags := append([]byte("OpusTags"), make([]byte, 8)...)
	b := o.page(head, 2, 0)
	b = append(b, o.page(tags, 0, 0)...)
	return append(b, o.page(packet, 4, 960)...)
}
func feedSpeech(t *testing.T, n *Native, id string, streamID uint32, omitID bool, packet ...[]byte) {
	t.Helper()
	speak := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: id, StreamId: new(int64(streamID))}, Event: &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &tts.TTtsSpeak{Format: "audio/opus"}}}}}
	if err := n.Server(t.Context(), websocket.MessageBinary, aa(speak)); err != nil {
		t.Fatal(err)
	}
	source := speechFixture(streamID)
	if len(packet) > 0 {
		source = speechPacketFixture(streamID, packet[0])
	}
	audio := append(binary.BigEndian.AppendUint32(nil, streamID), source...)
	if err := n.Server(t.Context(), websocket.MessageBinary, audio); err != nil {
		t.Fatal(err)
	}
	if omitID {
		id = ""
	}
	closeStream := &server.TServerMessage{Message: &server.TServerMessage_StreamControl{StreamControl: &stream.TStreamControl{MessageId: id, StreamId: int64(streamID), Action: &stream.TStreamControl_Close{Close: &stream.TStreamControl_TActionClose{}}}}}
	if err := n.Server(t.Context(), websocket.MessageBinary, aa(closeStream)); err != nil {
		t.Fatal(err)
	}
}

func TestProgressCancellationStopsWithoutFinalAnswer(t *testing.T) {
	release := make(chan struct{})
	down := make(chan wire.Frame, 20)
	n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error {
		down <- wire.Decode(b, wire.ServerToClient)
		return nil
	}, delayedRoute{release: release})
	n.ProgressAfter = time.Millisecond
	n.ProgressInterval = time.Millisecond
	input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "cancel-test"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("история")}}}}}}}
	if err := n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case f := <-down:
			if f.Kind() == "AliceResponse" {
				n.Close()
				goto closed
			}
		case <-time.After(time.Second):
			n.Close()
			t.Fatal("progress never started")
		}
	}
closed:
	close(release)
	for len(down) > 0 {
		f := <-down
		if f.Server.GetEvent().GetDeferredAliceResponse().GetIsLast() {
			t.Fatal("canceled request produced a final answer")
		}
	}
}

func TestFastLocalAndYandexRequestsDoNotStartProgress(t *testing.T) {
	for _, route := range []string{"llm", "yandex"} {
		t.Run(route, func(t *testing.T) {
			down := make(chan wire.Frame, 10)
			finished := make(chan struct{}, 1)
			r := immediateRoute{route}
			n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error {
				if route == "yandex" && wire.Decode(b, wire.ClientToServer).Kind() == "TextInput" {
					finished <- struct{}{}
				}
				return nil
			}, func(_ context.Context, _ websocket.MessageType, b []byte) error {
				f := wire.Decode(b, wire.ServerToClient)
				down <- f
				if f.Kind() == "AliceResponse" {
					finished <- struct{}{}
				}
				return nil
			}, r)
			n.ProgressAfter = time.Hour
			input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "fast-test"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("вопрос")}}}}}}}
			if err := n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("resolution blocked")
			}
			n.Close()
			for len(down) > 0 {
				f := <-down
				if f.Server.GetEvent().GetAliceResponse().GetResponse().GetIsStreaming() || f.Kind() == "DeferredAliceResponse" {
					t.Fatal("unexpected progress")
				}
			}
		})
	}
}

type immediateRoute struct{ route string }

func (r immediateRoute) Decide(context.Context, string) RouteDecision {
	return RouteDecision{Route: r.route}
}
func (r immediateRoute) Resolve(ctx context.Context, s string) (Resolution, error) {
	return r.ResolveDecision(ctx, s, r.Decide(ctx, s))
}
func (r immediateRoute) ResolveDecision(context.Context, string, RouteDecision) (Resolution, error) {
	return Resolution{Route: r.route, Answer: "Быстрый ответ"}, nil
}
