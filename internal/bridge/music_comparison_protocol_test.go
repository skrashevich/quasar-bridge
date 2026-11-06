package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	directives "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	response "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	asr "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	push "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/push"
	stream "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/stream_control"
	tts "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	deferred "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/streaming"
	"google.golang.org/protobuf/proto"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/musicmatch"
	"quasarbridge/internal/wire"
)

func comparisonRig(t *testing.T, p *comparisonProvider) (*Native, chan []byte, chan []byte) {
	t.Helper()
	up, down := make(chan []byte, 256), make(chan []byte, 256)
	n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- bytes.Clone(b); return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- bytes.Clone(b); return nil }, &Engine{SoundCloud: p})
	t.Cleanup(n.Close)
	return n, up, down
}

func TestNativeMusicComparisonCompletedSoundCloudToYandex(t *testing.T) {
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}}
	n, up, down := comparisonRig(t, p)
	comparisonStart(t, n, up, "SC")
	n.Server(t.Context(), websocket.MessageBinary, comparisonCloud("SC", "Other", "Artist"))
	comparisonWinner(t, down)
	n.wg.Wait()
	n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("YM", "включи песню New на Яндекс Музыке"))
	for {
		if wire.Decode(comparisonRead(t, up), wire.ClientToServer).Kind() == "TextInput" {
			break
		}
	}
	original := comparisonCloud("YM", "New", "Artist")
	if err := n.Server(t.Context(), websocket.MessageBinary, original); err != nil {
		t.Fatal(err)
	}
	if b := comparisonWinner(t, down); !bytes.Equal(b, original) {
		t.Fatal("explicit YM handover blocked")
	}
	f := wire.Decode(comparisonCloud("callback", "Next", "Artist"), wire.ServerToClient)
	callback := comparisonPush("", f.Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives())
	if err := n.Server(t.Context(), websocket.MessageBinary, callback); err != nil {
		t.Fatal(err)
	}
	if b := comparisonRead(t, down); !bytes.Equal(b, callback) {
		t.Fatal("textless/preload next blocked")
	}
}

func TestNativeMusicComparisonPriorSoundCloudUnknownLayoutFallback(t *testing.T) {
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}}
	n, up, down := comparisonRig(t, p)
	comparisonStart(t, n, up, "firstSC")
	n.Server(t.Context(), websocket.MessageBinary, comparisonCloud("firstSC", "Other", "Artist"))
	comparisonWinner(t, down)
	n.wg.Wait()
	p.release = make(chan struct{})
	n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("newer", "включи песню Гусары орать"))
	for {
		if wire.Decode(comparisonRead(t, up), wire.ClientToServer).Kind() == "TextInput" {
			break
		}
	}
	f := wire.Decode(comparisonCloud("newer", "New YM", "Artist"), wire.ServerToClient)
	unknown := comparisonPush("", f.Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives())
	if err := n.Server(t.Context(), websocket.MessageBinary, unknown); err != nil {
		t.Fatal(err)
	}
	if b := comparisonRead(t, down); !bytes.Equal(b, unknown) {
		t.Fatal("prior SC quarantine consumed new unknown-layout fallback")
	}
	if err := n.Server(t.Context(), websocket.MessageBinary, unknown); err != nil {
		t.Fatal(err)
	}
	if b := comparisonRead(t, down); !bytes.Equal(b, unknown) {
		t.Fatal("fallback did not release prior quarantine")
	}
	if p.committed.Load() != 1 {
		t.Fatal("fallback committed new SC")
	}
}

func TestNativeMusicComparisonFailedSearchAndRefusalKeepPriorSource(t *testing.T) {
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}}
	n, up, down := comparisonRig(t, p)
	comparisonStart(t, n, up, "playingSC")
	n.Server(t.Context(), websocket.MessageBinary, comparisonCloud("playingSC", "Other", "Artist"))
	comparisonWinner(t, down)
	n.wg.Wait()
	p.fail = errors.New("SC unavailable")
	n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("missing", "включи песню Гусары орать"))
	for {
		if wire.Decode(comparisonRead(t, up), wire.ClientToServer).Kind() == "TextInput" {
			break
		}
	}
	refusal := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "missing"}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new("Не нашла такую песню")}}}}}}})
	n.Server(t.Context(), websocket.MessageBinary, refusal)
	if b := comparisonRead(t, down); !bytes.Equal(b, refusal) {
		t.Fatal("refusal bytes lost")
	}
	n.wg.Wait()
	if p.accepted.Load() != 0 || p.committed.Load() != 1 {
		t.Fatal("failed search/refusal invalidated prior source")
	}
}

