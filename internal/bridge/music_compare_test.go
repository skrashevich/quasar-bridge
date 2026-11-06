package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	directives "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	response "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	push "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/push"
	tts "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/types/known/structpb"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/musicmatch"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/wire"
)

type comparisonProvider struct {
	soundCloudStub
	candidate     musicmatch.Candidate
	release       <-chan struct{}
	fail          error
	commitErr     error
	commitEntered chan struct{}
	commitRelease <-chan struct{}
	committed     atomic.Int32
	accepted      atomic.Int32
}

func TestNativeMusicComparisonDropsLateLoserMedia(t *testing.T) {
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать", Artist: "svk"}}
	n, up, down := soundSession(t, &Engine{SoundCloud: p})
	if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("late", "включи песню Гусары орать")); err != nil {
		t.Fatal(err)
	}
	nextSoundFrame(t, up)
	if err := n.Server(t.Context(), websocket.MessageBinary, comparisonCloud("late", "Другое", "Artist")); err != nil {
		t.Fatal(err)
	}
	finalSoundDirectives(t, down)
	n.wg.Wait()
	late := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: "late-tts", RefMessageId: "late", StreamId: new(int64(77))}, Event: &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &tts.TTtsSpeak{Format: "audio/opus"}}}}})
	if err := n.Server(t.Context(), websocket.MessageBinary, late); err != nil {
		t.Fatal(err)
	}
	if err := n.Server(t.Context(), websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, 77), []byte("losing audio")...)); err != nil {
		t.Fatal(err)
	}
	f := wire.Decode(comparisonCloud("ignored", "Late YM", "Artist"), wire.ServerToClient)
	ds := f.Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives()
	unknown := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: "unknown-late"}, Event: &server.TServerMessage_TEvent_Push{Push: &push.TPush{Directives: ds}}}}})
	if err := n.Server(t.Context(), websocket.MessageBinary, unknown); err != nil {
		t.Fatal(err)
	}
	if err := n.Server(t.Context(), websocket.MessageBinary, comparisonSpeech("", 79)); err != nil {
		t.Fatal(err)
	}
	if err := n.Server(t.Context(), websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, 79), []byte("unknown late voice")...)); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-down:
		t.Fatalf("late losing output leaked: %s", f.Kind())
	case <-time.After(30 * time.Millisecond):
	}
}

func TestNativeMusicComparisonKeepsUnrelatedPushSpeech(t *testing.T) {
	p := &comparisonProvider{candidate: musicmatch.Candidate{Title: "Гусары орать"}}
	n, up, down := soundSession(t, &Engine{SoundCloud: p})
	n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("song", "включи песню Гусары орать"))
	nextSoundFrame(t, up)
	n.Server(t.Context(), websocket.MessageBinary, comparisonCloud("song", "Other", "Artist"))
	finalSoundDirectives(t, down)
	n.wg.Wait()
	service := comparisonPush("", []*directives.TDirective{{Type: "client_action", Name: "iot_sync"}})
	if err := n.Server(t.Context(), websocket.MessageBinary, service); err != nil {
		t.Fatal(err)
	}
	nextSoundFrame(t, down)
	if err := n.Server(t.Context(), websocket.MessageBinary, comparisonSpeech("push", 81)); err != nil {
		t.Fatal(err)
	}
	if f := nextSoundFrame(t, down); f.Kind() != "TtsSpeak" {
		t.Fatal("unrelated service speech missing")
	}
	if err := n.Server(t.Context(), websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, 81), []byte("notification")...)); err != nil {
		t.Fatal(err)
	}
	nextSoundFrame(t, down)
}

func (p *comparisonProvider) ComparisonEnabled(_ context.Context, text string) bool {
	return strings.HasPrefix(text, "включи песню") && !strings.Contains(text, "на Яндекс")
}
func (p *comparisonProvider) IsDefault() bool                    { return true }
func (p *comparisonProvider) AcceptYandex(context.Context) error { p.accepted.Add(1); return nil }
func (p *comparisonProvider) Prepare(ctx context.Context, _ string) (musicmatch.Prepared, error) {
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.fail != nil {
		return nil, p.fail
	}
	return &comparisonPrepared{p}, nil
}

