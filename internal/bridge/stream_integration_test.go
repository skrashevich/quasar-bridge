package bridge

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

type streamTransport struct{ reader io.ReadCloser }

func (s streamTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: s.reader, Header: make(http.Header)}, nil
}

type streamResolver struct{ model *llm.Client }

func (r streamResolver) Decide(context.Context, string) RouteDecision {
	return RouteDecision{Route: "llm"}
}
func (r streamResolver) Resolve(ctx context.Context, text string) (Resolution, error) {
	return r.ResolveDecision(ctx, text, r.Decide(ctx, text))
}
func (r streamResolver) ResolveDecision(ctx context.Context, text string, _ RouteDecision) (Resolution, error) {
	answer, err := r.model.Complete(ctx, []llm.Message{{Role: "user", Content: text}}, false)
	return Resolution{Route: "llm", Answer: answer}, err
}

func TestLLMSpeechStartsBeforeModelCompletes(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	model := &llm.Client{API: "chatgpt", Model: "test", HTTP: &http.Client{Transport: streamTransport{reader}},
		AccessToken: func(context.Context) (string, error) { return "fixture", nil }}
	up, down := make(chan wire.Frame, 16), make(chan wire.Frame, 64)
	n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error {
		up <- wire.Decode(b, wire.ClientToServer)
		return nil
	}, func(_ context.Context, _ websocket.MessageType, b []byte) error {
		down <- wire.Decode(b, wire.ServerToClient)
		return nil
	}, streamResolver{model})
	defer n.Close()
	n.StreamLLMSpeech = true
	n.ProgressAfter = time.Hour
	input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: "stream-request"},
		Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{
			Header:  &request.TAliceRequestProto_THeader{RequestId: new("app-request")},
			Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("расскажи")}},
		}},
	}}}
	if err := n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Первая фраза. \"}\n\n")
	}()
	nextTTS := func() wire.Frame {
		t.Helper()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		for {
			select {
			case f := <-up:
				if f.Kind() == "TtsGenerate" {
					return f
				}
			case <-timer.C:
				t.Fatal("missing TTS")
			}
		}
	}
	setup := nextTTS()
	feedSpeech(t, n, setup.Client.GetEvent().GetHeader().GetMessageId(), 72, false)
	first := nextTTS()
	if got := first.Client.GetEvent().GetTtsGenerate().GetText(); got != "Первая фраза." {
		t.Fatalf("first speech = %q", got)
	}
	feedSpeech(t, n, first.Client.GetEvent().GetHeader().GetMessageId(), 88, false)
	// The model has not sent response.completed yet; seeing first TTS proves
	// that speech starts before the full answer has been received.
	go func() {
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Вторая фраза.\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Первая фраза. Вторая фраза.\"}]}]}}\n\n")
	}()
	second := nextTTS()
	if got := second.Client.GetEvent().GetTtsGenerate().GetText(); !strings.Contains(got, "Вторая") {
		t.Fatalf("second speech = %q", got)
	}
	feedSpeech(t, n, second.Client.GetEvent().GetHeader().GetMessageId(), 89, false)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case f := <-down:
			if f.Server.GetStreamControl().GetClose() != nil {
				return
			}
		case <-timer.C:
			t.Fatal("stream did not finish")
		}
	}
}
