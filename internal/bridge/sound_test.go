package bridge

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	directives "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	cancellation "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/cancel_request"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	fileplayer "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/audio_file_player"
	"google.golang.org/protobuf/proto"

	"quasarbridge/internal/sound"
	"quasarbridge/internal/wire"
)

func soundInput(id string) []byte {
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("проиграй звук")}}}}}}})
}

func soundSession(t *testing.T, resolver Resolver) (*Native, chan wire.Frame, chan wire.Frame) {
	t.Helper()
	up, down := make(chan wire.Frame, 100), make(chan wire.Frame, 100)
	n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error {
		up <- wire.Decode(b, wire.ClientToServer)
		return nil
	}, func(_ context.Context, _ websocket.MessageType, b []byte) error {
		down <- wire.Decode(b, wire.ServerToClient)
		return nil
	}, resolver)
	t.Cleanup(n.Close)
	return n, up, down
}

func nextSoundFrame(t *testing.T, frames <-chan wire.Frame) wire.Frame {
	t.Helper()
	select {
	case f := <-frames:
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("missing sound response")
		return wire.Frame{}
	}
}

func finalSoundDirectives(t *testing.T, frames <-chan wire.Frame) []*directives.TDirective {
	t.Helper()
	for {
		f := nextSoundFrame(t, frames)
		if r := f.Server.GetEvent().GetAliceResponse(); r != nil && !r.GetResponse().GetIsStreaming() {
			return r.GetResponse().GetDirectives()
		}
		if r := f.Server.GetEvent().GetDeferredAliceResponse(); r != nil && r.GetIsLast() {
			return r.GetBaseResponse().GetDirectives()
		}
	}
}

func TestNativeSoundFinalURLDirective(t *testing.T) {
	for _, tc := range []struct{ url, format string }{
		{"https://audio.example/test.mp3", "MP3"},
		{"https://audio.example/live.M3U8?token=test", "HLS"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			n, up, down := soundSession(t, resolverFunc(func(ctx context.Context, _ string) (Resolution, error) {
				err := sound.Play(ctx, sound.Request{URL: tc.url})
				return Resolution{Route: "llm", Answer: "Звук поставлен в очередь."}, err
			}))
			if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("url")); err != nil {
				t.Fatal(err)
			}
			var ds []*directives.TDirective
			for {
				f := nextSoundFrame(t, down)
				if r := f.Server.GetEvent().GetAliceResponse(); r != nil {
					if r.GetVoiceResponse().GetShouldListen() {
						t.Fatal("URL playback reopened listening")
					}
					if r.GetVoiceResponse().GetHasVoiceResponse() || r.GetVoiceResponse().GetOutputSpeech().GetText() != "" {
						t.Fatal("URL playback must not be masked by spoken confirmation")
					}
					if len(r.GetResponse().GetCards()) != 1 || r.GetResponse().GetCards()[0].GetText() != "Звук поставлен в очередь." {
						t.Fatal("URL playback lost the textual answer")
					}
					ds = r.GetResponse().GetDirectives()
					break
				}
			}
			if len(ds) != 1 || ds[0].GetType() != "client_action" || ds[0].GetName() != "audio_play" {
				t.Fatalf("missing audio_play directive: %v", ds)
			}
			stream := ds[0].GetPayload().GetFields()["stream"].GetStructValue().GetFields()
			if stream["url"].GetStringValue() != tc.url || stream["format"].GetStringValue() != tc.format {
				t.Fatalf("incorrect URL or codec: %v", stream)
			}
			if ds[0].GetPayload().GetFields()["screen_type"].GetStringValue() != "Default" {
				t.Fatal("URL playback requires a supported screen type")
			}
			n.Close()
			for len(up) > 0 {
				if f := <-up; f.Kind() == "TtsGenerate" {
					t.Fatal("URL playback synthesized a spoken confirmation")
				}
			}
		})
	}
}

