package bridge

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"quasarbridge/internal/wire"
)

type inspectedDeferredTTS struct {
	requestID, text string
	chunk, isLast   uint64
}

func parseDeferredTTS(t *testing.T, raw []byte) (inspectedDeferredTTS, bool) {
	t.Helper()
	var event client.TClientMessage
	if len(raw) >= 4 && string(raw[:4]) == "AAPI" {
		raw = raw[4:]
	}
	if err := proto.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	e := event.GetEvent()
	if e == nil {
		return inspectedDeferredTTS{}, false
	}
	b := e.ProtoReflect().GetUnknown()
	for len(b) > 0 {
		n, typ, sz := protowire.ConsumeTag(b)
		if sz < 0 {
			t.Fatalf("malformed event unknown: %d", sz)
		}
		b = b[sz:]
		if typ != protowire.BytesType {
			t.Fatal("non-bytes unknown event")
		}
		payload, sz := protowire.ConsumeBytes(b)
		if sz < 0 {
			t.Fatal("malformed deferred unknown bytes")
		}
		b = b[sz:]
		if n != 34 {
			continue
		}
		var got inspectedDeferredTTS
		for len(payload) > 0 {
			field, kind, step := protowire.ConsumeTag(payload)
			if step < 0 {
				t.Fatal("bad deferred field")
			}
			payload = payload[step:]
			switch kind {
			case protowire.BytesType:
				value, k := protowire.ConsumeString(payload)
				if k < 0 {
					t.Fatal("invalid deferred string")
				}
				if field == 1 {
					got.requestID = value
				}
				if field == 2 {
					got.text = value
				}
				payload = payload[k:]
			case protowire.VarintType:
				value, k := protowire.ConsumeVarint(payload)
				if k < 0 {
					t.Fatal("invalid deferred varint")
				}
				if field == 3 {
					got.isLast = value
				}
				if field == 4 {
					got.chunk = value
				}
				payload = payload[k:]
			default:
				t.Fatal("unexpected deferred field type")
			}
		}
		return got, true
	}
	return inspectedDeferredTTS{}, false
}

type scriptedNativeStreamingResolver struct {
	send func(context.Context, string, func(string) error) (Resolution, error)
}

func (s scriptedNativeStreamingResolver) Resolve(context.Context, string) (Resolution, error) {
	return Resolution{Route: "llm"}, nil
}

func (s scriptedNativeStreamingResolver) SupportsSentenceStreaming() bool { return true }

func (s scriptedNativeStreamingResolver) FallbackStream(ctx context.Context, prompt string, delta func(string) error) (Resolution, error) {
	return s.send(ctx, prompt, delta)
}

