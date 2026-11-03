package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"

	"github.com/coder/websocket"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	stream "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/stream_control"
	speak "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"

	"quasarbridge/internal/wire"
)

// Speech supplies Ogg/Opus for an answer instead of Yandex TTS.
type Speech interface {
	Open(context.Context, string) (io.ReadCloser, error)
}

// Provider streams use a range Yandex does not hand out, and keep a zero high
// byte so a frame can never be mistaken for a protobuf message.
const customVoiceStreamBase = 0x00100000

func (n *Native) newVoiceStream() uint32 {
	return customVoiceStreamBase + 2*n.voiceStreams.Add(1)
}

// speakCustom replays the provider's message sequence through Server, so ID
// rewriting, progress segment joining and cancellation stay on one code path.
// started=false means nothing reached the speaker and Yandex TTS can still answer.
func (n *Native) speakCustom(p *nativeRequest, text string) (started bool, err error) {
	body, err := n.Voice.Open(p.ctx, text)
	if err != nil {
		return false, err
	}
	defer body.Close()
	pages := bufio.NewReaderSize(body, 1<<16)
	first, err := readOpusStart(pages)
	if err != nil {
		return false, err
	}
	id := n.newVoiceStream()
	if err := n.injectSpeak(p, id); err != nil {
		return false, err
	}
	for _, page := range first {
		if err := n.injectAudio(p, id, page); err != nil {
			return true, err
		}
	}
	var failure error
	for {
		page, err := readOggPage(pages)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			failure = err
			break
		}
		if err := n.injectAudio(p, id, page); err != nil {
			return true, err
		}
	}
	if err := n.injectClose(p, id); err != nil {
		return true, err
	}
	return true, failure
}

// Progress only needs Opus headers to open the silent stream. Generating them
// locally starts the wait immediately instead of paying for a throwaway synthesis.
func (n *Native) speakProgressHeaders(p *nativeRequest) error {
	o := oggSpeech{serial: 1}
	head := append([]byte("OpusHead"), 1, 1, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 0)
	tags := append([]byte("OpusTags"), make([]byte, 8)...)
	audio := o.page(head, 2, 0)
	audio = append(audio, o.page(tags, 0, 0)...)
	audio = append(audio, o.page([]byte{0xf8, 0xff, 0xfe}, 4, 960)...)
	id := n.newVoiceStream()
	if err := n.injectSpeak(p, id); err != nil {
		return err
	}
	if err := n.injectAudio(p, id, audio); err != nil {
		return err
	}
	return n.injectClose(p, id)
}

func (n *Native) injectSpeak(p *nativeRequest, id uint32) error {
	n.mu.Lock()
	ref := p.ttsID
	n.mu.Unlock()
	streamID := int64(id)
	m := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: ref, StreamId: &streamID}, Event: &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &speak.TTtsSpeak{Format: "audio/opus"}}}}}
	b, err := (wire.Frame{Server: m, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return n.Server(p.ctx, websocket.MessageBinary, b)
}
func (n *Native) injectAudio(p *nativeRequest, id uint32, audio []byte) error {
	return n.Server(p.ctx, websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, id), audio...))
}
func (n *Native) injectClose(p *nativeRequest, id uint32) error {
	n.mu.Lock()
	ref := p.ttsID
	n.mu.Unlock()
	m := &server.TServerMessage{Message: &server.TServerMessage_StreamControl{StreamControl: &stream.TStreamControl{MessageId: ref, StreamId: int64(id), Action: &stream.TStreamControl_Close{Close: &stream.TStreamControl_TActionClose{}}}}}
	b, err := (wire.Frame{Server: m, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return n.Server(p.ctx, websocket.MessageBinary, b)
}

// CheckSpeech proves the service returns audio the speaker can play.
func CheckSpeech(ctx context.Context, speech Speech, text string) error {
	body, err := speech.Open(ctx, text)
	if err != nil {
		return err
	}
	defer body.Close()
	_, err = readOpusStart(bufio.NewReader(body))
	return err
}

// Head, tags and first audio prove the service really returned mono Ogg/Opus.
// Mono is required: progress joins the answer to a mono stream of silence.
func readOpusStart(r *bufio.Reader) ([][]byte, error) {
	first := make([][]byte, 0, 3)
	for range 3 {
		page, err := readOggPage(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("speech service returned too little audio")
			}
			return nil, err
		}
		first = append(first, page)
	}
	head := first[0][27+int(first[0][26]):]
	if len(head) < 19 || !bytes.HasPrefix(head, []byte("OpusHead")) {
		return nil, errors.New("speech service did not return Ogg/Opus")
	}
	if head[9] != 1 {
		return nil, errors.New("speech service must return mono audio")
	}
	return first, nil
}

// readOggPage returns one verified page: the speaker receives audio page by page.
func readOggPage(r *bufio.Reader) ([]byte, error) {
	fixed := make([]byte, 27)
	if _, err := io.ReadFull(r, fixed); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, errors.New("truncated Ogg page")
	}
	if string(fixed[:4]) != "OggS" || fixed[4] != 0 {
		return nil, errors.New("invalid Ogg page")
	}
	table := make([]byte, fixed[26])
	if _, err := io.ReadFull(r, table); err != nil {
		return nil, errors.New("truncated Ogg page")
	}
	size := 0
	for _, v := range table {
		size += int(v)
	}
	page := make([]byte, 27+len(table)+size)
	copy(page, fixed)
	copy(page[27:], table)
	if _, err := io.ReadFull(r, page[27+len(table):]); err != nil {
		return nil, errors.New("truncated Ogg page")
	}
	if oggChecksum(page) != binary.LittleEndian.Uint32(page[22:26]) {
		return nil, errors.New("invalid Ogg checksum")
	}
	return page, nil
}

func (n *Native) logVoiceFailure(p *nativeRequest, started bool, err error) {
	if p.ctx.Err() == nil {
		slog.Warn("custom_voice_failed", "request", requestTag(p.id), "started", started, "error", err)
	}
}
