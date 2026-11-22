package bridge

import (
	"bytes"
	"encoding/binary"
	"sync"
	"time"

	"github.com/skrashevich/quasar-bridge/internal/commandprompt"
	"github.com/skrashevich/quasar-bridge/internal/wire"
)

// aliced uses the ordinary cloud connection for START_VOCALIZE. Forward those
// packets normally, but keep the secret utterance and its audio out of all
// observers. Stream IDs are local to one WebSocket connection.
type privatePrompts struct {
	mu     sync.Mutex
	recent []privatePrompt
}

type privatePrompt struct {
	id     []byte
	stream uint32
	until  time.Time
}

func (p *privatePrompts) hidden(data []byte, d wire.Direction) bool {
	marked := bytes.Contains(data, []byte(commandprompt.Prefix))
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.recent) == 0 && !marked {
		return false
	}
	now := time.Now()
	for len(p.recent) > 0 && !now.Before(p.recent[0].until) {
		p.recent = p.recent[1:]
	}
	if marked && d == wire.ClientToServer {
		e := wire.Decode(data, d).Client.GetEvent()
		if id := e.GetHeader().GetMessageId(); e.GetTtsGenerate() != nil && id != "" && len(id) <= 256 {
			// One approval can be spoken at a time. Retain a few completed
			// IDs too, for delayed stats and errors from the native client.
			if len(p.recent) >= 8 {
				p.recent = p.recent[1:]
			}
			p.recent = append(p.recent, privatePrompt{id: []byte(id), until: now.Add(2 * time.Minute)})
		}
		return true
	}

	var speak, control uint32
	if d == wire.ServerToClient && len(data) >= 4 {
		stream := binary.BigEndian.Uint32(data)
		for _, r := range p.recent {
			if r.stream != 0 && r.stream == stream {
				return true // Opaque audio starts with the stream ID.
			}
		}
		if data[0] == 0 {
			return marked // Audio for another stream; protobuf never starts with zero.
		}
		m := wire.Decode(data, d).Server
		if e := m.GetEvent(); e.GetTtsSpeak() != nil {
			speak = uint32(e.GetHeader().GetStreamId())
		}
		control = uint32(m.GetStreamControl().GetStreamId())
	}
	hide := marked
	for i := range p.recent {
		r := &p.recent[i]
		if bytes.Contains(data, r.id) {
			hide = true
			if speak != 0 {
				r.stream = speak
			}
		} else if r.stream != 0 && r.stream == speak {
			r.stream = 0 // The server reused the stream for a different utterance.
		} else if r.stream != 0 && r.stream == control {
			hide = true // Some firmware closes audio streams without a message ID.
		}
	}
	return hide
}
