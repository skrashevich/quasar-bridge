package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	directives "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	speak "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"quasarbridge/internal/wire"
)

type speechFunc func(context.Context, string) (io.ReadCloser, error)

func (f speechFunc) Open(ctx context.Context, text string) (io.ReadCloser, error) {
	return f(ctx, text)
}

func fixedSpeech(audio []byte, said chan<- string) Speech {
	return speechFunc(func(_ context.Context, text string) (io.ReadCloser, error) {
		if said != nil {
			said <- text
		}
		return io.NopCloser(bytes.NewReader(audio)), nil
	})
}

type voiceHarness struct {
	n        *Native
	up, down chan wire.Frame
}

func newVoiceHarness(t *testing.T, resolver Resolver, speech Speech) *voiceHarness {
	t.Helper()
	h := &voiceHarness{up: make(chan wire.Frame, 64), down: make(chan wire.Frame, 256)}
	h.n = NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error {
		h.up <- wire.Decode(b, wire.ClientToServer)
		return nil
	}, func(_ context.Context, _ websocket.MessageType, b []byte) error {
		h.down <- wire.Decode(b, wire.ServerToClient)
		return nil
	}, resolver)
	h.n.Voice = speech
	t.Cleanup(h.n.Close)
	return h
}
func (h *voiceHarness) ask(t *testing.T, id string) {
	t.Helper()
	input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("вопрос")}}}}}}}
	if err := h.n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
		t.Fatal(err)
	}
}

// collect returns frames until StreamControl.Close of the speech stream.
func (h *voiceHarness) collect(t *testing.T) (kinds []string, audio []byte) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case f := <-h.down:
			kinds = append(kinds, f.Kind())
			if f.Kind() == "opaque" {
				audio = append(audio, f.Raw[4:]...)
			}
			if f.Kind() == "StreamControl" && f.Server.GetStreamControl().GetClose() != nil {
				return kinds, audio
			}
		case <-timeout:
			t.Fatalf("no end of speech, frames=%v", kinds)
		}
	}
}
func (h *voiceHarness) sawUp(kind string) bool {
	for {
		select {
		case f := <-h.up:
			if f.Kind() == kind {
				return true
			}
		default:
			return false
		}
	}
}
func TestCustomVoiceReplacesYandexSynthesis(t *testing.T) {
	audio := speechFixture(7)
	said := make(chan string, 1)
	h := newVoiceHarness(t, immediateRoute{route: "llm"}, fixedSpeech(audio, said))
	h.ask(t, "local")
	kinds, got := h.collect(t)
	if !bytes.Equal(got, audio) {
		t.Fatalf("audio changed: %d vs %d bytes", len(got), len(audio))
	}
	if !strings.Contains(strings.Join(kinds, ","), "AliceResponse") || !strings.Contains(strings.Join(kinds, ","), "TtsSpeak") {
		t.Fatalf("frames=%v", kinds)
	}
	if h.sawUp("TtsGenerate") {
		t.Fatal("Yandex synthesis was requested")
	}
	if text := <-said; text == "" {
		t.Fatal("empty speech text")
	}
}
func TestCustomVoiceFramesTargetRequest(t *testing.T) {
	h := newVoiceHarness(t, immediateRoute{route: "llm"}, fixedSpeech(speechFixture(7), nil))
	h.ask(t, "local")
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-h.down:
			switch f.Kind() {
			case "TtsSpeak":
				e := f.Server.GetEvent()
				if e.GetHeader().GetRefMessageId() != "local" || e.GetTtsSpeak().GetFormat() != "audio/opus" {
					t.Fatalf("speak ref=%q", e.GetHeader().GetRefMessageId())
				}
			case "opaque":
				if binary.BigEndian.Uint32(f.Raw)&1 != 0 || f.Raw[0] != 0 {
					t.Fatalf("stream id %x", f.Raw[:4])
				}
			case "StreamControl":
				if f.Server.GetStreamControl().GetMessageId() != "local" {
					t.Fatal("close does not target the request")
				}
				return
			}
		case <-deadline:
			t.Fatal("no close")
		}
	}
}
func TestCustomVoiceFallsBackToYandex(t *testing.T) {
	for name, speech := range map[string]Speech{
		"unreachable": speechFunc(func(context.Context, string) (io.ReadCloser, error) { return nil, errors.New("down") }),
		"not_ogg":     fixedSpeech([]byte("<html>error page, definitely not audio at all</html>"), nil),
		"too_short":   fixedSpeech(speechFixture(1)[:40], nil),
	} {
		t.Run(name, func(t *testing.T) {
			h := newVoiceHarness(t, immediateRoute{route: "llm"}, speech)
			h.ask(t, "local")
			deadline := time.After(2 * time.Second)
			for {
				select {
				case f := <-h.up:
					if f.Kind() == "TtsGenerate" {
						return
					}
				case f := <-h.down:
					if f.Kind() == "TtsSpeak" {
						t.Fatal("broken audio reached the speaker")
					}
				case <-deadline:
					t.Fatal("no Yandex fallback")
				}
			}
		})
	}
}
func TestCustomVoiceRejectsStereo(t *testing.T) {
	o := oggSpeech{serial: 1}
	head := append([]byte("OpusHead"), 1, 2, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0)
	audio := o.page(head, 2, 0)
	audio = append(audio, o.page(append([]byte("OpusTags"), make([]byte, 8)...), 0, 0)...)
	audio = append(audio, o.page([]byte{0xf8, 0xff, 0xfe}, 4, 960)...)
	if _, err := readOpusStart(bufio.NewReader(bytes.NewReader(audio))); err == nil {
		t.Fatal("stereo accepted")
	}
}
func TestCustomVoiceDuringProgress(t *testing.T) {
	release := make(chan struct{})
	h := newVoiceHarness(t, delayedRoute{release}, fixedSpeech(speechPacketFixture(9, []byte{0xfc, 1, 2, 3}), nil))
	h.n.ProgressAfter = 5 * time.Millisecond
	h.n.ProgressInterval = 20 * time.Millisecond
	h.ask(t, "slow")
	time.Sleep(50 * time.Millisecond)
	close(release)
	kinds, audio := h.collect(t)
	if h.sawUp("TtsGenerate") {
		t.Fatal("Yandex synthesis was requested")
	}
	if !bytes.Contains(audio, []byte{0xfc, 1, 2, 3}) {
		t.Fatalf("answer audio missing, frames=%v", kinds)
	}
	r := bufio.NewReader(bytes.NewReader(audio))
	heads, eos := 0, 0
	for {
		page, err := readOggPage(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("joined stream is invalid: %v", err)
		}
		if bytes.Contains(page, []byte("OpusHead")) {
			heads++
		}
		if page[5]&4 != 0 {
			eos++
		}
	}
	if heads != 1 || eos != 1 {
		t.Fatalf("joined stream heads=%d eos=%d", heads, eos)
	}
}

