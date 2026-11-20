package assistant

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
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
	"google.golang.org/protobuf/proto"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

// The real assistant, page/script reader and Native deliver media on the same
// session as whisper TTS. External model and web responses are the only fixtures.
func TestMilestoneMusicPlaybackWithWakewordPromptsWhisper(t *testing.T) {
	t.Setenv("QUASAR_AUTO_WHISPER", "1")
	whisper, err := os.ReadFile("../bridge/testdata/whisper-aperiodic.ogg")
	if err != nil {
		t.Fatal(err)
	}
	voiced, err := os.ReadFile("../bridge/testdata/whisper-voiced.ogg")
	if err != nil {
		t.Fatal(err)
	}
	var fetched []string
	tools := musicTools(t, func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.String())
		typ, body := "text/html", `<audio></audio><script src="/player.js"></script>`
		switch r.URL.String() {
		case "https://radio.example/":
		case "https://radio.example/player.js":
			typ, body = "application/javascript", `const audioUrl = 'https://cdn.example/live.m3u8';`
		default:
			return nil, errors.New("unexpected music fetch")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {typ}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	tools.Config.Time, tools.Config.History, tools.Config.Updates, tools.Config.Configuration = false, false, false, false
	tools.TTSMarkup = true
	const normalQuestion = "Расскажи про море"
	// A natural request with a leading pronoun uses the real LLM tool path,
	// rather than the shortcut for commands beginning with "включи".
	const musicQuestion = "Для меня включи радио с сайта https://radio.example/"
	calls := 0
	model := chatFunc(func(_ context.Context, messages []llm.Message, definitions []llm.Tool) (llm.Message, error) {
		calls++
		if len(messages) == 0 || !strings.Contains(messages[0].Content, "Custom milestone role.") || !strings.Contains(messages[0].Content, "Custom milestone music policy.") {
			return llm.Message{}, errors.New("custom system prompts were lost")
		}
		last := messages[len(messages)-1]
		if last.Role == "tool" {
			var result struct {
				Status string `json:"status"`
				Count  int    `json:"track_count"`
			}
			if err := json.Unmarshal([]byte(last.Content), &result); err != nil || last.ToolCallID != "play-radio" || result.Status != "queued" || result.Count != 1 {
				return llm.Message{}, errors.New("real play_music did not queue the discovered track")
			}
			return llm.Message{Content: `<speaker voice="zahar">Включаю радио.`}, nil
		}
		if last.Content == musicQuestion {
			if !slices.ContainsFunc(definitions, func(tool llm.Tool) bool { return tool.Function.Name == "play_music" }) {
				return llm.Message{}, errors.New("play_music was not offered")
			}
			return llm.Message{ToolCalls: []llm.ToolCall{{ID: "play-radio", Type: "function", Function: llm.FunctionCall{Name: "play_music", Arguments: `{"source":"https://radio.example/"}`}}}}, nil
		}
		if last.Role != "user" || last.Content != normalQuestion {
			return llm.Message{}, errors.New("unexpected model input")
		}
		return llm.Message{Content: "Спокойное море."}, nil
	})
	engine := &bridge.Engine{SystemPrompt: "Custom milestone role.", TTSMarkup: true, Music: tools,
		Assistant: &Agent{Client: model, Tools: tools, SystemPrompt: "Custom milestone music policy.", MaxRounds: 2, MaxCalls: 1}}
	routing := bridge.DefaultRoutingConfig()
	routing.DefaultRoute = "yandex"
	routing.Classifier.Enabled = new(false)
	routing.Rules = []bridge.RoutingRule{{Name: "jarvis", Route: "llm", Match: bridge.RoutingMatch{WakeWords: []string{"джарвис"}}}}
	if err := engine.SetRouting(routing); err != nil {
		t.Fatal(err)
	}
	var sentMu sync.Mutex
	var sent, delivered []wire.Frame
	down := make(chan wire.Frame, 32)
	synthesized := make(chan string, 4)
	native := bridge.NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, data []byte) error {
		sentMu.Lock()
		frame := wire.Decode(data, wire.ClientToServer)
		sent = append(sent, frame)
		sentMu.Unlock()
		if frame.Kind() == "TtsGenerate" {
			synthesized <- frame.Client.GetEvent().GetTtsGenerate().GetText()
		}
		return nil
	}, func(_ context.Context, _ websocket.MessageType, data []byte) error {
		frame := wire.Decode(data, wire.ServerToClient)
		sentMu.Lock()
		delivered = append(delivered, frame)
		sentMu.Unlock()
		down <- frame
		return nil
	}, engine)
	t.Cleanup(native.Close)
	routes := make(chan string, 4)
	native.OnRoute = func(_, route string) { routes <- route }
	const wakewordTag = "1fb379a3-a415-567d-9bc5-977cd44928b3"
	for _, turn := range []struct {
		id, text, tag string
		audio         []byte
	}{
		{"whisper-before-media", normalQuestion, wakewordTag, whisper},
		{"music", musicQuestion, wakewordTag, whisper},
		{"normal-after-media", normalQuestion, wakewordTag, voiced},
		{"untagged", normalQuestion, "", voiced},
	} {
		sendPlaybackMilestoneVoice(t, native, turn.id, turn.text, turn.tag, turn.audio)
		wantRoute := "llm"
		if turn.tag == "" {
			wantRoute = "yandex"
		}
		select {
		case route := <-routes:
			if route != wantRoute {
				t.Fatalf("%s route=%q, want %q", turn.id, route, wantRoute)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s route did not finish", turn.id)
		}
		if turn.tag == "" {
			continue
		}
		var response *server.TServerMessage_TEvent
		for response.GetAliceResponse() == nil {
			select {
			case frame := <-down:
				response = frame.Server.GetEvent()
			case <-time.After(3 * time.Second):
				t.Fatalf("%s final response missing", turn.id)
			}
		}
		answer := response.GetAliceResponse()
		if response.GetHeader().GetRefMessageId() != turn.id {
			t.Fatalf("reply correlated to the wrong request: %s", turn.id)
		}
		if turn.id == "music" {
			if answer.GetVoiceResponse().GetHasVoiceResponse() || answer.GetVoiceResponse().GetShouldListen() || answer.GetVoiceResponse().GetOutputSpeech().GetText() != "" {
				t.Fatal("music reopened listening or retained spoken confirmation")
			}
			cards, directives := answer.GetResponse().GetCards(), answer.GetResponse().GetDirectives()
			if len(cards) != 1 || cards[0].GetText() != "Включаю радио." || len(directives) != 1 || directives[0].GetName() != "audio_play" {
				t.Fatal("music must have one plain-text card and one audio_play directive")
			}
			stream := directives[0].GetPayload().GetFields()["stream"].GetStructValue().GetFields()
			if stream["url"].GetStringValue() != "https://cdn.example/live.m3u8" || stream["format"].GetStringValue() != "HLS" {
				t.Fatal("discovered HLS source did not reach the speaker")
			}
		} else {
			if !answer.GetVoiceResponse().GetHasVoiceResponse() || answer.GetVoiceResponse().GetOutputSpeech().GetText() != "Спокойное море." || len(answer.GetResponse().GetDirectives()) != 0 {
				t.Fatal("ordinary reply lost its plain speech or inherited media")
			}
			want := "Спокойное море."
			if turn.id == "whisper-before-media" {
				want = `<speaker is_whisper="true">` + want
			}
			select {
			case got := <-synthesized:
				if got != want {
					t.Fatalf("%s synthesis=%q, want %q", turn.id, got, want)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("%s speech missing", turn.id)
			}
		}
	}
	native.Close()
	sentMu.Lock()
	defer sentMu.Unlock()
	var synthesis []string
	for _, frame := range sent {
		if frame.Kind() == "TtsGenerate" {
			synthesis = append(synthesis, frame.Client.GetEvent().GetTtsGenerate().GetText())
		}
	}
	if !slices.Equal(synthesis, []string{`<speaker is_whisper="true">Спокойное море.`, "Спокойное море."}) {
		t.Fatalf("music synthesized speech or whisper leaked into next request: %q", synthesis)
	}
	plays := 0
	for _, frame := range delivered {
		for _, directive := range frame.Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives() {
			if directive.GetName() == "audio_play" {
				plays++
			}
		}
	}
	if plays != 1 {
		t.Fatalf("audio_play deliveries=%d, want one", plays)
	}
	if calls != 4 || !slices.Equal(fetched, []string{"https://radio.example/", "https://radio.example/player.js"}) {
		t.Fatalf("unexpected model calls=%d or page/script fetches=%v", calls, fetched)
	}
}

func sendPlaybackMilestoneVoice(t *testing.T, native *bridge.Native, id, text, tag string, audio []byte) {
	t.Helper()
	send := func(message proto.Message, clientSide bool) {
		data, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		data = append([]byte("AAPI"), data...)
		if clientSide {
			err = native.Client(t.Context(), websocket.MessageBinary, data)
		} else {
			err = native.Server(t.Context(), websocket.MessageBinary, data)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	input := &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: new(common.EEventType_voice_input)}}}
	if tag != "" {
		input.EventSource = &eventsource.TEventSource{Id: new(tag), Event: eventsource.TEventSource_Click, Source: eventsource.TEventSource_Hardware, Type: eventsource.TEventSource_Voice}
	}
	send(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id, StreamId: new(int64(55))}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: input}}}}, true)
	for len(audio) > 0 {
		size := min(len(audio), 509)
		if err := native.Client(t.Context(), websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, 55), audio[:size]...)); err != nil {
			t.Fatal(err)
		}
		audio = audio[size:]
	}
	send(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: id}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new(text)}}}}}}}, false)
}
