package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	"quasarbridge/internal/wire"
)

// An intermediate synthesis must not close the original player stream.
func TestSynthesizeSegmentKeepsDeferredAudioOpen(t *testing.T) {
	up := make(chan wire.Frame, 10)
	down := make(chan wire.Frame, 32)
	n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error {
		up <- wire.Decode(b, wire.ClientToServer)
		return nil
	}, func(_ context.Context, _ websocket.MessageType, b []byte) error {
		down <- wire.Decode(b, wire.ServerToClient)
		return nil
	}, nil)
	defer n.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := &nativeRequest{id: "segment-request", ctx: ctx, cancel: cancel, request: &request.TAliceRequestProto{
		Header: &request.TAliceRequestProto_THeader{RequestId: new("app-request")},
	}}
	if err := n.startDeferred(p); err != nil {
		t.Fatal(err)
	}
	var setup wire.Frame
	for {
		select {
		case setup = <-up:
			if setup.Kind() == "TtsGenerate" {
				goto ready
			}
		case <-time.After(time.Second):
			t.Fatal("missing initial TTS")
		}
	}
ready:
	feedSpeech(t, n, setup.Client.GetEvent().GetHeader().GetMessageId(), 72, false)
	done := make(chan error, 1)
	go func() { done <- n.synthesizeSegment(ctx, p, "Первая фраза.") }()
	var segment wire.Frame
	select {
	case segment = <-up:
		if segment.Kind() != "TtsGenerate" {
			t.Fatalf("unexpected event: %s", segment.Kind())
		}
	case <-time.After(time.Second):
		t.Fatal("missing segment synthesis")
	}
	feedSpeech(t, n, segment.Client.GetEvent().GetHeader().GetMessageId(), 88, false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("segment did not finish")
	}
	for len(down) > 0 {
		f := <-down
		if f.Server.GetStreamControl().GetClose() != nil {
			t.Fatal("intermediate segment closed the player")
		}
	}
}

func TestSynthesizeSegmentStopsWhenRequestCanceled(t *testing.T) {
	up := make(chan wire.Frame, 10)
	n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error {
		up <- wire.Decode(b, wire.ClientToServer)
		return nil
	}, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
	defer n.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := &nativeRequest{id: "segment-cancel", ctx: ctx, cancel: cancel, request: &request.TAliceRequestProto{
		Header: &request.TAliceRequestProto_THeader{RequestId: new("app-request")},
	}}
	if err := n.startDeferred(p); err != nil {
		t.Fatal(err)
	}
	var setup wire.Frame
	for {
		select {
		case setup = <-up:
			if setup.Kind() == "TtsGenerate" {
				goto ready
			}
		case <-time.After(time.Second):
			t.Fatal("missing initial TTS")
		}
	}
ready:
	feedSpeech(t, n, setup.Client.GetEvent().GetHeader().GetMessageId(), 72, false)
	done := make(chan error, 1)
	go func() { done <- n.synthesizeSegment(ctx, p, "Первая фраза.") }()
	select {
	case <-up:
	case <-time.After(time.Second):
		t.Fatal("segment did not begin")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("segment did not stop after cancellation")
	}
}
