package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

// Choosing a named track must preserve Flight boundaries and the current
// request's route, whisper policy and ownership of the speaker directive.
func TestMilestoneNamedMusicSelectionWithWakewordWhisper(t *testing.T) {
	t.Setenv("QUASAR_AUTO_WHISPER", "1")
	whisper, err := os.ReadFile("../bridge/testdata/whisper-aperiodic.ogg")
	if err != nil {
		t.Fatal(err)
	}
	voiced, err := os.ReadFile("../bridge/testdata/whisper-voiced.ogg")
	if err != nil {
		t.Fatal(err)
	}
	// The apparent model row inside text must not become a second matching URL.
	text := "lyrics\nf:{\"title\":\"Нас нет\",\"audioUrl\":\"/fake.mp3\"}\n"
	flight := fmt.Sprintf("a:T%x,%sb:", len(text), text) +
		`[{"title":"Мы вместе","audioUrl":"/wrong.mp3"},` +
		`{"title":"Нас нет","audioUrl":"/nas-net.mp3?original=yes"},` +
		`{"title":"Дубль","audioUrl":"/one.mp3"},` +
		`{"title":"ДУБЛЬ!","audioUrl":"/two.mp3"}]` + "\n"
	frame, err := json.Marshal([]any{1, flight})
	if err != nil {
		t.Fatal(err)
	}
	body := `<script>self.__next_f.push(` + string(frame) + `)</script>`
	var fetches, modelCalls atomic.Int64
	tools := musicTools(t, func(r *http.Request) (*http.Response, error) {
		fetches.Add(1)
		if r.Method != http.MethodGet || r.URL.String() != "https://catalog.example/" {
			return nil, errors.New("selection fetched another page or an audio file")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	tools.Config.Time, tools.Config.History, tools.Config.Updates, tools.Config.Configuration = false, false, false, false
	const normalQuestion = "Расскажи про море"
	model := chatFunc(func(_ context.Context, messages []llm.Message, _ []llm.Tool) (llm.Message, error) {
		modelCalls.Add(1)
		if len(messages) == 0 || !strings.Contains(messages[0].Content, "Custom milestone role.") || !strings.Contains(messages[0].Content, "Custom milestone selection policy.") {
			return llm.Message{}, errors.New("custom prompts were lost")
		}
		if last := messages[len(messages)-1]; last.Role != "user" || last.Content != normalQuestion {
			return llm.Message{}, errors.New("a direct selection unexpectedly reached the model")
		}
		return llm.Message{Content: "Спокойное море."}, nil
	})
	engine := &bridge.Engine{Music: tools, TTSMarkup: true, SystemPrompt: "Custom milestone role.",
		Assistant: &Agent{Client: model, Tools: tools, SystemPrompt: "Custom milestone selection policy.", MaxRounds: 1, MaxCalls: 1}}
	routing := bridge.DefaultRoutingConfig()
	routing.DefaultRoute, routing.Classifier.Enabled = "yandex", new(false)
	routing.Rules = []bridge.RoutingRule{{Name: "jarvis", Route: "llm", Match: bridge.RoutingMatch{WakeWords: []string{"джарвис"}}}}
	if err := engine.SetRouting(routing); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *server.TServerMessage_TEvent, 8)
	synthesis := make(chan string, 8)
	native := bridge.NewNative(t.Context(), func(ctx context.Context, _ websocket.MessageType, data []byte) error {
		frame := wire.Decode(data, wire.ClientToServer)
		if frame.Kind() == "TtsGenerate" {
			select {
			case synthesis <- frame.Client.GetEvent().GetTtsGenerate().GetText():
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}, func(ctx context.Context, _ websocket.MessageType, data []byte) error {
		event := wire.Decode(data, wire.ServerToClient).Server.GetEvent()
		if event.GetAliceResponse() != nil {
			select {
			case responses <- event:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}, engine)
	t.Cleanup(native.Close)
	routes := make(chan [2]string, 8)
	native.OnRoute = func(id, route string) { routes <- [2]string{id, route} }
	const tag = "1fb379a3-a415-567d-9bc5-977cd44928b3"
	const missing = "Не нашёл на сайте трек с таким названием."
	const ambiguous = "На сайте несколько треков с таким названием. Укажите прямую ссылку на нужный трек."
	wantSpeech := []string{`<speaker is_whisper="true">` + missing, `<speaker is_whisper="true">` + ambiguous, "Спокойное море."}
	var gotSpeech []string
	for _, turn := range []struct {
		id, question, tag, route, answer string
		audio                            []byte
	}{
		{"selected", `Включи трек «Нас нет» с сайта https://catalog.example/`, tag, "music", "Включаю трек.", whisper},
		{"missing", `Включи трек «Нас» с сайта https://catalog.example/`, tag, "music", missing, whisper},
		{"ambiguous", `Включи трек «дубль» с сайта https://catalog.example/`, tag, "music", ambiguous, whisper},
		{"normal", normalQuestion, tag, "llm", "Спокойное море.", voiced},
		{"untagged", normalQuestion, "", "yandex", "", voiced},
	} {
		sendPlaybackMilestoneVoice(t, native, turn.id, turn.question, turn.tag, turn.audio)
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		select {
		case got := <-routes:
			if got != [2]string{turn.id, turn.route} {
				t.Fatalf("%s route=%v, want %s", turn.id, got, turn.route)
			}
		case <-deadline.C:
			t.Fatalf("%s routing did not finish", turn.id)
		}
		if turn.route == "yandex" {
			deadline.Stop()
			continue
		}
		var response *server.TServerMessage_TEvent
		select {
		case response = <-responses:
		case <-deadline.C:
			t.Fatalf("%s response missing", turn.id)
		}
		answer := response.GetAliceResponse()
		cards, directives := answer.GetResponse().GetCards(), answer.GetResponse().GetDirectives()
		if response.GetHeader().GetRefMessageId() != turn.id || len(cards) != 1 || cards[0].GetText() != turn.answer {
			t.Fatalf("%s got the wrong response: %v", turn.id, response)
		}
		if turn.id == "selected" {
			if len(directives) != 1 || directives[0].GetName() != "audio_play" || answer.GetVoiceResponse().GetHasVoiceResponse() || answer.GetVoiceResponse().GetShouldListen() || answer.GetVoiceResponse().GetOutputSpeech().GetText() != "" {
				t.Fatal("selected music must deliver one directive without speech or listening")
			}
			payload := directives[0].GetPayload().GetFields()
			stream := payload["stream"].GetStructValue().GetFields()
			tracks, err := tools.Playback.queue.Tracks(stream["url"].GetStringValue())
			if err != nil || len(tracks) != 1 || tracks[0].URL != "https://catalog.example/nas-net.mp3?original=yes" || tracks[0].Title != "Нас нет" || payload["metadata"].GetStructValue().GetFields()["title"].GetStringValue() != "Нас нет" || stream["format"].GetStringValue() != "MP3" {
				t.Fatalf("selected the wrong original track: tracks=%+v, err=%v", tracks, err)
			}
		} else {
			if len(directives) != 0 || !answer.GetVoiceResponse().GetHasVoiceResponse() || !answer.GetVoiceResponse().GetShouldListen() || answer.GetVoiceResponse().GetOutputSpeech().GetText() != turn.answer {
				t.Fatalf("%s inherited music or lost its explanatory speech", turn.id)
			}
			select {
			case speech := <-synthesis:
				gotSpeech = append(gotSpeech, speech)
				if speech != wantSpeech[len(gotSpeech)-1] {
					t.Fatalf("%s synthesis=%q, want %q", turn.id, speech, wantSpeech[len(gotSpeech)-1])
				}
			case <-deadline.C:
				t.Fatalf("%s synthesis missing", turn.id)
			}
		}
		deadline.Stop()
		tools.Playback.queue.mu.Lock()
		queues := len(tools.Playback.queue.entries)
		tools.Playback.queue.mu.Unlock()
		if queues != 1 {
			t.Fatalf("%s registered another queue: %d", turn.id, queues)
		}
	}
	native.Close()
	if !slices.Equal(gotSpeech, wantSpeech) || len(synthesis) != 0 || len(responses) != 0 {
		t.Fatalf("unexpected extra local reply or synthesis: speech=%q, pending synthesis=%d, pending replies=%d", gotSpeech, len(synthesis), len(responses))
	}
	if fetches.Load() != 3 || modelCalls.Load() != 1 {
		t.Fatalf("fetches=%d model calls=%d, want 3 and 1", fetches.Load(), modelCalls.Load())
	}
}