type comparisonPrepared struct{ p *comparisonProvider }

func (p *comparisonPrepared) Candidate() musicmatch.Candidate { return p.p.candidate }
func (p *comparisonPrepared) Commit(ctx context.Context) error {
	if p.p.commitEntered != nil {
		close(p.p.commitEntered)
		select {
		case <-p.p.commitRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if p.p.commitErr != nil {
		return p.p.commitErr
	}
	p.p.committed.Add(1)
	return sound.Play(ctx, sound.Request{URL: "https://audio.example/sc.m3u8", Title: p.p.candidate.Title})
}
func (p *comparisonPrepared) Close() {}
func comparisonCloud(id, title, artist string) []byte {
	payload, _ := structpb.NewStruct(map[string]any{"stream": map[string]any{"id": "ym-stream", "duration_ms": 120000, "url": "https://audio.example/ym"}, "metadata": map[string]any{"title": title, "subtitle": artist}, "scenario_meta": map[string]any{"queue_item": "opaque-do-not-rebuild"}})
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: "ym-answer", RefMessageId: id}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{Response: &response.TAliceResponseProto_TResponse{Directives: []*directives.TDirective{{Type: "client_action", Name: "audio_play", PayloadType: &directives.TDirective_Payload{Payload: payload}}}}}}}}})
}
func TestMusicComparisonRoutesWithoutLLM(t *testing.T) {
	e := &Engine{SoundCloud: &comparisonProvider{}}
	ctx := e.Prepare(dialogue.WithSpeaker(t.Context(), "speaker"))
	if got := e.Decide(ctx, "включи песню Гусары орать").Route; got != "music_compare" {
		t.Fatalf("route=%s", got)
	}
}
func TestNativeMusicComparisonOnlyWinnerPlays(t *testing.T) {
	for _, tc := range []struct {
		name, ym, sc string
		fail         bool
		wantSC       bool
	}{
		{"SC better", "Другая песня", "Гусары орать", false, true},
		{"YM better", "Гусары орать", "Другая песня", false, false},
		{"SC error", "Гусары орать", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &comparisonProvider{candidate: musicmatch.Candidate{Title: tc.sc, Artist: "svk"}}
			if tc.fail {
				p.fail = errors.New("unavailable")
			}
			e := &Engine{SoundCloud: p}
			up := make(chan []byte, 32)
			down := make(chan []byte, 32)
			n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- bytes.Clone(b); return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- bytes.Clone(b); return nil }, e)
			t.Cleanup(n.Close)
			if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("compare", "включи песню Гусары орать")); err != nil {
				t.Fatal(err)
			}
			select {
			case b := <-up:
				if wire.Decode(b, wire.ClientToServer).Kind() != "TextInput" {
					t.Fatal("original not delegated")
				}
			case <-time.After(time.Second):
				t.Fatal("missing cloud input")
			}
			original := comparisonCloud("compare", tc.ym, "svk")
			if err := n.Server(t.Context(), websocket.MessageBinary, original); err != nil {
				t.Fatal(err)
			}
			var played []byte
			for played == nil {
				select {
				case b := <-down:
					if ds := wire.Decode(b, wire.ServerToClient).Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives(); len(ds) > 0 {
						played = b
					}
				case <-time.After(time.Second):
					t.Fatal("no winner")
				}
			}
			if tc.wantSC {
				if p.committed.Load() != 1 || bytes.Equal(played, original) {
					t.Fatal("SC did not win once")
				}
			} else {
				if p.committed.Load() != 0 || p.accepted.Load() != 1 || !bytes.Equal(played, original) {
					t.Fatal("YM bytes/session changed")
				}
			}
		})
	}
}
