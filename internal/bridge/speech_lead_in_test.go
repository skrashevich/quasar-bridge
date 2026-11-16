package bridge

import (
	"context"
	"testing"

	"github.com/coder/websocket"
)

func TestSpeechLeadInOnlyForModelAnswer(t *testing.T) {
	for _, route := range []string{"llm", "home", "recovery"} {
		t.Run(route, func(t *testing.T) {
			const answer = "Папа купил новую книгу."
			n, up, down := soundSession(t, resolverFunc(func(context.Context, string) (Resolution, error) {
				return Resolution{Route: route, Answer: answer}, nil
			}))
			if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("lead-in")); err != nil {
				t.Fatal(err)
			}
			f := nextSoundFrame(t, up)
			want := answer
			if route == "llm" {
				want = "sil <[150]> " + answer
			}
			if got := f.Client.GetEvent().GetTtsGenerate().GetText(); got != want {
				t.Fatalf("synthesis = %q, want %q", got, want)
			}
			for {
				f := nextSoundFrame(t, down)
				if r := f.Server.GetEvent().GetAliceResponse(); r != nil {
					if got := r.GetVoiceResponse().GetOutputSpeech().GetText(); got != answer {
						t.Fatalf("displayed answer = %q", got)
					}
					break
				}
			}
		})
	}
}