// Real output of tools/voice-server: ffmpeg's pages must survive segment joining.
func TestFFmpegVoiceDuringProgress(t *testing.T) {
	audio, err := os.ReadFile("testdata/ffmpeg_voice.ogg")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	h := newVoiceHarness(t, delayedRoute{release}, fixedSpeech(audio, nil))
	h.n.ProgressAfter = 5 * time.Millisecond
	h.n.ProgressInterval = 20 * time.Millisecond
	h.ask(t, "slow")
	time.Sleep(50 * time.Millisecond)
	close(release)
	_, joined := h.collect(t)
	if h.sawUp("TtsGenerate") {
		t.Fatal("custom voice failed and fell back to Yandex")
	}
	if len(joined) < len(audio)/2 {
		t.Fatalf("joined %d bytes from %d", len(joined), len(audio))
	}
}
func cloudAnswer(ref, text string, listen bool, ds ...*directives.TDirective) []byte {
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: ref}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{Response: &response.TAliceResponseProto_TResponse{Directives: ds}, VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{ShouldListen: listen, HasVoiceResponse: true, OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new(text)}}}}}}})
}

type yandexRoute struct{}

func (yandexRoute) Resolve(context.Context, string) (Resolution, error) {
	return Resolution{Route: "yandex"}, nil
}
func waitCloud(t *testing.T, h *voiceHarness) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-h.up:
			if f.Kind() == "ContinueRequest" {
				return
			}
		case <-deadline:
			t.Fatal("request did not reach the cloud")
		}
	}
}
func TestRevoiceSpeechOnlyYandexAnswer(t *testing.T) {
	said := make(chan string, 1)
	h := newVoiceHarness(t, yandexRoute{}, fixedSpeech(speechFixture(3), said))
	h.n.RevoiceYandex = true
	h.ask(t, "cloud")
	waitCloud(t, h)
	placeholder := &directives.TDirective{Type: "client_action", Name: "tts_play_placeholder"}
	if err := h.n.Server(t.Context(), websocket.MessageBinary, cloudAnswer("cloud", "Завтра sil <[200]> ясно", true, placeholder)); err != nil {
		t.Fatal(err)
	}
	// Cloud audio that was already on its way must not reach the speaker.
	old := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "cloud", StreamId: new(int64(22))}, Event: &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &speak.TTtsSpeak{Format: "audio/opus"}}}}})
	_ = h.n.Server(t.Context(), websocket.MessageBinary, old)
	_ = h.n.Server(t.Context(), websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, 22), []byte("old audio")...))
	_, audio := h.collect(t)
	if bytes.Contains(audio, []byte("old audio")) {
		t.Fatal("cloud voice leaked")
	}
	if text := <-said; text != "Завтра ясно" {
		t.Fatalf("spoke %q", text)
	}
}
func TestRevoiceKeepsResponsesWithActions(t *testing.T) {
	said := make(chan string, 1)
	h := newVoiceHarness(t, yandexRoute{}, fixedSpeech(speechFixture(3), said))
	h.n.RevoiceYandex = true
	h.ask(t, "cloud")
	waitCloud(t, h)
	timer := &directives.TDirective{Type: "client_action", Name: "timer_start"}
	if err := h.n.Server(t.Context(), websocket.MessageBinary, cloudAnswer("cloud", "Таймер запущен", false, timer)); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-h.down:
		if f.Kind() != "AliceResponse" || len(f.Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives()) != 1 {
			t.Fatalf("action response changed: %s", f.Kind())
		}
	case <-time.After(time.Second):
		t.Fatal("action response was held back")
	}
	select {
	case text := <-said:
		t.Fatalf("revoiced an action: %q", text)
	case <-time.After(50 * time.Millisecond):
	}
}
func TestRevoiceIsOffByDefault(t *testing.T) {
	said := make(chan string, 1)
	h := newVoiceHarness(t, yandexRoute{}, fixedSpeech(speechFixture(3), said))
	h.ask(t, "cloud")
	waitCloud(t, h)
	if err := h.n.Server(t.Context(), websocket.MessageBinary, cloudAnswer("cloud", "Завтра ясно", false)); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-h.down:
		if f.Kind() != "AliceResponse" {
			t.Fatalf("kind=%s", f.Kind())
		}
	case <-time.After(time.Second):
		t.Fatal("cloud answer lost")
	}
	select {
	case <-said:
		t.Fatal("revoiced without permission")
	case <-time.After(50 * time.Millisecond):
	}
}
