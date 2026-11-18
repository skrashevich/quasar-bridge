package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func waitMusicOgg(channels, mapping byte, eos bool, packets ...[]byte) []byte {
	o := oggSpeech{serial: 17}
	head := append([]byte("OpusHead"), 1, channels, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, mapping)
	out := o.page(head, 2, 0)
	out = append(out, o.page(append([]byte("OpusTags"), make([]byte, 8)...), 0, 0)...)
	var granule uint64
	for i, packet := range packets {
		samples, _ := opusSamples(packet)
		granule += samples
		var flags byte
		if eos && i == len(packets)-1 {
			flags = 4
		}
		out = append(out, o.page(packet, flags, granule)...)
	}
	return out
}

func TestLoadWaitMusic(t *testing.T) {
	short := []byte{0xf0, 0x21, 0x41}
	normal := []byte{0xf8, 0x24, 0x48}
	valid := waitMusicOgg(1, 0, true, short, normal)
	path := filepath.Join(t.TempDir(), "waiting.opus")
	if err := os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	track, err := LoadWaitMusic(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(track.packets) != 2 || !bytes.Equal(track.packets[0].data, short) ||
		track.packets[0].samples != 480 || track.packets[1].samples != 960 {
		t.Fatalf("unexpected track packets: %#v", track.packets)
	}

	badCRC := bytes.Clone(valid)
	badCRC[len(badCRC)-1] ^= 1
	for name, data := range map[string][]byte{
		"empty":        nil,
		"wrong format": []byte("ID3"),
		"no packets":   waitMusicOgg(1, 0, false),
		"stereo":       waitMusicOgg(2, 0, true, normal),
		"mapping":      waitMusicOgg(1, 1, true, normal),
		"invalid opus": waitMusicOgg(1, 0, true, []byte{3}),
		"truncated":    valid[:len(valid)-1],
		"missing EOS":  waitMusicOgg(1, 0, false, normal),
		"bad checksum": badCRC,
		"too large":    make([]byte, maxWaitMusicBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "waiting.opus")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadWaitMusic(path); err == nil {
				t.Fatal("accepted an unusable track")
			}
		})
	}
	for _, invalid := range []string{filepath.Dir(path), path + ".missing"} {
		if _, err := LoadWaitMusic(invalid); err == nil {
			t.Fatalf("accepted nonregular or missing path %q", invalid)
		}
	}
}

func TestWaitMusicPlaybackKeepsBufferAndLoops(t *testing.T) {
	a, b := []byte{0xf8, 0x12, 0x34}, []byte{0xf8, 0x56, 0x78}
	p := waitMusicPlayback{track: &WaitMusic{packets: []waitMusicPacket{{a, 960}, {b, 960}}}}
	o := oggSpeech{serial: 42}
	start := time.Unix(1, 0)
	for _, step := range []struct {
		at    time.Duration
		pages int
	}{
		{0, 15},
		{100 * time.Millisecond, 5},
		{280 * time.Millisecond, 9},
		{800 * time.Millisecond, 15}, // Refill after a stall without queuing the missed audio.
		{809 * time.Millisecond, 0},
	} {
		out := p.chunk(&o, start.Add(step.at))
		const pageSize = 31
		if len(out) != step.pages*pageSize {
			t.Fatalf("at %s: %d bytes, want %d", step.at, len(out), step.pages*pageSize)
		}
		for len(out) > 0 {
			got := out[:pageSize]
			sequence := binary.LittleEndian.Uint32(got[18:])
			want := a
			if sequence%2 != 0 {
				want = b
			}
			if !bytes.Equal(got[28:], want) || got[5] != 0 ||
				binary.LittleEndian.Uint64(got[6:]) != uint64(sequence+1)*960 ||
				binary.LittleEndian.Uint32(got[22:]) != oggChecksum(got) {
				t.Fatal("wait audio broke packet order, timing or framing")
			}
			out = out[pageSize:]
		}
	}
}

type waitingRoute struct {
	delayedRoute
	route string
}

func (r waitingRoute) Decide(context.Context, string) RouteDecision {
	return RouteDecision{Route: r.route}
}

func TestWaitMusicPlaysOnlyForLLMAndSharesSpeechStream(t *testing.T) {
	for _, route := range []string{"llm", "home"} {
		t.Run(route, func(t *testing.T) {
			release := make(chan struct{})
			n, up, down := soundSession(t, waitingRoute{delayedRoute{release}, route})
			n.ProgressAfter = time.Millisecond
			n.ProgressInterval = time.Hour
			music := []byte{0xf8, 0x12, 0x34, 0x67}
			n.WaitMusic = &WaitMusic{packets: []waitMusicPacket{{music, 960}}}
			if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("wait-music")); err != nil {
				t.Fatal(err)
			}
			setup := nextSoundFrame(t, up).Client.GetEvent()
			feedSpeech(t, n, setup.GetHeader().GetMessageId(), 72, false)
			for {
				f := nextSoundFrame(t, down)
				if f.Kind() == "opaque" && !bytes.Contains(f.Raw, []byte("OpusHead")) {
					if got := bytes.Contains(f.Raw, music); got != (route == "llm") {
						t.Fatalf("route %s: waiting music=%v", route, got)
					}
					break
				}
			}
			close(release)
			final := nextSoundFrame(t, up).Client.GetEvent()
			if final.GetTtsGenerate().GetText() != "Готовый ответ" {
				t.Fatal("missing final synthesis")
			}
			speech := []byte{0xf8, 0x24, 0x91}
			id := final.GetHeader().GetMessageId()
			startSpeech(t, n, id, 88)
			if route == "llm" {
				for {
					f := nextSoundFrame(t, down)
					if f.Kind() == "opaque" && bytes.Contains(f.Raw, music) {
						break
					}
				}
			}
			part := waitMusicOgg(1, 0, true, speech, speech)
			sendSpeechPackets(t, n, 88, part)
			n.mu.Lock()
			waiting := n.requests["wait-music"].audioWaiting
			n.mu.Unlock()
			if waiting {
				t.Fatal("audio pump remained active after sending speech")
			}
			closeSpeech(t, n, id, 88)
			spoken := false
			for {
				f := nextSoundFrame(t, down)
				if f.Kind() == "opaque" {
					if binary.BigEndian.Uint32(f.Raw) != 72 || (spoken && bytes.Contains(f.Raw, music)) {
						t.Fatal("unexpected stream or music after speech")
					}
					spoken = spoken || bytes.Contains(f.Raw, speech)
				}
				if sc := f.Server.GetStreamControl(); sc.GetClose() != nil {
					if !spoken || sc.GetStreamId() != 72 {
						t.Fatal("speech did not finish on the waiting stream")
					}
					break
				}
			}
		})
	}
}
