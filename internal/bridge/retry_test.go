package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/coder/websocket"
	app "github.com/freequasar/quasar-go-genproto/proto/alice/library/client/protos"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	asr "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
)

func TestRetryAliasesReuseDeliveredQuestion(t *testing.T) {
	for _, command := range []string{"используй нейросеть", "спроси нейросеть", "Алиса, спроси нейросеть!", " СПРОСИ   НЕЙРОСЕТЬ. "} {
		t.Run(command, func(t *testing.T) {
			checks, answers := 0, 0
			e := &Engine{LLM: completionFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
				if structured {
					checks++
					return `{"safe":true}`, nil
				}
				answers++
				if len(messages) != 2 || messages[1].Content != "Почему небо голубое?" {
					t.Fatalf("retry did not reuse the original question: %+v", messages)
				}
				return "Из-за рассеяния света.", nil
			})}
			e.RecordDelivered(t.Context(), "Почему небо голубое?", "Облачный ответ.", true, false)
			decision := e.Decide(t.Context(), command)
			if decision.Source != "dialogue_retry" {
				t.Fatalf("command selected a new query: %+v", decision)
			}
			got, err := e.ResolveDecision(t.Context(), command, decision)
			if err != nil || got.Answer != "Из-за рассеяния света." || checks != 1 || answers != 1 {
				t.Fatalf("result=%+v err=%v checks=%d answers=%d", got, err, checks, answers)
			}
		})
	}
}

func TestRetryRejectsUnapprovedQuestions(t *testing.T) {
	for _, tc := range []struct {
		name, decision string
		err            error
	}{
		{"blocked", `{"safe":false}`, nil},
		{"missing", `{}`, nil},
		{"extra-fields", `{"safe":true,"other":true}`, nil},
		{"malformed", `not json`, nil},
		{"offline", "", errors.New("offline")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := 0
			e := &Engine{LLM: completionFunc(func(_ context.Context, _ []llm.Message, structured bool) (string, error) {
				if !structured {
					t.Fatal("generation ran without a positive eligibility decision")
				}
				checks++
				return tc.decision, tc.err
			})}
			e.RecordDelivered(t.Context(), "Предыдущий вопрос.", "Предыдущий ответ.", true, false)
			got, err := e.Resolve(t.Context(), "спроси нейросеть")
			if err != nil || got.Route != "llm" || got.Answer == "" || checks != 1 {
				t.Fatalf("result=%+v err=%v checks=%d", got, err, checks)
			}
		})
	}
}

func TestRetryAliasDoesNotCaptureNewQuestions(t *testing.T) {
	e := &Engine{}
	got := e.Decide(t.Context(), "спроси нейросеть, почему небо голубое")
	if got.Route != "llm" || got.Source != "rule" {
		t.Fatalf("new question became a retry: %+v", got)
	}
}

func TestRetrySurvivesEmptyPostSpeechListening(t *testing.T) {
	e := &Engine{LLM: completionFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
		if structured {
			return `{"safe":true}`, nil
		}
		if messages[len(messages)-1].Content != "Что такое фотосинтез?" {
			t.Errorf("wrong replay question: %+v", messages)
		}
		return "Растения используют энергию света.", nil
	})}
	ctx := dialogue.WithSpeaker(t.Context(), "device:retry-test-speaker")
	e.RecordDelivered(ctx, "Что такое фотосинтез?", "Облачный ответ о растениях.", true, false)
	n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil },
		func(context.Context, websocket.MessageType, []byte) error { return nil }, e)
	defer n.Close()
	input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: "empty-listening"},
		Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{
			Application: &app.TClientInfoProto{DeviceId: new("retry-test-speaker")},
		}},
	}}}
	if err := n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
		t.Fatal(err)
	}
	final := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
		Header: &header.TEventHeader{RefMessageId: "empty-listening"},
		Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{
			EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("")}},
		}},
	}}}
	if err := n.Server(t.Context(), websocket.MessageBinary, aa(final)); err != nil {
		t.Fatal(err)
	}
	got, err := e.Resolve(ctx, "спроси нейросеть")
	if err != nil || got.Answer != "Растения используют энергию света." {
		t.Fatalf("empty listening lost completed question: %+v, %v", got, err)
	}
}
