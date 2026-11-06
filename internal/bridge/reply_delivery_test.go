package bridge

import (
	"context"
	"testing"

	"github.com/coder/websocket"
	"quasarbridge/internal/reply"
)

func TestNativeReplyDeliveryWaitsForSpeechStreamClose(t *testing.T) {
	request := make(chan context.Context, 1)
	n, up, down := soundSession(t, resolverFunc(func(ctx context.Context, _ string) (Resolution, error) {
		// Model completion alone must not release a pending restart.
		model, cancel := context.WithCancel(ctx)
		cancel()
		request <- reply.DeliveryContext(model)
		return Resolution{Route: "llm", Answer: "Перезапуск запланирован."}, nil
	}))
	if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("restart")); err != nil {
		t.Fatal(err)
	}
	delivery := <-request
	for {
		if nextSoundFrame(t, down).Kind() == "AliceResponse" {
			break
		}
	}
	select {
	case <-delivery.Done():
		t.Fatal("delivery completed before TTS")
	default:
	}
	for {
		frame := nextSoundFrame(t, up)
		if frame.Kind() == "TtsGenerate" {
			feedSpeech(t, n, frame.Client.GetEvent().GetHeader().GetMessageId(), 72, false)
			break
		}
	}
	select {
	case <-delivery.Done():
	default:
		t.Fatal("final TTS stream did not release restart")
	}
}