func TestNativeMusicComparisonYandexDeadlineUsesReadySoundCloud(t *testing.T) {
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}}
	n, up, down := comparisonRig(t, p)
	n.ComparisonTimeout = 50 * time.Millisecond
	comparisonStart(t, n, up, "slowYM")
	played := comparisonWinner(t, down)
	if bytes.Equal(played, comparisonCloud("slowYM", "Late YM", "Artist")) || p.committed.Load() != 1 {
		t.Fatal("ready SC not selected on cloud timeout")
	}
	n.wg.Wait()
	if err := n.Server(t.Context(), websocket.MessageBinary, comparisonCloud("slowYM", "Late YM", "Artist")); err != nil {
		t.Fatal(err)
	}
	if err := n.Server(t.Context(), websocket.MessageBinary, comparisonSpeech("slowYM", 111)); err != nil {
		t.Fatal(err)
	}
	if err := n.Server(t.Context(), websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, 111), []byte("late YM speech")...)); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-down:
		t.Fatalf("late timed-out cloud leaked: %s", wire.Decode(b, wire.ServerToClient).Kind())
	case <-time.After(20 * time.Millisecond):
	}
}

func TestProxyMusicComparisonASRApprovalAndOneExecutor(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	requests := make(chan string, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			f := wire.Decode(data, wire.ClientToServer)
			requests <- f.Kind()
			switch f.Kind() {
			case "VoiceInput":
				if !f.Client.GetEvent().GetVoiceInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
					t.Error("voice request bypassed approval")
				}
				result := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "voice"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("включи песню Гусары орать")}}}}}}})
				if err := ws.Write(ctx, websocket.MessageBinary, result); err != nil {
					return
				}
			case "ContinueRequest":
				if err := ws.Write(ctx, websocket.MessageBinary, comparisonCloud("voice", "Other", "Artist")); err != nil {
					return
				}
			}
		}
	}))
	defer upstream.Close()
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}}
	e := &Engine{SoundCloud: p}
	proxy := &Proxy{Upstream: "ws" + strings.TrimPrefix(upstream.URL, "http"), NewSession: func(ctx context.Context, up, down Sender) Session { return NewNative(ctx, up, down, e) }}
	api := httptest.NewServer(proxy)
	defer api.Close()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(api.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	input := wire.Decode(soundCloudTextInput("voice", ""), wire.ClientToServer)
	event := input.Client.GetEvent()
	event.Event = &client.TClientMessage_TEvent_VoiceInput{VoiceInput: event.GetTextInput()}
	if err := ws.Write(ctx, websocket.MessageBinary, aa(input.Client)); err != nil {
		t.Fatal(err)
	}
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		f := wire.Decode(data, wire.ServerToClient)
		ds, _, _ := comparisonDirectives(f.Server.GetEvent())
		if len(ds) > 0 {
			if ds[0].GetPayload().GetFields()["stream"].GetStructValue().GetFields()["url"].GetStringValue() != "https://audio.example/sc.m3u8" {
				t.Fatal("wrong winner")
			}
			break
		}
	}
	counts := map[string]int{}
	for {
		select {
		case name := <-requests:
			counts[name]++
			if name == "CancelRequest" {
				if counts["VoiceInput"] != 1 || counts["ContinueRequest"] != 1 || p.committed.Load() != 1 {
					t.Fatal("multiple executor/delegation")
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("missing losing cloud cancel")
		}
	}
}

func TestNativeMusicComparisonMicrophoneCloseAfterFinalASR(t *testing.T) {
	for _, action := range []string{"close", "spotter_end", "speech_close", "speech_spotter_end", "foreign_close", "output_close"} {
		t.Run(action, func(t *testing.T) {
			p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}}
			n, up, down := comparisonRig(t, p)
			input := wire.Decode(soundCloudTextInput("microphone", ""), wire.ClientToServer)
			e := input.Client.GetEvent()
			e.Header.StreamId = new(int64(1))
			e.Event = &client.TClientMessage_TEvent_VoiceInput{VoiceInput: e.GetTextInput()}
			kind := "VoiceInput"
			if strings.HasPrefix(action, "speech_") {
				e.Event = &client.TClientMessage_TEvent_SpeechInput{SpeechInput: e.GetVoiceInput()}
				kind = "SpeechInput"
			}
			if err := n.Client(t.Context(), websocket.MessageBinary, aa(input.Client)); err != nil {
				t.Fatal(err)
			}
			f := wire.Decode(comparisonRead(t, up), wire.ClientToServer)
			req := f.Client.GetEvent().GetVoiceInput()
			if kind == "SpeechInput" {
				req = f.Client.GetEvent().GetSpeechInput()
			}
			if f.Kind() != kind || !req.GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
				t.Fatal("microphone approval lost")
			}
			asrResult := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "microphone"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("включи песню Гусары орать")}}}}}}})
			if err := n.Server(t.Context(), websocket.MessageBinary, asrResult); err != nil {
				t.Fatal(err)
			}
			comparisonRead(t, down)
			if f := wire.Decode(comparisonRead(t, up), wire.ClientToServer); f.Kind() != "ContinueRequest" {
				t.Fatal("comparison was not started after ASR")
			}
			control := &stream.TStreamControl{StreamId: 1, MessageId: "upload-control-not-request-id"}
			if strings.HasSuffix(action, "close") {
				control.Action = &stream.TStreamControl_Close{Close: &stream.TStreamControl_TActionClose{Reason: stream.TStreamControl_TActionClose_STOP_RECORDING}}
			} else {
				control.Action = &stream.TStreamControl_SpotterEnd{SpotterEnd: &stream.TStreamControl_TActionSpotterEnd{}}
			}
			if action == "foreign_close" {
				control.StreamId = 2
			}
			if action == "output_close" {
				if err := n.Server(t.Context(), websocket.MessageBinary, comparisonSpeech("microphone", 1)); err != nil {
					t.Fatal(err)
				}
			}
			closeBytes := aa(&server.TServerMessage{Message: &server.TServerMessage_StreamControl{StreamControl: control}})
			if err := n.Server(t.Context(), websocket.MessageBinary, closeBytes); err != nil {
				t.Fatal(err)
			}
			if action == "output_close" {
				select {
				case <-down:
					t.Fatal("known output stream escaped comparison holding")
				default:
				}
			} else if got := comparisonRead(t, down); !bytes.Equal(got, closeBytes) {
				t.Fatal("upload close control was changed or held")
			}
			cloud := comparisonCloud("microphone", "Другая песня", "Artist")
			if err := n.Server(t.Context(), websocket.MessageBinary, cloud); err != nil {
				t.Fatal(err)
			}
			winner := comparisonWinner(t, down)
			if action == "foreign_close" {
				if p.committed.Load() != 0 || !bytes.Equal(winner, cloud) {
					t.Fatal("foreign control bypassed conservative Yandex fallback")
				}
				return
			}
			ds, _, _ := comparisonDirectives(wire.Decode(winner, wire.ServerToClient).Server.GetEvent())
			if p.committed.Load() != 1 || len(ds) != 1 || ds[0].GetPayload().GetFields()["stream"].GetStructValue().GetFields()["url"].GetStringValue() != "https://audio.example/sc.m3u8" {
				t.Fatal("upload termination forced wrong music provider")
			}
			if action == "output_close" {
				if err := n.Server(t.Context(), websocket.MessageBinary, closeBytes); err != nil {
					t.Fatal(err)
				}
				select {
				case <-down:
					t.Fatal("losing output control bypassed tombstone via input stream collision")
				default:
				}
			}
		})
	}
}

