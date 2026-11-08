package bridge

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

func TestLLMTTSMarkupDisabled(t *testing.T) {
	const marked = `<speaker is_whisper="true" pitch="high">Зам+ок. sil <[500]>`
	const plain = "Замок."
	engine := &Engine{LLM: completionFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
		if structured {
			return `{}`, nil
		}
		if strings.Contains(messages[0].Content, "<speaker") {
			t.Error("TTS instructions were sent with markup disabled")
		}
		return marked, nil
	})}
	ctx := dialogue.WithSpeaker(t.Context(), "tts-markup-disabled")
	first, err := engine.Fallback(ctx, "Расскажи про замок шёпотом")
	if err != nil || first.Answer != plain || first.speech != "" {
		t.Fatalf("answer with markup disabled = %+v, err = %v", first, err)
	}

	engine.RecordDelivered(engine.Prepare(ctx), "Расскажи про замок шёпотом", "Не могу ответить.", true, false)
	retry, err := engine.Resolve(ctx, "Спроси нейросеть")
	if err != nil || retry.Answer != plain || retry.speech != "" {
		t.Fatalf("retry with markup disabled = %+v, err = %v", retry, err)
	}
}

func TestLocalTTSMarkupStaysInSynthesis(t *testing.T) {
	const speech = `<speaker is_whisper="true">П+ауза sil <[500]> и транскрипция <[t r a n s k rr ii p c y j schwa]>. <speaker voice='zahar' effect="megaphone" emotion="good" speed="1.2">Готово. <speaker effect="-" audio="alice-sounds-game-win-1.opus" lang="ru-RU">Дальше C++ и 2+2.`
	const plain = "Пауза и транскрипция. Готово. Дальше C++ и 2+2."

	for _, name := range []string{"immediate", "deferred"} {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			modelContext := make(chan context.Context, 1)
			engine := &Engine{TTSMarkup: true, LLM: completionFunc(func(ctx context.Context, messages []llm.Message, structured bool) (string, error) {
				if structured {
					return `{"route":"llm"}`, nil
				}
				if !strings.Contains(messages[0].Content, `<speaker is_whisper="true">`) {
					t.Error("the model was not instructed how to request whisper")
				}
				if !strings.Contains(messages[0].Content, `<speaker effect="megaphone">`) {
					t.Error("the model was not instructed how to apply an effect to the default voice")
				}
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-release:
					modelContext <- ctx
					return speech, nil
				}
			})}
			n, up, down := soundSession(t, engine)
			if name == "deferred" {
				n.ProgressAfter = time.Millisecond
			} else {
				close(release)
			}
			if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("tts-markup")); err != nil {
				t.Fatal(err)
			}
			if name == "deferred" {
				setup := nextSoundFrame(t, up).Client.GetEvent()
				if setup.GetTtsGenerate().GetText() != "а" {
					t.Fatal("missing deferred Opus setup")
				}
				feedSpeech(t, n, setup.GetHeader().GetMessageId(), 72, false)
				close(release)
			}
			final := nextSoundFrame(t, up).Client.GetEvent()
			if got := final.GetTtsGenerate().GetText(); got != speech {
				t.Fatalf("synthesis text = %q, want %q", got, speech)
			}
			firstPacket := []byte{0xf8, 0x12, 0x34, 0x56}
			secondPacket := []byte{0xf8, 0x65, 0x43, 0x21}
			feedSpeech(t, n, final.GetHeader().GetMessageId(), 88, true, firstPacket, secondPacket)
			checkMarkedReply(t, down, plain, firstPacket, secondPacket)
			messages := engine.dialogue().View(<-modelContext).Messages
			if len(messages) != 2 || messages[1].Content != plain {
				t.Fatalf("dialogue history = %+v, want the plain answer", messages)
			}
		})
	}
}

func checkMarkedReply(t *testing.T, down <-chan wire.Frame, plain string, firstPacket, secondPacket []byte) {
	t.Helper()
	var audio []byte
	var displayed string
	for {
		frame := nextSoundFrame(t, down)
		if frame.Kind() == "opaque" && len(frame.Raw) >= 4 {
			audio = append(audio, frame.Raw[4:]...)
		}
		event := frame.Server.GetEvent()
		if response := event.GetAliceResponse(); response != nil && !response.GetResponse().GetIsStreaming() {
			displayed = response.GetResponse().GetCards()[0].GetText()
		}
		if response := event.GetDeferredAliceResponse(); response.GetIsLast() {
			displayed = response.GetBaseResponse().GetText()
		}
		if frame.Server.GetStreamControl().GetClose() != nil {
			break
		}
	}
	if displayed != plain {
		t.Errorf("displayed text = %q, want %q", displayed, plain)
	}
	first, second := bytes.Index(audio, firstPacket), bytes.Index(audio, secondPacket)
	if first < 0 || second <= first || bytes.Count(audio, []byte("OpusHead")) != 1 {
		t.Fatal("both speech segments must reach the speaker in a single Opus stream")
	}
}
