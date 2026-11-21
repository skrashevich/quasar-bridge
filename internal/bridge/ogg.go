package bridge

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// oggSpeech joins independently synthesized Opus segments into one Ogg stream.
// Container timing follows RFC 7845; Opus frame durations follow RFC 6716.
// A Station ends playback on Ogg EOS as well as StreamControl.Close, so both
// must be delayed until the final segment. Never use this for pass-through audio.
type oggSpeech struct {
	pending          []byte
	pendingSamples   uint64
	serial, sequence uint32
	granule          uint64
	head             []byte
	source           *oggSource
	trim             uint64
}
type oggSource struct {
	discardAudio   bool
	ended          bool
	buffer, packet []byte
	headers        int
	samples        uint64
}

func (o *oggSpeech) next() { o.source = new(oggSource); o.trim = 0 }
func (o *oggSpeech) feed(b []byte) ([]byte, error) {
	if o.source == nil {
		o.next()
	}
	s := o.source
	if len(s.buffer)+len(b) > 2<<20 {
		return nil, errors.New("oversized Ogg input")
	}
	s.buffer = append(s.buffer, b...)
	var out []byte
	for len(s.buffer) >= 27 {
		p := s.buffer
		if string(p[:4]) != "OggS" || p[4] != 0 {
			return nil, errors.New("invalid Ogg page")
		}
		count := int(p[26])
		if len(p) < 27+count {
			break
		}
		size := 27 + count
		for _, n := range p[27 : 27+count] {
			size += int(n)
		}
		if len(p) < size {
			break
		}
		page := p[:size]
		if oggChecksum(page) != binary.LittleEndian.Uint32(page[22:26]) {
			return nil, errors.New("invalid Ogg checksum")
		}
		// UniProxy can return a chain of logical Opus streams for one
		// TtsGenerate, for example a sound followed by spoken text.
		if page[5]&2 != 0 && s.headers > 0 {
			if !s.ended || len(s.packet) != 0 {
				return nil, errors.New("new Opus segment before previous EOS")
			}
			s.headers = 0
			s.samples = 0
			o.trim = 0
		}
		if len(o.head) == 0 {
			o.serial = binary.LittleEndian.Uint32(page[14:18])
		}
		pos := 27 + count
		for _, n := range page[27 : 27+count] {
			s.packet = append(s.packet, page[pos:pos+int(n)]...)
			pos += int(n)
			if len(s.packet) > 1<<20 {
				return nil, errors.New("oversized Ogg packet")
			}
			if n == 255 {
				continue
			}
			packet := s.packet
			s.packet = nil
			if len(packet) > 65024 {
				return nil, errors.New("oversized Opus packet")
			}
			switch s.headers {
			case 0:
				if len(packet) < 19 || !bytes.HasPrefix(packet, []byte("OpusHead")) {
					return nil, errors.New("missing OpusHead")
				}
				if len(o.head) == 0 {
					o.head = bytes.Clone(packet)
					out = append(out, o.page(packet, 2, 0)...)
				} else if packet[9] != o.head[9] || !bytes.Equal(packet[18:], o.head[18:]) {
					return nil, errors.New("incompatible Opus segments")
				}
			case 1:
				if !bytes.HasPrefix(packet, []byte("OpusTags")) {
					return nil, errors.New("missing OpusTags")
				}
				if o.sequence == 1 {
					out = append(out, o.page(packet, 0, 0)...)
				}
			default:
				samples, err := opusSamples(packet)
				if err != nil {
					return nil, err
				}
				if !s.discardAudio {
					s.samples += samples
					out = append(out, o.flushPending(0, 0)...)
					o.pending = bytes.Clone(packet)
					o.pendingSamples = samples
				}
			}
			s.headers++
		}
		s.ended = page[5]&4 != 0
		if s.ended && !s.discardAudio {
			end := binary.LittleEndian.Uint64(page[6:14])
			if end <= s.samples {
				o.trim = s.samples - end
			}
		}
		s.buffer = s.buffer[size:]
	}
	return out, nil
}
func (o *oggSpeech) endSource() error {
	if o.source == nil || len(o.source.buffer) != 0 || len(o.source.packet) != 0 || o.source.headers < 3 {
		return errors.New("incomplete Opus segment")
	}
	if o.trim > o.pendingSamples {
		return errors.New("unsupported Opus end trim")
	}
	return nil
}
func (o *oggSpeech) silence() []byte {
	// RFC 6716: one 20ms CELT frame, with the standard Opus silence payload.
	out := o.flushPending(0, 0)
	for range 5 {
		o.granule += 960
		out = append(out, o.page([]byte{0xf8, 0xff, 0xfe}, 0, o.granule)...)
	}
	return out
}
func (o *oggSpeech) finish() []byte { return o.flushPending(4, o.trim) }
func (o *oggSpeech) finishSilence() []byte {
	out := o.flushPending(0, 0)
	o.granule += 960
	return append(out, o.page([]byte{0xf8, 0xff, 0xfe}, 4, o.granule)...)
}
func (o *oggSpeech) flushPending(flags byte, trim uint64) []byte {
	if o.pending == nil {
		return nil
	}
	o.granule += o.pendingSamples
	out := o.page(o.pending, flags, o.granule-trim)
	o.pending = nil
	o.pendingSamples = 0
	return out
}
func (o *oggSpeech) page(packet []byte, flags byte, granule uint64) []byte {
	count := len(packet)/255 + 1
	if packet == nil {
		count = 0
	}
	b := make([]byte, 27+count+len(packet))
	copy(b, "OggS")
	b[5] = flags
	binary.LittleEndian.PutUint64(b[6:], granule)
	binary.LittleEndian.PutUint32(b[14:], o.serial)
	binary.LittleEndian.PutUint32(b[18:], o.sequence)
	o.sequence++
	b[26] = byte(count)
	for i := range count - 1 {
		b[27+i] = 255
	}
	if count > 0 {
		b[26+count] = byte(len(packet) % 255)
	}
	copy(b[27+count:], packet)
	binary.LittleEndian.PutUint32(b[22:], oggChecksum(b))
	return b
}
func oggChecksum(b []byte) uint32 {
	var crc uint32
	for i, v := range b {
		if i >= 22 && i < 26 {
			v = 0
		}
		crc ^= uint32(v) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
func opusSamples(p []byte) (uint64, error) {
	if len(p) == 0 {
		return 0, errors.New("empty Opus packet")
	}
	config := p[0] >> 3
	var frame uint64
	switch {
	case config >= 16:
		frame = 120 << uint(config&3)
	case config >= 12:
		frame = 480 << uint(config&1)
	default:
		frame = 480 << uint(config&3)
		if frame == 3840 {
			frame = 2880
		}
	}
	count := uint64(1)
	switch p[0] & 3 {
	case 1, 2:
		count = 2
	case 3:
		if len(p) < 2 {
			return 0, errors.New("short Opus frame count")
		}
		count = uint64(p[1] & 63)
	}
	if count == 0 || count*frame > 5760 {
		return 0, errors.New("invalid Opus duration")
	}
	return count * frame, nil
}
