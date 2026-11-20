package bridge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"slices"

	opus "github.com/skrashevich/go-opus"
)

// Experimental acoustic heuristic calibrated on this speaker. No audio is
// written to disk or sent to an additional service. Unknown input stays normal.
func (n *Native) collectWhisperAudio(data []byte) {
	if os.Getenv("QUASAR_AUTO_WHISPER") != "1" || len(data) < 4 {
		return
	}
	stream := int64(binary.BigEndian.Uint32(data[:4]))
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, p := range n.requests {
		if p.canceled || p.ctx.Err() != nil {
			p.audioInput = nil
			p.audioInputDone = true
			continue
		}
		if p.header == nil || p.header.GetStreamId() != stream || p.audioInputDone {
			continue
		}
		if len(p.audioInput)+len(data)-4 > 192*1024 {
			p.audioInput = nil
			p.audioInputDone = true
			return
		}
		p.audioInput = append(p.audioInput, data[4:]...)
		return
	}
}

func decodeWhisperOpus(b []byte) (pcm []float32, err error) {
	// Bound both compressed input and decoded duration, reject malformed streams.
	if len(b) > 192*1024 {
		return nil, errors.New("audio limit")
	}
	d, err := opus.NewDecoderWithOutput(16000, 1)
	if err != nil {
		return nil, err
	}
	var packet []byte
	headers := 0
	scratch := make([]float32, 1920)
	for len(b) >= 27 {
		if string(b[:4]) != "OggS" || b[4] != 0 {
			return nil, errors.New("invalid ogg")
		}
		count := int(b[26])
		if len(b) < 27+count {
			return nil, errors.New("truncated ogg lacing")
		}
		size := 27 + count
		for _, v := range b[27 : 27+count] {
			size += int(v)
		}
		if len(b) < size {
			return nil, errors.New("truncated ogg page")
		}
		page := b[:size]
		b = b[size:]
		if oggChecksum(page) != binary.LittleEndian.Uint32(page[22:26]) {
			return nil, errors.New("ogg checksum")
		}
		pos := 27 + count
		for _, v := range page[27 : 27+count] {
			packet = append(packet, page[pos:pos+int(v)]...)
			pos += int(v)
			if len(packet) > 65536 {
				return nil, errors.New("packet limit")
			}
			if v == 255 {
				continue
			}
			if bytes.HasPrefix(packet, []byte("OpusHead")) {
				if len(packet) < 19 || packet[9] != 1 || packet[18] != 0 {
					return nil, errors.New("unsupported audio")
				}
				// Station pre-roll may begin a new logical Opus stream. Its
				// headers are metadata, not compressed sound packets.
				d, err = opus.NewDecoderWithOutput(16000, 1)
				if err != nil {
					return nil, err
				}
				headers = 0
			} else if bytes.HasPrefix(packet, []byte("OpusTags")) {
				if headers == 0 {
					return nil, errors.New("tags before head")
				}
				if headers > 1 {
					packet = nil
					continue
				}
			} else {
				if headers < 2 {
					return nil, errors.New("missing audio headers")
				}
				count, err := d.DecodeToFloat32(packet, scratch)
				if err != nil {
					return nil, err
				}
				if count < 0 || count > len(scratch) {
					return nil, errors.New("sample limit")
				}
				if len(pcm)+count > 16000*10 {
					return pcm, nil
				}
				pcm = append(pcm, scratch[:count]...)
			}
			headers++
			packet = nil
		}
	}
	if len(b) != 0 || len(packet) != 0 {
		return nil, errors.New("truncated ogg stream")
	}
	if headers < 3 {
		return nil, errors.New("no audio")
	}
	return pcm, nil
}

type whisperMetrics struct {
	Active                   int
	ZCR, Periodicity, Voiced float64
	Whisper                  bool
}

func acousticWhisper(pcm []float32) whisperMetrics {
	var m whisperMetrics
	if len(pcm) < 16000/2 {
		return m
	}
	var energies []float64
	peak := 0.0
	for i := 0; i+400 <= len(pcm); i += 160 {
		e := 0.0
		for _, v := range pcm[i : i+400] {
			e += float64(v) * float64(v)
		}
		e = math.Sqrt(e / 400)
		energies = append(energies, e)
		if e > peak {
			peak = e
		}
	}
	if peak < 1e-5 {
		return m
	}
	var zcrs, corrs []float64
	voiced := 0
	for fi, rms := range energies {
		if rms < peak*.12 {
			continue
		}
		f := pcm[fi*160 : fi*160+400]
		mean := 0.0
		z := 0
		for i, v := range f {
			mean += float64(v)
			if i > 0 && (v >= 0) != (f[i-1] >= 0) {
				z++
			}
		}
		mean /= 400
		var a [400]float64
		den := 0.0
		for i, v := range f {
			a[i] = float64(v) - mean
			den += a[i] * a[i]
		}
		best := 0.0
		for lag := 40; lag <= 200; lag++ {
			c := 0.0
			for i := lag; i < 400; i++ {
				c += a[i] * a[i-lag]
			}
			if den > 0 && c/den > best {
				best = c / den
			}
		}
		zcrs = append(zcrs, float64(z)/400)
		corrs = append(corrs, best)
		if best > .45 {
			voiced++
		}
	}
	m.Active = len(zcrs)
	if m.Active < 35 {
		return m
	}
	slices.Sort(zcrs)
	slices.Sort(corrs)
	m.ZCR = zcrs[len(zcrs)/2]
	m.Periodicity = corrs[len(corrs)/2]
	m.Voiced = float64(voiced) / float64(m.Active)
	m.Whisper = m.ZCR > .22 && m.Periodicity < .30 && m.Voiced < .18
	return m
}
