package wire

import (
	"encoding/binary"
	"errors"
	"io"

	ipc "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
	"google.golang.org/protobuf/proto"
)

// ReadQuasarMessage reads a length-prefixed protobuf from a local service socket.
func ReadQuasarMessage(r io.Reader, limit uint32) (*ipc.QuasarMessage, error) {
	var size uint32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return nil, err
	}
	if size == 0 || size > limit {
		return nil, errors.New("invalid Quasar message length")
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var msg ipc.QuasarMessage
	if err := proto.Unmarshal(body, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// UnwrapRemoting returns the RPC carried by a local service message, if present.
func UnwrapRemoting(msg *ipc.QuasarMessage) *ipc.Remoting {
	if call := msg.GetRemoting(); call != nil {
		return call
	}
	switch msg.GetType() {
	case ipc.QuasarMessage_TYPE_REMOTING, ipc.QuasarMessage_TYPE_ALICE_STATE:
		// Some firmware sends AliceCapability callbacks inside AliceState payloads.
		var call ipc.Remoting
		if proto.Unmarshal(msg.GetPayload(), &call) == nil {
			return &call
		}
	}
	return nil
}

// WriteRemoting sends a local RPC using the direct field on older Max firmware
// or the typed payload on newer firmware.
func WriteRemoting(w io.Writer, call *ipc.Remoting, direct bool) error {
	msg := &ipc.QuasarMessage{Remoting: call}
	if !direct {
		body, err := proto.Marshal(call)
		if err != nil {
			return err
		}
		msg = &ipc.QuasarMessage{Type: ipc.QuasarMessage_TYPE_REMOTING.Enum(), Payload: body}
	}
	frame, err := proto.MarshalOptions{}.MarshalAppend(make([]byte, 4), msg)
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(frame, uint32(len(frame)-4))
	n, err := w.Write(frame)
	if err == nil && n != len(frame) {
		return io.ErrShortWrite
	}
	return err
}
