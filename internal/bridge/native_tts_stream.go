package bridge

import (
	"context"
	"log/slog"
	"strings"

	"github.com/coder/websocket"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"google.golang.org/protobuf/encoding/protowire"
	"quasarbridge/internal/wire"
)

// Both official event field IDs and payload numbers were verified on the
// actual Yandex UniProxy server, using an isolated live protocol session.
// The currently pinned Go protobuf package does not include this event.
func nativeDeferredTTSFrame(requestID, text string, chunk uint64, last bool) ([]byte, error) {
	payload := protowire.AppendTag(nil, 1, protowire.BytesType)
	payload = protowire.AppendString(payload, requestID)
	payload = protowire.AppendTag(payload, 2, protowire.BytesType)
	payload = protowire.AppendString(payload, text)
	payload = protowire.AppendTag(payload, 3, protowire.VarintType)
	if last {
		payload = protowire.AppendVarint(payload, 1)
	} else {
		payload = protowire.AppendVarint(payload, 0)
	}
	payload = protowire.AppendTag(payload, 4, protowire.VarintType)
	payload = protowire.AppendVarint(payload, chunk)
	event := &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: newID()}}
	wireField := protowire.AppendTag(nil, 34, protowire.BytesType)
	wireField = protowire.AppendBytes(wireField, payload)
	event.ProtoReflect().SetUnknown(wireField)
	frame := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: event}}
	return (wire.Frame{Client: frame, Prefix: []byte("AAPI")}).Marshal()
}

func (n *Native) sendNativeTTSChunk(p *nativeRequest, text string, last bool) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	n.mu.Lock()
	if p.canceled || !p.nativeTextStream {
		n.mu.Unlock()
		return context.Canceled
	}
	p.nativeTextChunk++
	index := p.nativeTextChunk
	id := p.ttsID
	n.mu.Unlock()
	// IMPORTANT: Preserve all characters, including spaces. Stream deltas
	// can start/end inside a word, and trimming would corrupt speech.
	b, err := nativeDeferredTTSFrame(id, text, index, last)
	if err != nil {
		return err
	}
	slog.Info("native_tts_incremental_text", "request", requestTag(p.id),
		"chunk", index, "last", last, "text_bytes", len(text))
	return n.up(p.ctx, websocket.MessageBinary, b)
}

// beginNativeStreamedReply opens one Yandex native streaming session.
func (n *Native) beginNativeStreamedReply(p *nativeRequest, _ string) error {
	n.mu.Lock()
	if p.canceled || p.ctx.Err() != nil {
		n.mu.Unlock()
		return context.Canceled
	}
	p.local = true
	p.streaming = true
	p.nativeTextStream = true
	p.responseID = newID()
	p.ttsID = newID()
	p.listenOnStreamStart = true
	n.tts[p.ttsID] = p
	n.mu.Unlock()
	if p.gated {
		if err := n.sendCancel(p.ctx, p.id); err != nil {
			return err
		}
	} else if err := n.ack(p); err != nil {
		return err
	}
	if err := n.beginDeferred(p); err != nil {
		return err
	}
	slog.Info("native_tts_one_session_started", "request", requestTag(p.id))
	return nil
}

func (n *Native) appendNativeStreamedReply(p *nativeRequest, text string) error {
	if err := n.deferredPart(p, text, false); err != nil {
		return err
	}
	return n.sendNativeTTSChunk(p, text, false)
}

// The active native mode uses the firmware's existing TtsGenerate
// (IsStreaming=true) plus DeferredTtsGenerate on ONE request. Unlike the
// former 16-rune transport batching, each non-final deferred part now contains
// a complete sentence, except for the first long sentence which may start
// immediately at a natural clause pause. This retains the stock synthesizer
// and player; it does not create additional TtsGenerate requests or streams.
//
// DeferredTtsGenerate boundaries are observed as independent logical Ogg/Opus
// sequences from Yandex. They do not guarantee identical neural prosody to
// original Alice, but avoid asking the synthesizer to restart every 16 runes.
func (n *Native) streamSpeechNative(p *nativeRequest, prompt string, provider StreamingFallbackResolver) (Resolution, error) {
	var buffer streamedSentenceBuffer
	var received string
	started := false
	chunks := 0
	ensureStarted := func() error {
		if started {
			return nil
		}
		if err := n.beginNativeStreamedReply(p, ""); err != nil {
			return err
		}
		started = true
		return nil
	}
	submit := func(text string) error {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		if err := ensureStarted(); err != nil {
			return err
		}
		if err := n.appendNativeStreamedReply(p, text); err != nil {
			return err
		}
		chunks++
		return nil
	}
	accept := func(delta string) error {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if delta == "" {
			return nil
		}
		received += delta
		return buffer.feedNative(delta, submit)
	}
	result, err := provider.FallbackStream(p.ctx, prompt, accept)
	if err != nil {
		return result, err
	}
	// Providers may return the entire answer only at completion or add a
	// final suffix after the last streaming callback. Never replay a prefix.
	if received == "" {
		if err := accept(result.Answer); err != nil {
			return result, err
		}
	} else if strings.HasPrefix(result.Answer, received) {
		if err := accept(result.Answer[len(received):]); err != nil {
			return result, err
		}
	} else if result.Answer != "" && result.Answer != received {
		slog.Warn("native_stream_provider_text_mismatch", "received_bytes", len(received), "final_bytes", len(result.Answer))
	}
	// The model may finish without punctuation. Only the residual text
	// becomes the final spoken part; no arbitrary mid-word timeout splits.
	if err := submit(buffer.pending); err != nil {
		return result, err
	}
	if !started {
		if err := ensureStarted(); err != nil {
			return result, err
		}
	}
	slog.Info("native_stream_complete", "request", requestTag(p.id),
		"native_sentences", chunks, "text_bytes", len(received))
	// The native completion path sends only one IsLast=true empty message.
	// Everything vocalized has already been transmitted, once, in order.
	result.Answer = ""
	return result, nil
}

// The final streaming answer must NOT wait for the TTS stream to close:
// IsLast is precisely what tells Yandex to finish producing that stream.
func (n *Native) finishNativeStreamedReply(p *nativeRequest, tail string) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if err := n.deferredPart(p, tail, true); err != nil {
		return err
	}
	return n.sendNativeTTSChunk(p, tail, true)
}