func TestNativeMusicComparisonBlockedAckYandexHandover(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}}
	e := &Engine{SoundCloud: p}
	upA, upB, down := make(chan []byte, 64), make(chan []byte, 64), make(chan []byte, 64)
	downSend := func(_ context.Context, _ websocket.MessageType, b []byte) error {
		f := wire.Decode(b, wire.ServerToClient)
		if f.Kind() == "InputStartAck" && f.Server.GetEvent().GetHeader().GetRefMessageId() == "A" {
			once.Do(func() { close(entered) })
			<-release
			return nil
		}
		down <- bytes.Clone(b)
		return nil
	}
	a := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error { upA <- bytes.Clone(b); return nil }, downSend, e)
	b := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, v []byte) error { upB <- bytes.Clone(v); return nil }, downSend, e)
	t.Cleanup(func() { a.Close(); b.Close() })
	a.Client(t.Context(), websocket.MessageBinary, soundCloudDeviceFrame(soundCloudTextInput("A", "включи песню Гусары орать")))
	comparisonRead(t, upA)
	a.Server(t.Context(), websocket.MessageBinary, comparisonCloud("A", "Other", "Artist"))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("ACK hold not reached")
	}
	b.Client(t.Context(), websocket.MessageBinary, soundCloudDeviceFrame(soundCloudTextInput("B", "включи песню New на Яндекс Музыке")))
	comparisonRead(t, upB)
	original := comparisonCloud("B", "New", "Artist")
	sent := make(chan error, 1)
	go func() { sent <- b.Server(t.Context(), websocket.MessageBinary, original) }()
	select {
	case <-sent:
		close(release)
		t.Fatal("newer Yandex bypassed in-flight music batch")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("newer Yandex remained blocked")
	}
	if got := comparisonWinner(t, down); !bytes.Equal(got, original) {
		t.Fatal("old SC audio escaped after blocked ACK")
	}
	a.wg.Wait()
	b.wg.Wait()
	// A late uncorrelated music Push is now allowed: explicit Yandex has taken
	// ownership, including the textless callback/preload path.
	f := wire.Decode(comparisonCloud("callback", "Next Yandex", "Artist"), wire.ServerToClient)
	callback := comparisonPush("", f.Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives())
	if err := a.Server(t.Context(), websocket.MessageBinary, callback); err != nil {
		t.Fatal(err)
	}
	if got := comparisonRead(t, down); !bytes.Equal(got, callback) {
		t.Fatal("Yandex callback handover lost")
	}
}