func TestNativeTTSOneSessionIncrementalBeforeLLMCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	up := make(chan []byte, 40)
	down := make(chan wire.Frame, 40)
	release := make(chan struct{})
	provider := scriptedNativeStreamingResolver{send: func(ctx context.Context, _ string, send func(string) error) (Resolution, error) {
		if err := send("Жил-был часовщик Григорий. "); err != nil {
			return Resolution{}, err
		}
		select {
		case <-release:
		case <-ctx.Done():
			return Resolution{}, ctx.Err()
		}
		if err := send("Он нашёл часы. "); err != nil {
			return Resolution{}, err
		}
		if err := send("А потом случилось чудо."); err != nil {
			return Resolution{}, err
		}
		return Resolution{Route: "llm", Answer: "Жил-был часовщик Григорий. Он нашёл часы. А потом случилось чудо."}, nil
	}}
	n := NewNative(ctx,
		func(_ context.Context, _ websocket.MessageType, b []byte) error {
			select {
			case up <- b:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		func(_ context.Context, _ websocket.MessageType, b []byte) error {
			select {
			case down <- wire.Decode(b, wire.ServerToClient):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, provider)
	n.StreamSentences = true
	defer func() { cancel(); n.Close() }()
	p := &nativeRequest{id: "native-story-test", ctx: ctx, cancel: cancel, request: &request.TAliceRequestProto{}}
	answerDone := make(chan error, 1)
	go func() {
		res, err := n.streamSpeechNative(p, "расскажи длинную историю", provider)
		if err == nil && p.streaming {
			err = n.reply(p, res.Answer)
		}
		answerDone <- err
	}()

	firstID := ""
	for firstID == "" {
		select {
		case raw := <-up:
			f := wire.Decode(raw, wire.ClientToServer)
			if f.Kind() == "TtsGenerate" {
				ts := f.Client.GetEvent().GetTtsGenerate()
				if ts.GetText() != "" {
					t.Fatalf("streaming TtsGenerate must have EMPTY text; initial text is ignored by Yandex: %q", ts.GetText())
				}
				got := ts.ProtoReflect().GetUnknown()
				n, kind, step := protowire.ConsumeTag(got)
				if step < 0 || n != 12 || kind != protowire.VarintType {
					t.Fatalf("initial streaming flag missing: %x", got)
				}
				v, _ := protowire.ConsumeVarint(got[step:])
				if v != 1 {
					t.Fatal("native streaming flag not true")
				}
				firstID = f.Client.GetEvent().GetHeader().GetMessageId()
			}
		case <-ctx.Done():
			t.Fatal("first native TtsGenerate never appeared")
		}
	}
	// The LLM is paused after its first delta. The FIRST spoken words must
	// already be on the wire via DeferredTtsGenerate, not TtsGenerate.Text.
	var chunks []inspectedDeferredTTS
	for len(chunks) == 0 {
		select {
		case raw := <-up:
			if c, ok := parseDeferredTTS(t, raw); ok {
				chunks = append(chunks, c)
			}
		case <-ctx.Done():
			t.Fatal("first deferred TTS delta was not sent before the model completed")
		}
	}
	if !strings.Contains(chunks[0].text, "Жил-был часовщик Григорий.") || chunks[0].chunk != 1 || chunks[0].isLast != 0 {
		t.Fatalf("opening words were lost or incorrectly finalized: %+v", chunks[0])
	}
	select {
	case <-answerDone:
		t.Fatal("LLM completed before first deferred TTS request")
	default:
	}
	close(release)
	for chunks[len(chunks)-1].isLast != 1 {
		select {
		case raw := <-up:
			if c, ok := parseDeferredTTS(t, raw); ok {
				chunks = append(chunks, c)
			} else if wire.Decode(raw, wire.ClientToServer).Kind() == "TtsGenerate" {
				t.Fatal("new TtsGenerate would break native streaming continuity")
			}
		case <-ctx.Done():
			t.Fatalf("missing deferred TTS chunks: %v", chunks)
		}
	}
	var spoken strings.Builder
	for i, c := range chunks {
		if c.requestID != firstID || c.chunk != uint64(i+1) {
			t.Fatalf("deferred request correlation/order broken: %+v", chunks)
		}
		if i != len(chunks)-1 && c.isLast != 0 {
			t.Fatalf("premature IsLast: %+v", chunks)
		}
		if i == len(chunks)-1 {
			if c.text != "" || c.isLast != 1 {
				t.Fatalf("last chunk must only close the session: %+v", c)
			}
			continue
		}
		spoken.WriteString(c.text)
	}
	if got, want := spoken.String(), "Жил-был часовщик Григорий. Он нашёл часы. А потом случилось чудо."; got != want {
		t.Fatalf("native text stream altered or dropped bytes: got %q want %q", got, want)
	}
	select {
	case err := <-answerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("native stream answer not finalized")
	}
	// The real Yandex server returns one native stream, which should close
	// exactly once and never be treated as an intermediate progress segment.
	feedSpeech(t, n, firstID, 501, false)
	tspeaks, closes := 0, 0
	for {
		select {
		case f := <-down:
			if f.Kind() == "TtsSpeak" {
				tspeaks++
			}
			if sc := f.Server.GetStreamControl(); sc != nil && sc.GetClose() != nil {
				closes++
				if sc.GetStreamId() != 501 {
					t.Fatalf("unexpected final stream %d", sc.GetStreamId())
				}
			}
			if closes == 1 {
				if tspeaks != 1 {
					t.Fatalf("expected one TtsSpeak, got %d", tspeaks)
				}
				return
			}
		case <-ctx.Done():
			t.Fatalf("native single TTS stream not finished; tts=%d close=%d", tspeaks, closes)
		}
	}
}

func TestNativeTTSDeferredWireFields(t *testing.T) {
	b, err := nativeDeferredTTSFrame("original-id", "продолжение", 3, true)
	if err != nil {
		t.Fatal(err)
	}
	chunk, ok := parseDeferredTTS(t, b)
	if !ok || chunk.chunk != 3 || chunk.requestID != "original-id" || chunk.isLast != 1 {
		t.Fatalf("invalid protobuf deferred event: %+v", chunk)
	}
	if len(b) < 4 || binary.BigEndian.Uint32(b[:4]) != 0x41415049 {
		t.Fatal("AAPI prefix missing")
	}
}
