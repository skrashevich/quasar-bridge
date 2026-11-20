package bridge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	asr "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"quasarbridge/internal/llm"
)

func TestDiagnosticsCommandsHaveDedicatedRoute(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		e := &Engine{Configuration: enabled, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
			t.Error("web-interface command contacted the model")
			return `{"route":"llm"}`, nil
		})}
		cfg := DefaultRoutingConfig()
		cfg.Rules = []RoutingRule{{Name: "catch_all", Route: "llm", Match: RoutingMatch{Regex: ".*"}}}
		if err := e.SetRouting(cfg); err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{"Алиса, включи веб-интерфейс", "Алиса, выключи веб-интерфейс", "отключи веб интерфейс"} {
			if d := e.Decide(t.Context(), text); d.Route != "diagnostics" || d.Source != "bridge_diagnostics" {
				t.Errorf("%q: wrong route %+v", text, d)
			}
		}
	}
}

func TestDiagnosticsCommandsExecuteWithoutModels(t *testing.T) {
	for _, tc := range []struct {
		text    string
		enabled bool
	}{
		{"Алиса, включи веб-интерфейс", true}, {"выключи веб интерфейс", false},
	} {
		var called bool
		e := &Engine{Diagnostics: func(_ context.Context, enabled bool) (string, error) {
			called = true
			if enabled != tc.enabled {
				t.Errorf("enabled=%t, want %t", enabled, tc.enabled)
			}
			return "Готово.", nil
		}, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
			t.Error("direct command contacted LLM")
			return "", errors.New("offline")
		})}
		e.Assistant = e.LLM
		result, err := e.Resolve(t.Context(), tc.text)
		if err != nil || !called || result.Route != "diagnostics" || result.Answer != "Готово." {
			t.Fatalf("wrong direct result: %+v called=%t err=%v", result, called, err)
		}
	}
}

func TestDiagnosticsErrorsDoNotFallbackToModels(t *testing.T) {
	failure := errors.New("listener unavailable")
	for _, control := range []func(context.Context, bool) (string, error){nil, func(context.Context, bool) (string, error) { return "", failure }} {
		e := &Engine{Diagnostics: control, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
			t.Error("failed direct command contacted LLM")
			return "", nil
		})}
		result, err := e.Resolve(t.Context(), "включи веб-интерфейс")
		if err == nil || result.Route != "diagnostics" || result.Answer != "" || (control != nil && !errors.Is(err, failure)) {
			t.Fatalf("wrong failure: %+v %v", result, err)
		}
	}
	e := &Engine{Diagnostics: func(context.Context, bool) (string, error) {
		t.Error("canceled command executed")
		return "", nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := e.Resolve(ctx, "включи веб-интерфейс"); !errors.Is(err, context.Canceled) || result.Route != "diagnostics" {
		t.Fatalf("wrong cancellation: %+v %v", result, err)
	}
}

func TestDiagnosticsCommandsDoNotCaptureOtherRequests(t *testing.T) {
	e := &Engine{Diagnostics: func(context.Context, bool) (string, error) {
		t.Error("indirect command executed")
		return "", nil
	}}
	cfg := DefaultRoutingConfig()
	cfg.Rules = nil
	cfg.DefaultRoute = "yandex"
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"не включи веб-интерфейс", "как включить веб-интерфейс", "«включи веб-интерфейс»", "включи веб-интерфейс и выключи свет", "включи веб интерфейс роутера", "включи веб-камеру"} {
		if result, err := e.Resolve(t.Context(), text); err != nil || result.Route != "yandex" {
			t.Errorf("%q: wrong result %+v %v", text, result, err)
		}
	}
}

func TestNativeDiagnosticsCommandsUseLocalReplyAndTTS(t *testing.T) {
	for _, kind := range []string{"text", "voice"} {
		t.Run(kind, func(t *testing.T) {
			const command = "Алиса, включи веб-интерфейс"
			const answer = "Сетевой доступ к веб-интерфейсу включён."
			var enabled atomic.Bool
			e := &Engine{Diagnostics: func(_ context.Context, value bool) (string, error) {
				enabled.Store(value)
				return answer, nil
			}, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
				t.Error("native direct command contacted model")
				return "", errors.New("offline")
			})}
			n, up, down := soundSession(t, e)
			n.ShouldListen = true
			if kind == "text" {
				if err := n.Client(t.Context(), websocket.MessageBinary, airPlayListeningInput("diagnostics", command)); err != nil {
					t.Fatal(err)
				}
			} else {
				input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
					Header: &header.TEventHeader{MessageId: "diagnostics"},
					Event:  &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{}}}},
				}}}
				if err := n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
					t.Fatal(err)
				}
				result := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
					Header: &header.TEventHeader{RefMessageId: "diagnostics"},
					Event:  &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new(command)}}}},
				}}}
				if err := n.Server(t.Context(), websocket.MessageBinary, aa(result)); err != nil {
					t.Fatal(err)
				}
			}
			response := airPlayListeningResponse(t, down)
			if !enabled.Load() || response.GetVoiceResponse().GetOutputSpeech().GetText() != answer || !response.GetVoiceResponse().GetShouldListen() {
				t.Fatalf("missing normal local confirmation: enabled=%t response=%v", enabled.Load(), response)
			}
			for {
				frame := nextSoundFrame(t, up)
				event := frame.Client.GetEvent()
				if event.GetTextInput() != nil || event.GetContinueRequest() != nil {
					t.Fatal("original command escaped to Yandex execution")
				}
				if input := event.GetVoiceInput(); input != nil && !input.GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
					t.Fatal("voice request was sent without execution approval gate")
				}
				if speech := event.GetTtsGenerate(); speech != nil {
					if speech.GetText() != answer {
						t.Fatalf("TTS did not receive confirmation: %q", speech.GetText())
					}
					break
				}
			}
		})
	}
}

func TestNativeDiagnosticsFailureExplainsWebInterface(t *testing.T) {
	e := &Engine{Diagnostics: func(context.Context, bool) (string, error) { return "", errors.New("bind failed") }}
	n, _, down := soundSession(t, e)
	if err := n.Client(t.Context(), websocket.MessageBinary, airPlayListeningInput("diagnostics-failed", "выключи веб интерфейс")); err != nil {
		t.Fatal(err)
	}
	response := airPlayListeningResponse(t, down)
	if text := response.GetVoiceResponse().GetOutputSpeech().GetText(); text != "Не удалось изменить сетевой доступ к веб-интерфейсу. Проверьте журнал Quasar Bridge." {
		t.Fatalf("misleading failure: %q", text)
	}
}
