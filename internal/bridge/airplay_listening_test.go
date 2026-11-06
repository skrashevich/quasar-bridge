package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"google.golang.org/protobuf/encoding/protowire"
	"quasarbridge/internal/assistant"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

type airPlayListeningController struct {
	controlErr, statusErr error
	wait                  <-chan struct{}
}

func (c airPlayListeningController) Control(ctx context.Context, action string) (string, error) {
	if action == "start" && c.wait != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-c.wait:
		}
	}
	return "Готово.", c.controlErr
}
func (c airPlayListeningController) Active(context.Context) bool { return true }
func (c airPlayListeningController) ControlMedia(context.Context, string) (string, error) {
	return "Готово.", c.controlErr
}
func (c airPlayListeningController) Status(context.Context) (assistant.AirPlayStatus, error) {
	return assistant.AirPlayStatus{Enabled: true, State: "running"}, c.statusErr
}

type airPlayListeningChat struct {
	tool, args string
	wait       <-chan struct{}
	finalErr   error
}

func (c airPlayListeningChat) Chat(ctx context.Context, messages []llm.Message, _ []llm.Tool) (llm.Message, error) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			if messages[i].Content == "информация" {
				return llm.Message{Role: "assistant", Content: "Обычный ответ."}, nil
			}
			break
		}
	}
	if messages[len(messages)-1].Role != "tool" {
		return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "airplay", Type: "function", Function: llm.FunctionCall{Name: c.tool, Arguments: c.args}}}}, nil
	}
	if c.wait != nil {
		select {
		case <-ctx.Done():
			return llm.Message{}, ctx.Err()
		case <-c.wait:
		}
	}
	return llm.Message{Role: "assistant", Content: "Готово."}, c.finalErr
}

func airPlayListeningEngine(t *testing.T, control airPlayListeningController, chat airPlayListeningChat) *Engine {
	t.Helper()
	e := &Engine{AirPlay: control, Assistant: &assistant.Agent{Tools: &assistant.Tools{AirPlay: control}, Client: chat, MaxRounds: 2, MaxCalls: 2}}
	cfg := DefaultRoutingConfig()
	cfg.Rules = nil
	cfg.DefaultRoute = "llm"
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	return e
}

func airPlayListeningInput(id, text string) []byte {
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new(text)}}}}}}})
}

func airPlayListeningResponse(t *testing.T, down <-chan wire.Frame) *response.TAliceResponseProto {
	t.Helper()
	for {
		if r := nextSoundFrame(t, down).Server.GetEvent().GetAliceResponse(); r != nil {
			return r
		}
	}
}

// Missing success-only suppression reopens the microphone after starting AirPlay;
// route/text-based suppression would also break the status, stop and failure cases.
func TestAirPlayStartSuppressesListeningOnlyForSuccessfulRequest(t *testing.T) {
	failure := errors.New("fixture failure")
	for _, tc := range []struct {
		name, text, tool, args          string
		controlErr, statusErr, finalErr error
		disableListening, wantListen    bool
	}{
		{name: "native_start", text: "включи airplay"},
		{name: "native_start_config_disabled", text: "включи airplay", disableListening: true},
		{name: "native_start_failed", text: "включи airplay", controlErr: failure, wantListen: true},
		{name: "native_status", text: "статус airplay", wantListen: true},
		{name: "native_stop", text: "выключи airplay", wantListen: true},
		{name: "native_pause", text: "пауза", wantListen: true},
		{name: "tool_start", tool: "control_airplay", args: `{"action":"start"}`},
		{name: "tool_start_failed", tool: "control_airplay", args: `{"action":"start"}`, controlErr: failure, wantListen: true},
		{name: "tool_status", tool: "get_airplay_status", args: `{}`, wantListen: true},
		{name: "tool_stop", tool: "control_airplay", args: `{"action":"stop"}`, wantListen: true},
		{name: "tool_pause", tool: "control_airplay_playback", args: `{"action":"pause"}`, wantListen: true},
		{name: "tool_start_then_status_error", tool: "control_airplay", args: `{"action":"start"}`, statusErr: failure},
		{name: "tool_start_then_model_error", tool: "control_airplay", args: `{"action":"start"}`, finalErr: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := airPlayListeningEngine(t, airPlayListeningController{controlErr: tc.controlErr, statusErr: tc.statusErr}, airPlayListeningChat{tool: tc.tool, args: tc.args, finalErr: tc.finalErr})
			n, _, down := soundSession(t, engine)
			n.ShouldListen = !tc.disableListening
			text := tc.text
			if text == "" {
				text = "подготовь приёмник по моей просьбе"
			}
			if err := n.Client(t.Context(), websocket.MessageBinary, airPlayListeningInput("airplay", text)); err != nil {
				t.Fatal(err)
			}
			voice := airPlayListeningResponse(t, down).GetVoiceResponse()
			if voice.GetShouldListen() != tc.wantListen {
				t.Fatalf("should_listen=%v, want %v", voice.GetShouldListen(), tc.wantListen)
			}
			if !voice.GetHasVoiceResponse() || voice.GetOutputSpeech().GetText() == "" {
				t.Fatal("AirPlay response lost its spoken confirmation")
			}
			if err := n.Client(t.Context(), websocket.MessageBinary, airPlayListeningInput("next", "информация")); err != nil {
				t.Fatal(err)
			}
			if next := airPlayListeningResponse(t, down).GetVoiceResponse(); next.GetShouldListen() != !tc.disableListening {
				t.Fatal("AirPlay listening policy leaked into the next request")
			}
		})
	}
}

