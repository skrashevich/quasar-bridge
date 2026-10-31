package bridge

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	asr "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"

	"quasarbridge/internal/journal"
)

func asrFrame(texts []string) []byte {
	candidates := make([]*recognition.TAsrRecognition, len(texts))
	for i, text := range texts {
		candidates[i] = &recognition.TAsrRecognition{Normalized: new(text)}
	}
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "voice"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: candidates}}}}})
}

func TestNativeASRRecoversOnlyUniqueWebsiteSource(t *testing.T) {
	const top = "Включи музыку с сайта abc точка слух"
	for _, tc := range []struct {
		name  string
		texts []string
		want  string
	}{
		{"recover", []string{top, "Включи музыку с сайта abc точка су"}, "Включи музыку с сайта https://abc.su"},
		{"observed suffix error", []string{"Включи музыку с сайта свк точка слух", "Включи музыку с сайта свк точка су"}, "Включи музыку с сайта https://svk.su"},
		{"same canonical alternatives", []string{top, "Включи музыку с сайта abc точка су", "включи музыку с сайта abc.su"}, "Включи музыку с сайта https://abc.su"},
		{"known top", []string{"Включи музыку с сайта example.org", "Включи музыку с сайта other.org"}, "Включи музыку с сайта example.org"},
		{"ambiguous", []string{top, "Включи музыку с сайта abc.su", "Включи музыку с сайта abc.ru"}, top},
		{"unrelated stem", []string{"Включи музыку с сайта альфа точка слух", "Включи музыку с сайта other.org"}, "Включи музыку с сайта альфа точка слух"},
		{"different port", []string{top + ":8000/music", "Включи музыку с сайта abc.su:80/music"}, top + ":8000/music"},
		{"different command", []string{top, "Выключи музыку с сайта abc.su"}, top},
		{"changed words", []string{top, "Включи радио с сайта abc.su"}, top},
		{"negated top", []string{"Не включи музыку с сайта abc точка слух", "Включи музыку с сайта abc.su"}, "Не включи музыку с сайта abc точка слух"},
		{"negated source", []string{"Включи музыку не с сайта abc точка слух", "Включи музыку с сайта abc.su"}, "Включи музыку не с сайта abc точка слух"},
		{"ordinary speech", []string{"Расскажи про abc точка слух", "Расскажи про example.org"}, "Расскажи про abc точка слух"},
		{"unreliable alternative", []string{top, "Включи музыку с сайта abc.invalid"}, top},
		{"credentials", []string{top, "Включи музыку с сайта https://user:pass@abc.su"}, top},
		{"private address", []string{top, "Включи музыку с сайта http://127.0.0.1"}, top},
		{"suffix change", []string{top + "!", "Включи музыку с сайта abc.su?"}, top + "!"},
		{"preserve source suffix", []string{"Включи музыку с сайта abc точка слух!", "Включи музыку с сайта abc точка су!"}, "Включи музыку с сайта https://abc.su!"},
		{"recover same path", []string{top + " слэш Music?album=1", "Включи музыку с сайта abc точка су слэш Music?album=1"}, "Включи музыку с сайта https://abc.su/Music?album=1"},
		{"different path", []string{top + "/music", "Включи музыку с сайта abc.su/other"}, top + "/music"},
		{"different query", []string{top + "/music?album=1", "Включи музыку с сайта abc.su/music?album=2"}, top + "/music?album=1"},
		{"scheme downgrade", []string{"Включи музыку с сайта https://abc.invalid/music", "Включи музыку с сайта http://abc.su/music"}, "Включи музыку с сайта https://abc.invalid/music"},
		{"bounded selection", []string{top, "Включи музыку с сайта abc.su", "Включи музыку с сайта abc.su", "Включи музыку с сайта abc.su", "Включи музыку с сайта abc.su", "Включи музыку с сайта abc.su", "Включи музыку с сайта abc.su", "Включи музыку с сайта abc.su", "Включи музыку с сайта other.org"}, top},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			textSeen := make(chan string, 1)
			n := NewNative(ctx, func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, resolverFunc(func(_ context.Context, text string) (Resolution, error) {
				textSeen <- text
				return Resolution{Route: "yandex"}, nil
			}))
			defer n.Close()
			input := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "voice", StreamId: new(int64(1))}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_voice_input.Enum()}}}}}}})
			if err := n.Client(ctx, websocket.MessageBinary, input); err != nil {
				t.Fatal(err)
			}
			if err := n.Server(ctx, websocket.MessageBinary, asrFrame(tc.texts)); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-textSeen:
				if got != tc.want {
					t.Fatalf("got %q want %q", got, tc.want)
				}
			case <-ctx.Done():
				t.Fatal("no ASR decision")
			}
		})
	}
}

func TestConversationRecordsSelectedASRAndOriginalHypotheses(t *testing.T) {
	cfg := journal.DefaultConfig()
	cfg.Enabled = true
	cfg.Path = filepath.Join(t.TempDir(), "conversation.jsonl")
	writer := new(journal.Writer)
	if err := writer.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	c := newConversation(writer)
	c.server(asrFrame([]string{"Включи музыку с сайта abc точка слух", "Включи музыку с сайта abc точка су"}), false)
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []journal.Event
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		var e journal.Event
		if err = json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, e)
	}
	if len(rows) != 3 || rows[0].Event != "asr_hypothesis" || rows[0].Text != "Включи музыку с сайта abc точка слух" || rows[1].Text != "Включи музыку с сайта abc точка су" || rows[2].Event != "transcript" || rows[2].Text != "Включи музыку с сайта https://abc.su" {
		t.Fatalf("ASR evidence or selected transcript lost: %+v", rows)
	}
}
