package bridge

import (
	"errors"
	"io"
	"os"
	"time"
)

const (
	maxWaitMusicBytes   = 2 << 20
	maxWaitMusicPackets = 30000
	waitMusicBuffer     = 300 * time.Millisecond
)

// WaitMusic holds the Opus packets of a locally configured track. Sessions share
// the track, while each request has its own playback position.
type WaitMusic struct {
	packets []waitMusicPacket
}

type waitMusicPacket struct {
	data    []byte
	samples uint64
}

// LoadWaitMusic reads a bounded mono Ogg Opus file.
func LoadWaitMusic(path string) (*WaitMusic, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("wait music must be a regular file")
	}
	if info.Size() > maxWaitMusicBytes {
		return nil, errors.New("wait music exceeds 2 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxWaitMusicBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxWaitMusicBytes {
		return nil, errors.New("wait music exceeds 2 MiB")
	}

	var track WaitMusic
	o := oggSpeech{onPacket: func(data []byte, samples uint64) error {
		if len(track.packets) >= maxWaitMusicPackets {
			return errors.New("wait music exceeds 30000 Opus packets")
		}
		track.packets = append(track.packets, waitMusicPacket{data, samples})
		return nil
	}}
	o.next()
	o.source.discardAudio = true
	if _, err := o.feed(data); err != nil {
		return nil, err
	}
	if err := o.endSource(); err != nil {
		return nil, err
	}
	if !o.source.ended {
		return nil, errors.New("wait music is missing Ogg end of stream")
	}
	if !plainOpusHeader(o.head) || o.head[9] != 1 {
		return nil, errors.New("wait music requires mono Ogg Opus")
	}
	return &track, nil
}

func plainOpusHeader(head []byte) bool {
	return len(head) == 19 && head[8] == 1 && (head[9] == 1 || head[9] == 2) && head[18] == 0
}

type waitMusicPlayback struct {
	track         *WaitMusic
	nextPacket    int
	bufferedUntil time.Time
}

// chunk is called under Native.mu. Top up the buffer from elapsed time so a late
// ticker wakeup does not starve playback or accumulate audio after a stall.
func (p *waitMusicPlayback) chunk(o *oggSpeech, now time.Time) []byte {
	if p.track == nil || len(p.track.packets) == 0 {
		return o.silence()
	}
	if p.bufferedUntil.Before(now) {
		p.bufferedUntil = now
	}
	deadline := now.Add(waitMusicBuffer)
	var out []byte
	for {
		packet := p.track.packets[p.nextPacket]
		end := p.bufferedUntil.Add(time.Duration(packet.samples) * time.Second / 48000)
		if end.After(deadline) {
			return out
		}
		p.bufferedUntil = end
		p.nextPacket = (p.nextPacket + 1) % len(p.track.packets)
		o.granule += packet.samples
		out = append(out, o.page(packet.data, 0, o.granule)...)
	}
}