func TestAirPlayStartDeferredResponseKeepsSpeechWithoutListening(t *testing.T) {
	for _, mode := range []string{"native", "tool", "tool_model_error"} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			control := airPlayListeningController{}
			chat := airPlayListeningChat{tool: "control_airplay", args: `{"action":"start"}`}
			text := "подготовь приёмник по моей просьбе"
			if mode == "native" {
				control.wait = release
				text = "включи airplay"
			} else {
				chat.wait = release
				if mode == "tool_model_error" {
					chat.finalErr = errors.New("model failed after starting AirPlay")
				}
			}
			engine := airPlayListeningEngine(t, control, chat)
			n, up, down := soundSession(t, engine)
			n.ProgressAfter = time.Millisecond
			n.ProgressInterval = time.Millisecond
			if err := n.Client(t.Context(), websocket.MessageBinary, airPlayListeningInput("stream", text)); err != nil {
				t.Fatal(err)
			}
			initial := airPlayListeningResponse(t, down)
			if !initial.GetResponse().GetIsStreaming() || initial.GetVoiceResponse().GetShouldListen() {
				t.Fatal("AirPlay streaming envelope reopened listening")
			}
			for {
				f := nextSoundFrame(t, up)
				if f.Kind() == "TtsGenerate" {
					feedSpeech(t, n, f.Client.GetEvent().GetHeader().GetMessageId(), 72, false)
					break
				}
			}
			for parts := 0; parts < 2; {
				if part := nextSoundFrame(t, down).Server.GetEvent().GetDeferredAliceResponse(); part != nil {
					if part.GetIsLast() || part.GetBaseResponse().GetShouldListen() {
						t.Fatal("AirPlay keepalive reopened listening")
					}
					parts++
				}
			}
			// A concurrent request on another socket uses the same Engine and tools.
			// Successful AirPlay start must not change that request's microphone policy.
			other, _, otherDown := soundSession(t, engine)
			if err := other.Client(t.Context(), websocket.MessageBinary, airPlayListeningInput("other", "информация")); err != nil {
				t.Fatal(err)
			}
			if !airPlayListeningResponse(t, otherDown).GetVoiceResponse().GetShouldListen() {
				t.Fatal("AirPlay policy leaked to a concurrent request")
			}
			close(release)
			for {
				part := nextSoundFrame(t, down).Server.GetEvent().GetDeferredAliceResponse()
				if part == nil || !part.GetIsLast() {
					continue
				}
				if part.GetBaseResponse().GetShouldListen() || part.GetBaseResponse().GetText() == "" {
					t.Fatal("AirPlay final response reopened listening or lost confirmation")
				}
				unknown := part.GetBaseResponse().ProtoReflect().GetUnknown()
				num, typ, count := protowire.ConsumeTag(unknown)
				voice, _ := protowire.ConsumeString(unknown[count:])
				if num != 2 || typ != protowire.BytesType || voice == "" {
					t.Fatal("AirPlay final response lost legacy speech")
				}
				break
			}
			for {
				if speech := nextSoundFrame(t, up).Client.GetEvent().GetTtsGenerate(); speech != nil {
					if speech.GetText() == "" {
						t.Fatal("AirPlay confirmation was not synthesized")
					}
					break
				}
			}
		})
	}
}
