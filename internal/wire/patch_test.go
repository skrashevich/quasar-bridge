package wire

import (
	"bytes"
	"testing"

	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func fieldBytes(out []byte, num protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(out, num, protowire.BytesType), value)
}

func TestGatePatchesOnlyApprovalAndPreservesDuplicateRequestFields(t *testing.T) {
	h, _ := proto.Marshal(&header.TEventHeader{MessageId: "patch-test"})
	req := fieldBytes(nil, 13, []byte("first-firmware"))
	req = fieldBytes(req, 13, []byte("latest-firmware"))
	event := fieldBytes(fieldBytes(nil, 1, h), 2, req)
	raw := append([]byte("AAPI"), fieldBytes(nil, 1, event)...)
	f := Decode(raw, ClientToServer)
	got, err := EnableApproval(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("first-firmware")) || !bytes.Contains(got, []byte("latest-firmware")) {
		t.Fatal("gate normalized unrelated protobuf fields")
	}
	parsed := Decode(got, ClientToServer)
	if !parsed.Client.GetEvent().GetTextInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
		t.Fatal("approval flag not enabled")
	}
}