func TestNativeMusicComparisonQuarantineSurvivesInformationTurn(t *testing.T) {
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}}
	n, up, down := comparisonRig(t, p)
	comparisonStart(t, n, up, "oldSC")
	n.Server(t.Context(), websocket.MessageBinary, comparisonCloud("oldSC", "Other", "Artist"))
	comparisonWinner(t, down)
	n.wg.Wait()
	n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("info", "какая погода"))
	comparisonRead(t, up) // cancellation or delegated input
	f := wire.Decode(comparisonCloud("late", "Old YM", "Artist"), wire.ServerToClient)
	n.Server(t.Context(), websocket.MessageBinary, comparisonPush("", f.Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives()))
	select {
	case b := <-down:
		if comparisonHasMusic(wire.Decode(b, wire.ServerToClient).Server.GetEvent()) {
			t.Fatal("information turn removed loser quarantine")
		}
	case <-time.After(20 * time.Millisecond):
	}
}

func TestNativeMusicComparisonAggregateBufferIncludesBlockedReplay(t *testing.T) {
	releaseSearch := make(chan struct{})
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Other"}, release: releaseSearch}
	n, up, _ := comparisonRig(t, p)
	entered, releaseWrite := make(chan struct{}), make(chan struct{})
	var once sync.Once
	n.down = func(ctx context.Context, _ websocket.MessageType, _ []byte) error {
		once.Do(func() { close(entered) })
		select {
		case <-releaseWrite:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	n.ComparisonTimeout = 30 * time.Millisecond
	comparisonStart(t, n, up, "buffered")
	n.Server(t.Context(), websocket.MessageBinary, comparisonSpeech("buffered", 88))
	raw := append(binary.BigEndian.AppendUint32(nil, 88), make([]byte, comparisonMaxBytes/2)...)
	n.Server(t.Context(), websocket.MessageBinary, raw)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("replay not held")
	}
	n.mu.Lock()
	c := n.requests["buffered"].comparison
	if c.inflightBytes < len(raw) {
		n.mu.Unlock()
		close(releaseWrite)
		t.Fatal("in-flight replay escaped aggregate accounting")
	}
	n.mu.Unlock()
	close(releaseWrite)
	n.wg.Wait()
}
func comparisonRead(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("missing comparison frame")
		return nil
	}
}
func comparisonStart(t *testing.T, n *Native, up <-chan []byte, id string) {
	t.Helper()
	if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput(id, "включи песню Гусары орать")); err != nil {
		t.Fatal(err)
	}
	comparisonRead(t, up)
}
func comparisonWinner(t *testing.T, down <-chan []byte) []byte {
	t.Helper()
	for {
		b := comparisonRead(t, down)
		f := wire.Decode(b, wire.ServerToClient)
		ds, _, _ := comparisonDirectives(f.Server.GetEvent())
		if len(ds) > 0 {
			return b
		}
	}
}
func comparisonSpeech(id string, stream int64) []byte {
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: "tts", RefMessageId: id, StreamId: new(stream)}, Event: &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &tts.TTtsSpeak{Format: "audio/opus"}}}}})
}
func comparisonPush(id string, ds []*directives.TDirective) []byte {
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: "push", RefMessageId: id}, Event: &server.TServerMessage_TEvent_Push{Push: &push.TPush{Directives: ds}}}}})
}