func TestNativeSoundBuiltinsDeduplicateAndIsolateSessions(t *testing.T) {
	for _, tc := range []struct {
		sound string
		id    fileplayer.TAudioFilePlayerCapability_EStoredSound
	}{
		{"notification", fileplayer.TAudioFilePlayerCapability_NotificationSound},
		{"success", fileplayer.TAudioFilePlayerCapability_AliceIoTSoundsDone},
		{"error", fileplayer.TAudioFilePlayerCapability_SpotterError},
		{"timer", fileplayer.TAudioFilePlayerCapability_Timer},
	} {
		t.Run(tc.sound, func(t *testing.T) {
			n, _, down := soundSession(t, resolverFunc(func(ctx context.Context, _ string) (Resolution, error) {
				req := sound.Request{Sound: tc.sound}
				if err := sound.Play(ctx, req); err != nil {
					return Resolution{Route: "llm"}, err
				}
				if err := sound.Play(ctx, req); err != nil {
					t.Errorf("duplicate rejected: %v", err)
				}
				if err := sound.Play(ctx, sound.Request{URL: "https://audio.example/second.mp3"}); err == nil {
					t.Error("different second sound accepted")
				}
				return Resolution{Route: "llm", Answer: "Готово."}, nil
			}))
			other, _, otherDown := soundSession(t, resolverFunc(func(context.Context, string) (Resolution, error) {
				return Resolution{Route: "llm", Answer: "Другой ответ."}, nil
			}))
			for _, session := range []*Native{n, other} {
				if err := session.Client(t.Context(), websocket.MessageBinary, soundInput("same-id")); err != nil {
					t.Fatal(err)
				}
			}
			ds := finalSoundDirectives(t, down)
			if len(ds) != 1 || ds[0].GetName() != "local_audio_file_play" {
				t.Fatalf("missing single built-in sound: %v", ds)
			}
			var playback fileplayer.TAudioFilePlayerCapability_TLocalAudioFilePlayDirective
			if err := proto.Unmarshal(ds[0].GetPayloadRaw(), &playback); err != nil {
				t.Fatal(err)
			}
			if playback.GetSoundToPlay() != tc.id || playback.GetPlayTimes() != 1 || playback.GetLooped() {
				t.Fatalf("wrong built-in playback: %v", &playback)
			}
			if ds := finalSoundDirectives(t, otherDown); len(ds) != 0 {
				t.Fatalf("sound leaked into another session: %v", ds)
			}
		})
	}
}

func TestNativeSoundDroppedOnResolutionFailureOrCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "canceled"}[canceled], func(t *testing.T) {
			queued := make(chan context.Context, 1)
			n, _, down := soundSession(t, resolverFunc(func(ctx context.Context, _ string) (Resolution, error) {
				err := sound.Play(ctx, sound.Request{Sound: "notification"})
				queued <- ctx
				if err != nil {
					return Resolution{Route: "llm"}, err
				}
				if canceled {
					<-ctx.Done()
					return Resolution{Route: "llm"}, ctx.Err()
				}
				return Resolution{Route: "llm"}, errors.New("final LLM request failed")
			}))
			if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("drop")); err != nil {
				t.Fatal(err)
			}
			ctx := <-queued
			if !canceled {
				if ds := finalSoundDirectives(t, down); len(ds) != 0 {
					t.Fatalf("failed resolution emitted sound: %v", ds)
				}
				return
			}
			cancel := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "cancel"}, Event: &client.TClientMessage_TEvent_CancelRequest{CancelRequest: &cancellation.TCancelRequest{RequestMessageId: "drop"}}}}})
			if err := n.Client(t.Context(), websocket.MessageBinary, cancel); err != nil {
				t.Fatal(err)
			}
			n.Close()
			if err := sound.Play(ctx, sound.Request{Sound: "notification"}); err == nil {
				t.Fatal("canceled request accepted playback")
			}
			if len(down) != 0 {
				t.Fatal("canceled request produced a response")
			}
		})
	}
}

