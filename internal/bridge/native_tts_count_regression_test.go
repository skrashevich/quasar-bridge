package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	"quasarbridge/internal/wire"
)

// Regression: Yandex ignores TtsGenerate.Text when IsStreaming is true.
// All spoken words, starting at "Один", MUST travel in DeferredTtsGenerate
// on one request. Chunk boundaries must not lose words or punctuation.
func TestNativeTTSCountingUsesOnlyOpeningClauseAndRemainingSentence(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	words := []string{
		"Один, два, три, четыре, пять, шесть, семь, восемь, девять, ",
		"десять, одиннадцать, двенадцать, тринадцать, четырнадцать, пятнадцать, ",
		"шестнадцать, семнадцать, восемнадцать, девятнадцать, двадцать, ",
		"двадцать один, двадцать два, двадцать три, двадцать четыре, ",
		"двадцать пять, двадцать шесть, двадцать семь, двадцать восемь, двадцать девять, тридцать.",
	}
	complete := strings.Join(words, "")
	provider := scriptedNativeStreamingResolver{send: func(_ context.Context, _ string, accept func(string) error) (Resolution, error) {
		for _, text := range words {
			// No sentence-ending period before the final number; the opening comma is an allowed first pause.
			if err := accept(text); err != nil {
				return Resolution{}, err
			}
		}
		return Resolution{Route: "llm", Answer: complete}, nil
	}}
	up := make(chan []byte, 80)
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error {
		select {
		case up <- append([]byte(nil), b...):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, func(_ context.Context, _ websocket.MessageType, _ []byte) error {
		return nil
	}, provider)
	n.StreamSentences = true
	defer n.Close()
	p := &nativeRequest{id: "count-1-30", ctx: ctx, cancel: cancel, request: &request.TAliceRequestProto{}}
	r, err := n.streamSpeechNative(p, "Посчитай до тридцати вслух", provider)
	if err != nil {
		t.Fatal(err)
	}
	if err = n.reply(p, r); err != nil {
		t.Fatal(err)
	}
	tss := 0
	var id string
	var chunks []inspectedDeferredTTS
	for len(up) > 0 {
		data := <-up
		if f := wire.Decode(data, wire.ClientToServer); f.Kind() == "TtsGenerate" {
			tss++
			id = f.Client.GetEvent().GetHeader().GetMessageId()
			if f.Client.GetEvent().GetTtsGenerate().GetText() != "" {
				t.Fatalf("native streaming bootstrap must be empty: %q", f.Client.GetEvent().GetTtsGenerate().GetText())
			}
		} else if c, ok := parseDeferredTTS(t, data); ok {
			chunks = append(chunks, c)
		}
	}
	if tss != 1 {
		t.Fatalf("expected one TtsGenerate; got %d", tss)
	}
	var spoken strings.Builder
	for i, c := range chunks {
		if c.requestID != id || c.chunk != uint64(i+1) {
			t.Fatalf("bad incremental request/order: %+v", c)
		}
		if i == len(chunks)-1 {
			if c.isLast != 1 || c.text != "" {
				t.Fatalf("missing clean IsLast: %+v", c)
			}
		} else {
			if c.isLast != 0 {
				t.Fatal("TTS finalized before all numbers were transmitted")
			}
			spoken.WriteString(c.text)
		}
	}
	if spoken.String() != complete {
		t.Fatalf("spoken text was lost: got %q, want %q", spoken.String(), complete)
	}
	if !strings.HasPrefix(spoken.String(), "Один, два") || !strings.HasSuffix(spoken.String(), "тридцать.") {
		t.Fatal("beginning or end of the count is missing")
	}
	if len(chunks) != 3 {
		t.Fatalf("counting must use one opening clause plus full remaining sentence and IsLast: %+v", chunks)
	}
	if !strings.HasSuffix(chunks[0].text, ",") || !strings.HasSuffix(chunks[1].text, "тридцать.") {
		t.Fatalf("unexpected counting phrase boundaries: %+v", chunks)
	}
}
