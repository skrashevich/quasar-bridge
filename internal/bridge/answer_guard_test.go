package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/skrashevich/quasar-bridge/internal/assistant"
	"github.com/skrashevich/quasar-bridge/internal/llm"
)

type markupChatter struct{ release <-chan struct{} }

func (c markupChatter) Chat(ctx context.Context, _ []llm.Message, _ []llm.Tool) (llm.Message, error) {
	select {
	case <-ctx.Done():
		return llm.Message{}, ctx.Err()
	case <-c.release:
		return llm.Message{Content: `<tool_call><function=read_webpage><parameter=url>https://example.org</parameter></function></tool_call>`}, nil
	}
}

func TestNativeNeverSynthesizesRawToolMarkup(t *testing.T) {
	for _, progress := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "deferred"}[progress], func(t *testing.T) {
			release := make(chan struct{})
			engine := &Engine{Assistant: &assistant.Agent{Client: markupChatter{release}, Tools: &assistant.Tools{}, MaxRounds: 1, MaxCalls: 1}}
			cfg := DefaultRoutingConfig()
			cfg.Rules = nil
			cfg.DefaultRoute = "llm"
			if err := engine.SetRouting(cfg); err != nil {
				t.Fatal(err)
			}
			n, up, down := soundSession(t, engine)
			if progress {
				n.ProgressAfter = time.Millisecond
			}
			if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("malformed-answer")); err != nil {
				t.Fatal(err)
			}
			if progress {
				for {
					f := nextSoundFrame(t, up)
					if f.Kind() == "TtsGenerate" {
						feedSpeech(t, n, f.Client.GetEvent().GetHeader().GetMessageId(), 72, false)
						break
					}
				}
			}
			close(release)
			for {
				f := nextSoundFrame(t, up)
				if tts := f.Client.GetEvent().GetTtsGenerate(); tts != nil {
					if text := tts.GetText(); strings.Contains(text, "<") || strings.Contains(text, "example.org") || !strings.Contains(text, "локальной модели") {
						t.Fatalf("unsafe TTS request: %q", text)
					}
					break
				}
			}
			for {
				f := nextSoundFrame(t, down)
				text := f.Server.GetEvent().GetAliceResponse().GetVoiceResponse().GetOutputSpeech().GetText()
				if part := f.Server.GetEvent().GetDeferredAliceResponse(); part != nil && part.GetIsLast() {
					text = part.GetBaseResponse().GetText()
				}
				if text == "" {
					continue
				}
				if strings.Contains(text, "<tool") || strings.Contains(text, "example.org") {
					t.Fatalf("unsafe downstream response: %q", text)
				}
				break
			}
		})
	}
}