type soundProgressResolver struct{ resolverFunc }

func (soundProgressResolver) Decide(context.Context, string) RouteDecision {
	return RouteDecision{Route: "llm"}
}
func (r soundProgressResolver) ResolveDecision(ctx context.Context, text string, _ RouteDecision) (Resolution, error) {
	return r.Resolve(ctx, text)
}

func TestNativeSoundOnlyOnFinalDeferredResponse(t *testing.T) {
	release := make(chan struct{})
	n, up, down := soundSession(t, soundProgressResolver{resolverFunc(func(ctx context.Context, _ string) (Resolution, error) {
		if err := sound.Play(ctx, sound.Request{URL: "https://audio.example/test.mp3"}); err != nil {
			return Resolution{Route: "llm"}, err
		}
		select {
		case <-ctx.Done():
			return Resolution{}, ctx.Err()
		case <-release:
			return Resolution{Route: "llm", Answer: "Готово."}, nil
		}
	})})
	n.ProgressAfter = time.Millisecond
	if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("deferred-sound")); err != nil {
		t.Fatal(err)
	}
	for {
		f := nextSoundFrame(t, up)
		if f.Kind() == "TtsGenerate" {
			feedSpeech(t, n, f.Client.GetEvent().GetHeader().GetMessageId(), 72, false)
			break
		}
	}
	for len(down) > 0 {
		f := <-down
		for _, d := range f.Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives() {
			if d.GetName() == "audio_play" {
				t.Fatal("intermediate response played sound")
			}
		}
		if len(f.Server.GetEvent().GetDeferredAliceResponse().GetBaseResponse().GetDirectives()) != 0 {
			t.Fatal("intermediate deferred response played sound")
		}
	}
	close(release)
	sawEOS, sawClose := false, false
	for {
		f := nextSoundFrame(t, down)
		if f.Kind() == "opaque" && len(f.Raw) > 31 && string(f.Raw[4:8]) == "OggS" && f.Raw[9]&4 != 0 {
			page := f.Raw[4:]
			if binary.BigEndian.Uint32(f.Raw[:4]) != 72 || oggChecksum(page) != binary.LittleEndian.Uint32(page[22:26]) || len(page) != 31 || string(page[28:]) != string([]byte{0xf8, 0xff, 0xfe}) {
				t.Fatal("invalid final silent Opus packet")
			}
			sawEOS = true
		}
		if c := f.Server.GetStreamControl(); c != nil && c.GetClose() != nil {
			if !sawEOS || c.GetStreamId() != 72 || c.GetMessageId() != "deferred-sound" {
				t.Fatal("silent stream must finish before its correlated Close")
			}
			sawClose = true
		}
		if r := f.Server.GetEvent().GetDeferredAliceResponse(); r != nil && r.GetIsLast() {
			if !sawClose {
				t.Fatal("URL playback began before silent Dialog playback ended")
			}
			if r.GetBaseResponse().GetShouldListen() {
				t.Fatal("deferred URL playback reopened listening")
			}
			ds := r.GetBaseResponse().GetDirectives()
			if len(ds) != 1 || ds[0].GetName() != "audio_play" || r.GetBaseResponse().GetText() != "Готово." {
				t.Fatalf("missing final deferred sound/text: %v", ds)
			}
			unknown := r.GetBaseResponse().ProtoReflect().GetUnknown()
			if len(unknown) != 2 || unknown[0] != 0x12 || unknown[1] != 0 {
				t.Fatalf("URL playback retained legacy Voice confirmation: %x", unknown)
			}
			break
		}
	}
	n.Close()
	for len(up) > 0 {
		if f := <-up; f.Kind() == "TtsGenerate" {
			t.Fatal("URL playback synthesized final TTS under the sound")
		}
	}
}