func TestNativeMusicComparisonHoldsTTSRawAndDeferred(t *testing.T) {
	for _, wantSC := range []bool{false, true} {
		t.Run(map[bool]string{false: "YM", true: "SC"}[wantSC], func(t *testing.T) {
			release := make(chan struct{})
			sc := "Other"
			if wantSC {
				sc = "Гусары орать"
			}
			p := &comparisonProvider{candidate: musicmatch.Candidate{Title: sc}, release: release}
			n, up, down := comparisonRig(t, p)
			comparisonStart(t, n, up, "hold")
			speech := comparisonSpeech("hold", 82)
			raw := append(binary.BigEndian.AppendUint32(nil, 82), []byte("losing voice")...)
			payload := wire.Decode(comparisonCloud("hold", "Other YM", "Artist"), wire.ServerToClient).Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives()
			if !wantSC {
				payload = wire.Decode(comparisonCloud("hold", "Гусары орать", "Artist"), wire.ServerToClient).Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives()
			}
			payload = append(payload,
				&directives.TDirective{Type: "server_action", Name: "@@mm_stack_engine_get_next"},
				&directives.TDirective{Type: "uniproxy_action", Name: "update_memento"},
			)
			cloud := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: "last", RefMessageId: "hold"}, Event: &server.TServerMessage_TEvent_DeferredAliceResponse{DeferredAliceResponse: &deferred.TDeferedAliceResponseProto{IsLast: true, TDeferredResponse: &deferred.TDeferedAliceResponseProto_BaseResponse{BaseResponse: &deferred.TDeferedAliceResponseProto_TBaseResponse{Directives: payload}}}}}}})
			for _, b := range [][]byte{speech, raw, cloud} {
				if err := n.Server(t.Context(), websocket.MessageBinary, b); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-down:
				t.Fatal("pre-winner cloud output leaked")
			default:
			}
			close(release)
			if wantSC {
				comparisonWinner(t, down)
				if p.committed.Load() != 1 {
					t.Fatal("missing SCcommit")
				}
			} else {
				for _, want := range [][]byte{speech, raw, cloud} {
					if b := comparisonRead(t, down); !bytes.Equal(b, want) {
						t.Fatal("YM frame order/bytes changed")
					}
				}
			}
		})
	}
}
func TestNativeMusicComparisonCommitFailureKeepsYandex(t *testing.T) {
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}, commitErr: errors.New("queue failed")}
	n, up, down := comparisonRig(t, p)
	comparisonStart(t, n, up, "fail")
	cloud := comparisonCloud("fail", "Other", "Artist")
	n.Server(t.Context(), websocket.MessageBinary, cloud)
	if b := comparisonWinner(t, down); !bytes.Equal(b, cloud) {
		t.Fatal("lost viable YM fallback")
	}
	n.wg.Wait()
	for len(up) > 0 {
		if wire.Decode(<-up, wire.ClientToServer).Kind() == "CancelRequest" {
			t.Fatal("canceled YM before SC was viable")
		}
	}
}
func TestNativeMusicComparisonUnknownPushAndTimeoutFallback(t *testing.T) {
	for _, mode := range []string{"unknownpush", "timeout", "buffer"} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}, release: release}
			n, up, down := comparisonRig(t, p)
			n.ComparisonTimeout = 40 * time.Millisecond
			comparisonStart(t, n, up, mode)
			cloud := comparisonCloud(mode, "Other", "Artist")
			if mode == "unknownpush" {
				f := wire.Decode(cloud, wire.ServerToClient)
				cloud = comparisonPush("", f.Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives())
			}
			if err := n.Server(t.Context(), websocket.MessageBinary, cloud); err != nil {
				t.Fatal(err)
			}
			if mode == "buffer" {
				speech := comparisonSpeech(mode, 99)
				n.Server(t.Context(), websocket.MessageBinary, speech)
				if err := n.Server(t.Context(), websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, 99), make([]byte, comparisonMaxBytes+1)...)); err != nil {
					t.Fatal(err)
				}
			}
			if b := comparisonRead(t, down); !bytes.Equal(b, cloud) {
				t.Fatal("fallback lost original frame")
			}
			if p.committed.Load() != 0 {
				t.Fatal("fallback played SC")
			}
		})
	}
}
func TestComparisonTicketSupersedesIndependentOfDialogue(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			cfg := dialogue.DefaultConfig()
			cfg.Enabled = enabled
			e := &Engine{Dialogue: dialogue.New(cfg)}
			first := e.Prepare(dialogue.WithSpeaker(t.Context(), "a"))
			second := e.Prepare(dialogue.WithSpeaker(t.Context(), "a"))
			other := e.Prepare(dialogue.WithSpeaker(t.Context(), "b"))
			if e.ComparisonCurrent(first) || !e.ComparisonCurrent(second) || !e.ComparisonCurrent(other) {
				t.Fatal("ticket ordering or isolation failed")
			}
			if e.ComparisonContext(first).Err() == nil {
				t.Fatal("superseded network context not canceled")
			}
			if !e.ComparisonCurrent(e.ComparisonContext(second)) {
				t.Fatal("derived delivery context lost ticket")
			}
		})
	}
}
func TestNativeMusicComparisonCancelWhileCommitWaits(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}, commitEntered: entered, commitRelease: release}
	n, up, down := comparisonRig(t, p)
	comparisonStart(t, n, up, "old")
	n.Server(t.Context(), websocket.MessageBinary, comparisonCloud("old", "Other", "Artist"))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no commit")
	}
	if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("new", "какая погода")); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	old := n.requests["old"]
	n.mu.Unlock()
	select {
	case <-old.comparison.done():
	case <-time.After(time.Second):
		t.Fatal("old comparison not canceled")
	}
	if p.committed.Load() != 0 || p.accepted.Load() != 0 {
		t.Fatal("stale comparison mutated queue")
	}
	select {
	case b := <-down:
		ds, _, _ := comparisonDirectives(wire.Decode(b, wire.ServerToClient).Server.GetEvent())
		if len(ds) > 0 {
			t.Fatal("stale audio leaked")
		}
	default:
	}
}
func TestMusicComparisonTypedPayloads(t *testing.T) {
	original := wire.Decode(comparisonCloud("raw", "Гусары орать", "svk"), wire.ServerToClient).Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives()[0]
	raw, _ := proto.Marshal(original.GetPayload())
	copy := proto.Clone(original).(*directives.TDirective)
	copy.PayloadType = &directives.TDirective_PayloadRaw{PayloadRaw: raw}
	c, ok := comparisonCandidate(copy)
	if !ok || c.Title != "Гусары орать" || c.Artist != "svk" {
		t.Fatal("typed payload not understood")
	}
}
func TestNativeMusicComparisonRefusalAndInitialPlaceholder(t *testing.T) {
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}}
	n, up, down := comparisonRig(t, p)
	comparisonStart(t, n, up, "refusal")
	placeholder := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "refusal"}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{}}}}})
	n.Server(t.Context(), websocket.MessageBinary, placeholder)
	select {
	case <-down:
		t.Fatal("empty initial response prematurely selected SC")
	case <-time.After(20 * time.Millisecond):
	}
	refusal := wire.Decode(placeholder, wire.ServerToClient)
	refusal.Server.GetEvent().GetAliceResponse().VoiceResponse = &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new("Не нашла такую песню")}}
	n.Server(t.Context(), websocket.MessageBinary, aa(refusal.Server))
	comparisonWinner(t, down)
	if p.committed.Load() != 1 {
		t.Fatal("matched refusal did not permit SC")
	}
}
