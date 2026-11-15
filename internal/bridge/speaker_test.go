package bridge

import (
	"context"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/speaker"
)

const (
	fixtureRequestID = "28630110-2f70-4054-823d-c89697a269c5"
	// The first and last sentences of the base system prompt, unchanged by speaker_context.
	promptStart = "Ты голосовой помощник пользователя. Отвечай по-русски"
	promptEnd   = "Для Home Assistant есть отдельный обработчик умного дома."
)

func speakerFrame(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../speaker/testdata/matcheduser-frame.hex")
	if err != nil {
		t.Fatal(err)
	}
	data, err := hex.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// systemPrompt runs the LLM fallback and returns the system prompt it sent.
func systemPrompt(t *testing.T, e *Engine, ctx context.Context) string {
	t.Helper()
	var prompt string
	e.LLM = completionFunc(func(_ context.Context, m []llm.Message, _ bool) (string, error) {
		prompt = m[0].Content
		return "Ответ", nil
	})
	if _, err := e.Fallback(ctx, "Кто я по голосу?"); err != nil {
		t.Fatal(err)
	}
	return prompt
}

func speakerConfig(enabled bool) speaker.Config {
	cfg := speaker.DefaultConfig()
	cfg.Enabled = enabled
	return cfg
}

func recordedContext(t *testing.T, enabled bool, frame []byte) context.Context {
	t.Helper()
	n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, nil, nil)
	t.Cleanup(n.Close)
	n.SpeakerContext = enabled
	if frame != nil {
		if err := n.Client(t.Context(), websocket.MessageBinary, frame); err != nil {
			t.Fatal(err)
		}
	}
	return n.withSpeaker(t.Context(), fixtureRequestID)
}

func TestSpeakerContext_AddsOneSentence(t *testing.T) {
	frame := speakerFrame(t)
	got := systemPrompt(t, &Engine{SpeakerContext: speakerConfig(true)}, recordedContext(t, true, frame))
	want := "Сейчас говорит: взрослый, мужчина. Уверенность: высокая."
	if !strings.HasSuffix(got, "\n"+want) || strings.Count(got, "Сейчас говорит") != 1 {
		t.Fatalf("prompt does not end with the speaker sentence:\n%s", got)
	}
	if !strings.HasPrefix(got, promptStart) || !strings.Contains(got, promptEnd) {
		t.Fatalf("base instructions changed:\n%s", got)
	}
}

func TestSpeakerContext_DisabledKeepsPrompt(t *testing.T) {
	golden := systemPrompt(t, &Engine{}, t.Context())
	if !strings.HasPrefix(golden, promptStart) || !strings.HasSuffix(golden, promptEnd) {
		t.Fatalf("unexpected reference prompt:\n%s", golden)
	}
	frame := speakerFrame(t)
	// The Engine switch is off while a classification is available.
	if got := systemPrompt(t, &Engine{SpeakerContext: speakerConfig(false)}, recordedContext(t, true, frame)); got != golden {
		t.Fatalf("disabled engine changed the prompt:\n%s", got)
	}
	// The Native switch is off, so nothing is recorded.
	if got := systemPrompt(t, &Engine{SpeakerContext: speakerConfig(true)}, recordedContext(t, false, frame)); got != golden {
		t.Fatalf("disabled native changed the prompt:\n%s", got)
	}
}

func TestSpeakerContext_WithoutMatchedUserKeepsPrompt(t *testing.T) {
	golden := systemPrompt(t, &Engine{}, t.Context())
	if got := systemPrompt(t, &Engine{SpeakerContext: speakerConfig(true)}, recordedContext(t, true, nil)); got != golden {
		t.Fatalf("prompt changed without MatchedUser:\n%s", got)
	}
	if got := systemPrompt(t, &Engine{SpeakerContext: speakerConfig(true)}, t.Context()); got != golden {
		t.Fatalf("prompt changed without a lookup:\n%s", got)
	}
}

func TestSpeakerContext_BelowThresholdKeepsPrompt(t *testing.T) {
	golden := systemPrompt(t, &Engine{}, t.Context())
	cfg := speakerConfig(true)
	cfg.MinConfidence = 1
	frame := speakerFrame(t)
	if got := systemPrompt(t, &Engine{SpeakerContext: cfg}, recordedContext(t, true, frame)); got != golden {
		t.Fatalf("low-confidence characteristic was added:\n%s", got)
	}
}
