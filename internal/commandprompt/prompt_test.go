package commandprompt

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	ipc "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
	"google.golang.org/protobuf/proto"
)

func TestNativePlaybackEvents(t *testing.T) {
	start := ipc.Remoting_AliceCapabilityListenerMethod_TTS_STARTED
	end := ipc.Remoting_AliceCapabilityListenerMethod_TTS_COMPLETED
	failed := ipc.Remoting_AliceCapabilityListenerMethod_ON_ALICE_ERROR
	frame := func(method ipc.Remoting_AliceCapabilityListenerMethod_Method, id string, envelope *ipc.QuasarMessage_Type) []byte {
		t.Helper()
		listener := &ipc.Remoting_AliceCapabilityListenerMethod{Method: method.Enum(), RequestId: &id}
		callback := &ipc.Remoting{RemoteObjectId: new(capability), Method: &ipc.Remoting_AliceCapabilityListenerMethod_{AliceCapabilityListenerMethod: listener}}
		msg := &ipc.QuasarMessage{Remoting: callback}
		if envelope != nil {
			body, err := proto.Marshal(callback)
			if err != nil {
				t.Fatal(err)
			}
			msg = &ipc.QuasarMessage{Type: envelope, Payload: body}
		}
		raw, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return append(binary.LittleEndian.AppendUint32(nil, uint32(len(raw))), raw...)
	}
	wrapped := ipc.QuasarMessage_TYPE_REMOTING.Enum()
	older := ipc.QuasarMessage_TYPE_ALICE_STATE.Enum()
	for _, tc := range []struct {
		name   string
		frames [][]byte
		ok     bool
	}{
		{"station2 omits completion id", [][]byte{frame(start, "play", wrapped), frame(end, "", wrapped)}, true},
		{"direct and alternate envelopes", [][]byte{frame(start, "play", nil), frame(end, "play", older)}, true},
		{"completed without starting", [][]byte{frame(end, "", wrapped)}, false},
		{"different playback completed", [][]byte{frame(start, "play", wrapped), frame(end, "other", wrapped)}, false},
		{"overlapping speech", [][]byte{frame(start, "play", wrapped), frame(start, "other", wrapped), frame(end, "", wrapped)}, false},
		{"speech failed", [][]byte{frame(start, "play", wrapped), frame(failed, "", wrapped), frame(end, "", wrapped)}, false},
		{"truncated frame", [][]byte{{9, 0, 0, 0, 1}}, false},
		{"oversized frame", [][]byte{binary.LittleEndian.AppendUint32(nil, 512<<10+1)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := waitPlayback(bytes.NewReader(bytes.Join(tc.frames, nil)))
			if (err == nil) != tc.ok {
				t.Errorf("playback result %v, want success %v", err, tc.ok)
			}
		})
	}
}

func TestPlayStopsWaitingWhenCallerCancels(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	cfg := filepath.Join(t.TempDir(), "quasar.cfg")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `{"aliced":{"port":%d}}`, listener.Addr().(*net.TCPAddr).Port), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- Play(ctx, cfg, 123) }()
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled request reported a successful playback")
		}
	case <-time.After(time.Second):
		t.Fatal("still waiting for Alice after cancellation")
	}
}
