package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"

	"github.com/coder/websocket"
	directive "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	"quasarbridge/internal/wire"
)

const (
	maxCloudRequestBytes = 1 << 20
	maxCloudSessionBytes = 4 << 20
	maxCloudFrames       = 256
)

type cloudFrame struct {
	kind websocket.MessageType
	data []byte
}

// holdCloud runs under serverMu. Only output from an identified request or its
// TTS stream is held; recognition, acknowledgments and unrelated frames pass.
func (n *Native) holdCloud(f wire.Frame, kind websocket.MessageType, data []byte) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var p *nativeRequest
	var closeStream uint32
	closing := false
	if f.Server == nil {
		if len(data) >= 4 {
			p = n.cloudStreams[binary.BigEndian.Uint32(data)]
		}
	} else if sc := f.Server.GetStreamControl(); sc != nil {
		p = n.cloudStreams[uint32(sc.GetStreamId())]
		if sc.GetClose() != nil {
			closeStream = uint32(sc.GetStreamId())
			closing = true
		}
	} else if event := f.Server.GetEvent(); event != nil {
		var isTTS bool
		p, isTTS = n.findLocked(event.GetHeader())
		if f.Kind() == "TtsSpeak" {
			// A new speaker owns a reused stream, including local synthesis.
			delete(n.cloudStreams, uint32(event.GetHeader().GetStreamId()))
		}
		if isTTS {
			return false, nil
		}
		switch f.Kind() {
		case "AliceResponse", "DeferredAliceResponse", "TtsSpeak":
		default:
			return false, nil
		}
		if p != nil && p.outputPending && !p.canceled {
			var directives []*directive.TDirective
			if response := event.GetAliceResponse(); response != nil {
				directives = response.GetResponse().GetDirectives()
			}
			if response := event.GetDeferredAliceResponse(); response != nil {
				directives = response.GetBaseResponse().GetDirectives()
			}
			if !safeCloudOutput(directives) {
				// Output buffering cannot undo an action already run by the cloud.
				// Abort further local work; actions already run cannot be undone.
				p.canceled = true
				p.cancel()
				n.takeCloudLocked(p, true)
				n.cancel()
				return true, errors.New("early cloud response contains executor directives")
			}
			if f.Kind() == "TtsSpeak" {
				n.cloudStreams[uint32(event.GetHeader().GetStreamId())] = p
			}
		}
	}
	if p == nil || !p.outputPending {
		if closing {
			delete(n.cloudStreams, closeStream)
		}
		return false, nil
	}
	if p.canceled || p.ctx.Err() != nil {
		if event := f.Server.GetEvent(); event.GetTtsSpeak() != nil {
			n.blocked[uint32(event.GetHeader().GetStreamId())] = p
		}
		return true, nil
	}
	if len(p.cloudFrames) >= maxCloudFrames || len(data) > maxCloudRequestBytes-p.cloudBytes || len(data) > maxCloudSessionBytes-n.cloudBytes {
		p.canceled = true
		p.cancel()
		n.takeCloudLocked(p, true)
		n.cancel()
		return true, errors.New("pending cloud response exceeds buffer limit")
	}
	p.cloudFrames = append(p.cloudFrames, cloudFrame{kind: kind, data: bytes.Clone(data)})
	p.cloudBytes += len(data)
	n.cloudBytes += len(data)
	if closing {
		delete(n.cloudStreams, closeStream)
	}
	return true, nil
}

func (n *Native) hasCloudStreamLocked(p *nativeRequest) bool {
	for _, owner := range n.cloudStreams {
		if owner == p {
			return true
		}
	}
	for _, owner := range n.blocked {
		if owner == p {
			return true
		}
	}
	return false
}

// replayCloud runs under serverMu so fresh incoming output cannot overtake it.
func (n *Native) replayCloud(ctx context.Context, p *nativeRequest, frames []cloudFrame) error {
	for _, frame := range frames {
		decoded := wire.Decode(frame.data, wire.ServerToClient)
		if sc := decoded.Server.GetStreamControl(); sc.GetClose() != nil {
			n.mu.Lock()
			if n.cloudStreams[uint32(sc.GetStreamId())] == p {
				delete(n.cloudStreams, uint32(sc.GetStreamId()))
			}
			n.mu.Unlock()
		}
		if err := n.server(ctx, frame.kind, frame.data); err != nil {
			return err
		}
	}
	return nil
}

// takeCloudLocked releases all memory charged to this request. Discarded live
// TTS streams remain blocked until their close or a new TtsSpeak reuses the ID.
func (n *Native) takeCloudLocked(p *nativeRequest, block bool) []cloudFrame {
	frames := p.cloudFrames
	p.cloudFrames = nil
	n.cloudBytes -= p.cloudBytes
	p.cloudBytes = 0
	for stream, owner := range n.cloudStreams {
		if owner == p {
			delete(n.cloudStreams, stream)
			if block {
				n.blocked[stream] = p
			}
		}
	}
	return frames
}

func safeCloudOutput(directives []*directive.TDirective) bool {
	for _, d := range directives {
		if d.GetType() != "client_action" {
			return false
		}
		switch d.GetName() {
		case "tts_play_placeholder", "draw_animation_directive":
		default:
			return false
		}
	}
	return true
}
