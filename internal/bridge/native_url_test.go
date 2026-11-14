package bridge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"quasarbridge/internal/assistant"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/llm"
)

func TestNativeSpeechOmitsURLsAndKeepsAnswer(t *testing.T) {
	const spoken = "«Бавария» играет с «Кёльном» в пятницу, 20 ноября, в 22:30 по московскому времени. Матч пройдёт дома, в Мюнхене."
	const answer = spoken + " ([app.fcbayern.com](https://app.fcbayern.com/en/?utm_source=openai))"
	const whisper = `<speaker is_whisper="true">`
	for _, tc := range []struct {
		name     string
		deferred bool
		markup   bool
	}{
		{name: "immediate"},
		{name: "deferred", deferred: true},
		{name: "immediate whisper", markup: true},
		{name: "deferred whisper", deferred: true, markup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			modelContext := make(chan context.Context, 1)
			engine := &Engine{TTSMarkup: tc.markup, LLM: completionFunc(func(ctx context.Context, _ []llm.Message, _ bool) (string, error) {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-release:
				}
				modelContext <- ctx
				if tc.markup {
					return whisper + answer, nil
				}
				return answer, nil
			})}
			cfg := DefaultRoutingConfig()
			cfg.Rules = nil
			cfg.DefaultRoute = "llm"
			if err := engine.SetRouting(cfg); err != nil {
				t.Fatal(err)
			}
			n, up, down := soundSession(t, engine)
			writer := new(journal.Writer)
			t.Cleanup(func() { _ = writer.Close() })
			journalConfig := journal.DefaultConfig()
			journalConfig.Enabled = true
			journalConfig.Path = filepath.Join(t.TempDir(), "conversation.jsonl")
			if err := writer.Configure(journalConfig); err != nil {
				t.Fatal(err)
			}
			recorder := newConversation(writer)
			n.OnRoute = recorder.route
			if tc.deferred {
				n.ProgressAfter = time.Millisecond
			} else {
				close(release)
			}
			input := soundInput("url-answer")
			recorder.client(input)
			if err := n.Client(t.Context(), websocket.MessageBinary, input); err != nil {
				t.Fatal(err)
			}
			if tc.deferred {
				for {
					frame := nextSoundFrame(t, up)
					if event := frame.Client.GetEvent(); event.GetTtsGenerate() != nil {
						feedSpeech(t, n, event.GetHeader().GetMessageId(), 72, false)
						break
					}
				}
				close(release)
			}
			wantSpeech := spoken
			if tc.markup {
				wantSpeech = whisper + spoken
			}
			for {
				frame := nextSoundFrame(t, up)
				event := frame.Client.GetEvent()
				if event.GetTtsGenerate() == nil {
					continue
				}
				if got := event.GetTtsGenerate().GetText(); got != wantSpeech {
					t.Fatalf("synthesis text = %q, want %q", got, wantSpeech)
				}
				feedSpeech(t, n, event.GetHeader().GetMessageId(), 88, true)
				break
			}
			var display, voice string
			for {
				frame := nextSoundFrame(t, down)
				data, err := frame.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				recorder.server(data, true)
				event := frame.Server.GetEvent()
				if response := event.GetAliceResponse(); response != nil && !response.GetResponse().GetIsStreaming() {
					display = response.GetResponse().GetCards()[0].GetText()
					voice = response.GetVoiceResponse().GetOutputSpeech().GetText()
				}
				if response := event.GetDeferredAliceResponse(); response.GetIsLast() {
					display = response.GetBaseResponse().GetText()
					voice = deferredResponseSpeech(response.GetBaseResponse())
				}
				if frame.Server.GetStreamControl().GetClose() != nil {
					break
				}
			}
			if display != answer || voice != spoken {
				t.Errorf("display = %q, voice = %q; want original answer and speech without URLs", display, voice)
			}
			history := engine.dialogue().View(<-modelContext).Messages
			if len(history) != 2 || history[1].Content != answer {
				t.Errorf("dialogue history lost the original source: %+v", history)
			}
			stored, err := assistant.SearchHistory(t.Context(), journalConfig, assistant.HistoryQuery{Limit: 1}, 1)
			if err != nil || len(stored.Items) != 1 || stored.Items[0].Answer != answer {
				t.Errorf("recorded history lost the original source: %+v, err = %v", stored, err)
			}
		})
	}
}
