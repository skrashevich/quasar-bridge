package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
	app "github.com/skrashevich/quasar-go-genproto/proto/alice/library/client/protos"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

func TestNativeDialogueSurvivesReconnectAndSeparatesDevices(t *testing.T) {
	calls := 0
	e := &Engine{LLM: completionFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
		if structured {
			return `{"route":"yandex"}`, nil
		}
		calls++
		if calls == 2 && (len(m) != 4 || m[1].Content != "о чём мы сегодня общались?") {
			t.Error("reconnected speaker lost context")
		}
		return "Сегодня говорили о музыке", nil
	})}
	turn := func(device, text string) string {
		t.Helper()
		done := make(chan string, 1)
		n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error {
			kind := wire.Decode(b, wire.ClientToServer).Kind()
			if kind == "TtsGenerate" || kind == "TextInput" {
				done <- kind
			}
			return nil
		}, func(context.Context, websocket.MessageType, []byte) error { return nil }, e)
		defer n.Close()
		input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: newID()}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Application: &app.TClientInfoProto{DeviceId: new(device)}, Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new(text)}}}}}}}
		if err := n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
			t.Fatal(err)
		}
		select {
		case k := <-done:
			return k
		case <-time.After(time.Second):
			t.Fatal("request never resolved")
			return ""
		}
	}
	if turn("speaker-a", "о чём мы сегодня общались?") != "TtsGenerate" {
		t.Fatal("first topic not local")
	}
	if turn("speaker-a", "а вчера?") != "TtsGenerate" {
		t.Fatal("followup lost local context")
	}
	if turn("speaker-b", "а вчера?") != "TextInput" || calls != 2 {
		t.Fatal("another device inherited context")
	}
	if turn("", "а вчера?") != "TextInput" {
		t.Fatal("anonymous connection inherited another device")
	}
}
