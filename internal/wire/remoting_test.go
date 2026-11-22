package wire

import (
	"bytes"
	"encoding/binary"
	"testing"

	ipc "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
	"google.golang.org/protobuf/proto"
)

func TestWriteRemotingFirmwareFormats(t *testing.T) {
	call := &ipc.Remoting{RemoteObjectId: new("AliceCapability")}
	payload, err := proto.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		direct bool
		want   *ipc.QuasarMessage
	}{
		{"older Max", true, &ipc.QuasarMessage{Remoting: call}},
		{"Station 2", false, &ipc.QuasarMessage{Type: ipc.QuasarMessage_TYPE_REMOTING.Enum(), Payload: payload}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := WriteRemoting(&out, call, tc.direct); err != nil {
				t.Fatal(err)
			}
			frame := out.Bytes()
			if len(frame) < 4 || int(binary.LittleEndian.Uint32(frame)) != len(frame)-4 {
				t.Fatalf("invalid length prefix in %x", frame)
			}
			var msg ipc.QuasarMessage
			if err := proto.Unmarshal(frame[4:], &msg); err != nil || !proto.Equal(&msg, tc.want) {
				t.Fatalf("unexpected RPC envelope %v (decode error %v)", &msg, err)
			}
		})
	}
}
