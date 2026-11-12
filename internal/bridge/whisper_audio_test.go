package bridge

import (
	"math"
	"math/rand"
	"os"
	"testing"
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
