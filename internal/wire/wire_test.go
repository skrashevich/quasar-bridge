package wire

import (
	"bytes"
	"testing"

	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	asr "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	stream "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/stream_control"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestDecodeNativeRequestPreservesIdentityAndUnknownFields(t *testing.T) {
	m := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: "request-17", StreamId: new(int64(31))},
		Event:  &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Firmware: new("test-firmware")}},
	}}}
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	raw = protowire.AppendTag(raw, 999, protowire.BytesType)
	raw = protowire.AppendString(raw, "future-field")
	f := Decode(raw, ClientToServer)
	if f.Client == nil || f.Client.GetEvent().GetHeader().GetMessageId() != "request-17" {
		t.Fatalf("lost request identity: %+v", f)
	}
	if f.Kind() != "TextInput" {
		t.Fatalf("kind=%q", f.Kind())
	}
	f.Client.GetEvent().GetTextInput().Firmware = new("changed-firmware")
	got, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var decoded client.TClientMessage
	if err := proto.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.GetEvent().GetTextInput().GetFirmware() != "changed-firmware" || !bytes.Equal(decoded.ProtoReflect().GetUnknown(), mWithUnknown(raw)) {
		t.Fatal("mutation lost unknown fields")
	}
}

func mWithUnknown(raw []byte) []byte {
	var m client.TClientMessage
	_ = proto.Unmarshal(raw, &m)
	return m.ProtoReflect().GetUnknown()
}

func TestOpaqueAudioIsNotMistakenForRequest(t *testing.T) {
	for _, raw := range [][]byte{{0xff, 0x0a, 0x80, 0x00}, {0, 1, 2, 3}, []byte("raw-opus")} {
		f := Decode(raw, ClientToServer)
		if f.Client != nil || f.Kind() != "opaque" {
			t.Fatalf("audio misparsed: %+v", f)
		}
	}
}

func TestServerEventUsesServerSchema(t *testing.T) {
	m := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
		Header: &header.TEventHeader{MessageId: "response-17", RefMessageId: "request-17"},
		Event:  &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true)}},
	}}}
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	f := Decode(raw, ServerToClient)
	if f.Kind() != "AsrResult" {
		t.Fatalf("kind=%q", f.Kind())
	}
}

func TestNativeAAPrefixIsPreservedAndFuturePrefixIsOpaque(t *testing.T) {
	m := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "prefix-test"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{}}}}}
	data, _ := proto.Marshal(m)
	f := Decode(append([]byte("AAPI"), data...), ClientToServer)
	if f.Kind() != "TextInput" {
		t.Fatalf("AA kind=%q", f.Kind())
	}
	roundtrip, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundtrip[:4], []byte("AAPI")) {
		t.Fatal("lost control prefix")
	}
	if got := Decode(append([]byte{'A', 'B'}, data...), ClientToServer); got.Kind() != "opaque" {
		t.Fatal("future encoding interpreted as current protocol")
	}
}

func TestAAPITransportMarkerIsNotProtobuf(t *testing.T) {
	m := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "id"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{}}}}}
	body, _ := proto.Marshal(m)
	raw := append([]byte("AAPI"), body...)
	f := Decode(raw, ClientToServer)
	if string(f.Prefix) != "AAPI" {
		t.Fatalf("marker=%q", f.Prefix)
	}
	got, err := f.Marshal()
	if err != nil || !bytes.Equal(raw, got) {
		t.Fatal("transport marker was absorbed into protobuf unknown fields")
	}
}

func TestDecodeServerCloseWithoutMessageID(t *testing.T) {
	msg := &server.TServerMessage{Message: &server.TServerMessage_StreamControl{StreamControl: &stream.TStreamControl{StreamId: 72, Action: &stream.TStreamControl_Close{Close: &stream.TStreamControl_TActionClose{}}}}}
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	raw = append([]byte("AAPI"), raw...)
	f := Decode(raw, ServerToClient)
	if f.Kind() != "StreamControl" || f.Server.GetStreamControl().GetStreamId() != 72 {
		t.Fatal("valid Close became opaque")
	}
	out, err := f.Marshal()
	if err != nil || !bytes.Equal(out, raw) {
		t.Fatal("Close changed during round trip")
	}
}
