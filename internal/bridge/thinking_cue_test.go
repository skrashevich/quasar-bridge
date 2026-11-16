package bridge

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestThinkingCueFastAndSlowReply(t *testing.T) {
	for _, fast := range []bool{false, true} {
		t.Run(map[bool]string{false: "slow", true: "fast"}[fast], func(t *testing.T) {
			release := make(chan struct{})
			if fast {
				close(release)
			}
			n, up, down := soundSession(t, delayedRoute{release})
			n.ProgressCue = true
			n.ProgressAfter = time.Hour // Cue starts immediately after the route.
			if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("cue")); err != nil {
				t.Fatal(err)
			}
			setup := nextSoundFrame(t, up).Client.GetEvent()
			if setup.GetTtsGenerate().GetText() != "а" {
				t.Fatal("missing discarded setup")
			}
			feedSpeech(t, n, setup.GetHeader().GetMessageId(), 72, false)
			var audio []byte
			for {
				f := nextSoundFrame(t, down)
				if f.Kind() == "opaque" && len(f.Raw) > 4 {
					audio = append(audio, f.Raw[4:]...)
				}
				if countThinkingCues(audio) == 1 {
					break
				}
			}
			if !fast {
				// Observe the next waiting-audio tick, which must contain only silence.
				for {
					f := nextSoundFrame(t, down)
					if f.Kind() == "opaque" && len(f.Raw) > 4 {
						audio = append(audio, f.Raw[4:]...)
						break
					}
				}
				close(release)
			}
			final := nextSoundFrame(t, up).Client.GetEvent()
			if got := final.GetTtsGenerate().GetText(); got != "Готовый ответ" {
				t.Fatalf("final synthesis=%q", got)
			}
			marker := []byte{0xf8, 0x12, 0x34, 0x56}
			feedSpeech(t, n, final.GetHeader().GetMessageId(), 88, false, marker)
			for {
				f := nextSoundFrame(t, down)
				if f.Kind() == "opaque" && len(f.Raw) > 4 {
					audio = append(audio, f.Raw[4:]...)
				}
				if f.Server.GetStreamControl().GetClose() != nil {
					break
				}
			}
			if countThinkingCues(audio) != 1 || bytes.Count(audio, []byte("OpusHead")) != 1 || bytes.Count(audio, marker) != 1 {
				t.Fatal("cue/final speech repeated or stream headers duplicated")
			}
		})
	}
}

func countThinkingCues(audio []byte) int {
	var packets [][]byte
	for len(audio) >= 27 {
		n := int(audio[26])
		if len(audio) < 27+n {
			return 0
		}
		size := 27 + n
		for _, v := range audio[27 : 27+n] {
			size += int(v)
		}
		if len(audio) < size {
			return 0
		}
		packets = append(packets, audio[27+n:size])
		audio = audio[size:]
	}
	count := 0
	for i := 0; i+len(thinkingCuePackets) <= len(packets); i++ {
		match := true
		for j, p := range thinkingCuePackets {
			if !bytes.Equal(p, packets[i+j]) {
				match = false
				break
			}
		}
		if match {
			count++
		}
	}
	return count
}

func TestThinkingCueContainerAndDuration(t *testing.T) {
	for _, channels := range []byte{1, 2} {
		o := oggSpeech{}
		o.next()
		o.source.discardAudio = true
		if _, err := o.feed(speechFixture(72)); err != nil {
			t.Fatal(err)
		}
		o.head[9] = channels
		cue := o.thinkingCue()
		if countThinkingCues(cue) != 1 || o.granule != 19200 {
			t.Fatal("invalid 400ms cue")
		}
		for len(cue) > 0 {
			n := int(cue[26])
			size := 27 + n
			for _, v := range cue[27 : 27+n] {
				size += int(v)
			}
			page := cue[:size]
			if page[5] != 0 || oggChecksum(page) != binary.LittleEndian.Uint32(page[22:26]) {
				t.Fatal("invalid cue page or premature EOS")
			}
			if samples, err := opusSamples(page[27+n:]); err != nil || samples != 960 {
				t.Fatal("invalid cue frame")
			}
			cue = cue[size:]
		}
	}
}

func TestThinkingCueCancellationBeforeAudio(t *testing.T) {
	release := make(chan struct{})
	n, up, down := soundSession(t, delayedRoute{release})
	n.ProgressCue = true
	n.ProgressAfter = time.Hour
	if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("cue-cancel")); err != nil {
		t.Fatal(err)
	}
	_ = nextSoundFrame(t, up)
	n.Close()
	close(release)
	for len(down) > 0 {
		f := <-down
		if f.Kind() == "opaque" {
			t.Fatal("audio emitted after cancellation")
		}
	}
}

func TestThinkingCueDoesNotStartForYandex(t *testing.T) {
	n, up, down := soundSession(t, immediateRoute{"yandex"})
	n.ProgressCue = true
	n.ProgressAfter = time.Hour
	if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("native")); err != nil {
		t.Fatal(err)
	}
	if f := nextSoundFrame(t, up); f.Kind() != "TextInput" {
		t.Fatalf("native route sent %s", f.Kind())
	}
	n.Close()
	for len(down) > 0 {
		if f := <-down; f.Kind() == "opaque" || f.Server.GetEvent().GetAliceResponse().GetResponse().GetIsStreaming() {
			t.Fatal("cue on native route")
		}
	}
}
