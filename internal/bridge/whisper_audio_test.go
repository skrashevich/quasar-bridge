package bridge

import (
	"context"
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"testing"
	"time"

	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
)

func TestAcousticWhisperSignalClasses(t *testing.T) {
	silence := make([]float32, 32000)
	voiced := make([]float32, 32000)
	noise := make([]float32, 32000)
	r := rand.New(rand.NewSource(8))
	for i := range voiced {
		voiced[i] = float32(.01 * math.Sin(2*math.Pi*160*float64(i)/16000))
		noise[i] = float32(.01 * (r.Float64()*2 - 1))
	}
	if acousticWhisper(silence).Whisper {
		t.Fatal("silence classified as whisper")
	}
	if acousticWhisper(voiced).Whisper {
		t.Fatal("voiced speech classified as whisper")
	}
	if !acousticWhisper(noise).Whisper {
		t.Fatal("aperiodic signal not detected")
	}
	// Noise also scores as whisper: caller must require recognized speech. This
	// remains a heuristic, not a validated general-purpose classifier.
	if acousticWhisper(noise[:1000]).Whisper {
		t.Fatal("short input classified")
	}
}
func TestWhisperRejectsMalformedAudio(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("garbage"), make([]byte, 193*1024)} {
		if _, e := decodeWhisperOpus(b); e == nil {
			t.Fatal("accepted malformed input")
		}
	}
}

func TestWhisperOpusDecodeVoiced(t *testing.T) {
	b, e := os.ReadFile("testdata/whisper-voiced.ogg")
	if e != nil {
		t.Fatal(e)
	}
	pcm, e := decodeWhisperOpus(b)
	if e != nil {
		t.Fatal(e)
	}
	if len(pcm) < 15000 || len(pcm) > 18000 {
		t.Fatalf("unexpected sample count %d", len(pcm))
	}
	m := acousticWhisper(pcm)
	if m.Active < 35 || m.Whisper {
		t.Fatalf("decoded tone misclassified: %+v", m)
	}
}

func TestWhisperOpusChainedHeaders(t *testing.T) {
	b, e := os.ReadFile("testdata/whisper-voiced.ogg")
	if e != nil {
		t.Fatal(e)
	}
	b = append(b, append([]byte(nil), b...)...)
	pcm, e := decodeWhisperOpus(b)
	if e != nil {
		t.Fatal(e)
	}
	if len(pcm) < 30000 || len(pcm) > 36000 {
		t.Fatalf("unexpected chained samples %d", len(pcm))
	}
	if acousticWhisper(pcm).Whisper {
		t.Fatal("chained tone classified as whisper")
	}
}

// Automatic whisper must use the same Ogg chain joining as model TTS markup.
func TestAutomaticWhisperJoinsSpeechSegments(t *testing.T) {
	n, up, down := soundSession(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	p := &nativeRequest{ctx: ctx, cancel: cancel, expires: time.NewTimer(time.Hour), id: "auto-whisper", ttsID: "auto-whisper-tts", inputWhisper: true}
	n.requests[p.id] = p
	n.tts[p.ttsID] = p
	if err := n.reply(p, Resolution{Route: "llm", Answer: "Готово."}); err != nil {
		t.Fatal(err)
	}
	generated := nextSoundFrame(t, up).Client.GetEvent()
	if got := generated.GetTtsGenerate().GetText(); got != `<speaker is_whisper="true">Готово.` {
		t.Fatalf("synthesis = %q", got)
	}
	first, second := []byte{0xf8, 0x12, 0x34, 0x56}, []byte{0xf8, 0x65, 0x43, 0x21}
	feedSpeech(t, n, generated.GetHeader().GetMessageId(), 88, true, first, second)
	checkMarkedReply(t, down, "Готово.", first, second)
}

func TestWhisperRejectsTruncatedOgg(t *testing.T) {
	b, err := os.ReadFile("testdata/whisper-voiced.ogg")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"partial payload", b[:len(b)-1]},
		{"partial page header", append(append([]byte(nil), b...), []byte("OggS")...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeWhisperOpus(tc.data); err == nil {
				t.Fatal("truncated audio accepted")
			}
		})
	}
}

func TestWhisperIgnoresCanceledInputStream(t *testing.T) {
	t.Setenv("QUASAR_AUTO_WHISPER", "1")
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "expired"}[expired], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stale := &nativeRequest{ctx: ctx, header: &header.TEventHeader{StreamId: new(int64(17))}, canceled: !expired}
			if expired {
				cancel()
			}
			n := &Native{requests: map[string]*nativeRequest{"stale": stale}}
			chunk := append(binary.BigEndian.AppendUint32(nil, 17), []byte("ogg")...)
			n.collectWhisperAudio(chunk)
			if len(stale.audioInput) != 0 {
				t.Fatal("canceled request collected new audio")
			}
			current := &nativeRequest{ctx: t.Context(), header: &header.TEventHeader{StreamId: new(int64(17))}}
			n.requests["current"] = current
			for range 64 {
				n.collectWhisperAudio(chunk)
			}
			if len(current.audioInput) != 64*3 {
				t.Fatal("reused stream lost audio to a canceled request")
			}
		})
	}
}
