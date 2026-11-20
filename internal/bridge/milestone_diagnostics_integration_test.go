package bridge

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	asr "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	eventsource "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/data/event_source"

	"quasarbridge/internal/llm"
)

// Diagnostics control must precede a wakeword's LLM rule without carrying its
// acoustic reply policy into the next command or the next model response.
func TestMilestoneNativeDiagnosticsWakewordWhisper(t *testing.T) {
	t.Setenv("QUASAR_AUTO_WHISPER", "1")
	whisper, err := os.ReadFile("testdata/whisper-aperiodic.ogg")
	if err != nil {
		t.Fatal(err)
	}
	voiced, err := os.ReadFile("testdata/whisper-voiced.ogg")
	if err != nil {
		t.Fatal(err)
	}
	const custom = "Custom milestone diagnostics role."
	const markedAnswer = `<speaker voice="zahar">Готово.`
	const tag = "1fb379a3-a415-567d-9bc5-977cd44928b3"
	var controls, modelCalls atomic.Int64
	var enabled atomic.Bool
	engine := &Engine{SystemPrompt: custom, TTSMarkup: true,
		Diagnostics: func(_ context.Context, value bool) (string, error) {
			controls.Add(1)
			enabled.Store(value)
			if value {
				return "Веб-панель включена.", nil
			}
			return "Веб-панель выключена.", nil
		},
		LLM: completionFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
			modelCalls.Add(1)
			if structured || len(messages) != 2 || messages[0].Role != "system" || !strings.HasPrefix(messages[0].Content, custom) || !strings.Contains(messages[0].Content, `<speaker is_whisper="true">`) || messages[1].Content != "Расскажи про маяк" {
				return "", errors.New("diagnostics reached the model or custom prompt/TTS guidance was lost")
			}
			return markedAnswer, nil
		})}
	routing := DefaultRoutingConfig()
	routing.DefaultRoute, routing.Classifier.Enabled = "yandex", new(false)
	routing.Rules = []RoutingRule{{Name: "jarvis", Route: "llm", Match: RoutingMatch{WakeWords: []string{"джарвис"}}}}
	if err := engine.SetRouting(routing); err != nil {
		t.Fatal(err)
	}
	native, up, down := soundSession(t, engine)
	routes := make(chan [2]string, 4)
	native.OnRoute = func(id, route string) { routes <- [2]string{id, route} }
	// sendMilestoneVoice fixes the normal question; only the two administration
	// requests need their own final ASR text. Both paths reuse stream 55.
	sendControl := func(id, text string, audio []byte) {
		t.Helper()
		input := &request.TAliceRequestProto{
			Request:     &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: new(common.EEventType_voice_input)}},
			EventSource: &eventsource.TEventSource{Id: new(tag), Event: eventsource.TEventSource_Click, Source: eventsource.TEventSource_Hardware, Type: eventsource.TEventSource_Voice},
		}
		message := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id, StreamId: new(int64(55))}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: input}}}}
		if err := native.Client(t.Context(), websocket.MessageBinary, aa(message)); err != nil {
			t.Fatal(err)
		}
		for len(audio) > 0 {
			size := min(len(audio), 509)
			if err := native.Client(t.Context(), websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, 55), audio[:size]...)); err != nil {
				t.Fatal(err)
			}
			audio = audio[size:]
		}
		final := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: id}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new(text)}}}}}}}
		if err := native.Server(t.Context(), websocket.MessageBinary, aa(final)); err != nil {
			t.Fatal(err)
		}
	}
	for _, turn := range []struct {
		id, command, tag, route, answer, speech string
		audio                                   []byte
	}{
		{"enable", "включи веб-интерфейс", tag, "diagnostics", "Веб-панель включена.", `<speaker is_whisper="true">Веб-панель включена.`, whisper},
		{"disable", "выключи веб-интерфейс", tag, "diagnostics", "Веб-панель выключена.", "Веб-панель выключена.", voiced},
		{"model", "", tag, "llm", "Готово.", markedAnswer, voiced},
		{"untagged", "", "", "yandex", "", "", voiced},
	} {
		if turn.command != "" {
			sendControl(turn.id, turn.command, turn.audio)
		} else {
			sendMilestoneVoice(t, native, turn.id, turn.tag, turn.audio)
		}
		select {
		case got := <-routes:
			if got != [2]string{turn.id, turn.route} {
				t.Fatalf("%s route=%v, want %s", turn.id, got, turn.route)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s route missing", turn.id)
		}
		if turn.route == "yandex" {
			break
		}
		if turn.command != "" && (modelCalls.Load() != 0 || enabled.Load() != (turn.id == "enable")) {
			t.Fatalf("%s bypassed local control: model calls=%d, enabled=%t", turn.id, modelCalls.Load(), enabled.Load())
		}
		response := airPlayListeningResponse(t, down)
		if response.GetVoiceResponse().GetOutputSpeech().GetText() != turn.answer || !response.GetVoiceResponse().GetHasVoiceResponse() || !response.GetVoiceResponse().GetShouldListen() || len(response.GetResponse().GetDirectives()) != 0 {
			t.Fatalf("%s lost its plain local confirmation: %v", turn.id, response)
		}
		for {
			frame := nextSoundFrame(t, up)
			event := frame.Client.GetEvent()
			if event.GetContinueRequest() != nil {
				t.Fatalf("%s escaped to Yandex execution", turn.id)
			}
			if input := event.GetVoiceInput(); input != nil && !input.GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
				t.Fatalf("%s was sent without the execution gate", turn.id)
			}
			if synthesis := event.GetTtsGenerate(); synthesis != nil {
				if synthesis.GetText() != turn.speech {
					t.Fatalf("%s synthesis=%q, want %q", turn.id, synthesis.GetText(), turn.speech)
				}
				break
			}
		}
	}
	native.Close()
	continues := 0
	for len(up) > 0 {
		event := (<-up).Client.GetEvent()
		if event.GetTtsGenerate() != nil {
			t.Fatal("untagged request inherited local synthesis")
		}
		if continuation := event.GetContinueRequest(); continuation != nil {
			continues++
			if continuation.GetRequestMessageId() != "untagged" {
				t.Fatal("diagnostics command was forwarded for cloud execution")
			}
		}
	}
	if controls.Load() != 2 || modelCalls.Load() != 1 || continues != 1 {
		t.Fatalf("controls=%d model calls=%d cloud continuations=%d", controls.Load(), modelCalls.Load(), continues)
	}
}
