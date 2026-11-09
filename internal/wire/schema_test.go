package wire

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func schemaVarint(out []byte, number protowire.Number, value uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(out, number, protowire.VarintType), value)
}

func TestUpdatedSchemaRecognizesNewEvents(t *testing.T) {
	// Encode published field numbers directly, independently of the generated
	// bindings. These event variants did not exist in the previous schemas.
	ping := schemaVarint(schemaVarint(schemaVarint(nil, 1, 17), 2, 250), 3, 500)
	tts := fieldBytes(fieldBytes(nil, 1, []byte("source-request")), 2, []byte("hello"))
	tts = schemaVarint(schemaVarint(tts, 3, 1), 4, 7)
	ack := fieldBytes(nil, 1, schemaVarint(nil, 1, 9))
	for _, tc := range []struct {
		name      string
		direction Direction
		number    protowire.Number
		payload   []byte
		checks    []schemaCheck
	}{
		{"Ping", ClientToServer, 33, ping, []schemaCheck{{[]protoreflect.FieldNumber{1}, uint32(17)}, {[]protoreflect.FieldNumber{2}, uint32(250)}, {[]protoreflect.FieldNumber{3}, uint32(500)}}},
		{"DeferredTtsGenerate", ClientToServer, 34, tts, []schemaCheck{{[]protoreflect.FieldNumber{1}, "source-request"}, {[]protoreflect.FieldNumber{2}, "hello"}, {[]protoreflect.FieldNumber{3}, true}, {[]protoreflect.FieldNumber{4}, uint64(7)}}},
		{"ChunkAck", ServerToClient, 39, ack, []schemaCheck{{[]protoreflect.FieldNumber{1, 1}, uint64(9)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := fieldBytes(fieldBytes(nil, 1, []byte("schema-test")), 2, []byte("source-request"))
			header = schemaVarint(header, 4, 31)
			payloadUnknown := fieldBytes(nil, 902, []byte("future-payload"))
			payload := append(bytes.Clone(tc.payload), payloadUnknown...)
			event := fieldBytes(fieldBytes(nil, 1, header), tc.number, payload)
			eventUnknown := fieldBytes(nil, 900, []byte("future-event"))
			event = append(event, eventUnknown...)
			rootUnknown := fieldBytes(nil, 901, []byte("future-envelope"))
			raw := append(fieldBytes(nil, 1, event), rootUnknown...)
			raw = append([]byte("AAPI"), raw...)

			frame := Decode(raw, tc.direction)
			for round := range 2 {
				if frame.Kind() != tc.name {
					t.Fatalf("round %d: kind = %q, want %q", round, frame.Kind(), tc.name)
				}
				var root protoreflect.Message
				if tc.direction == ClientToServer {
					root = frame.Client.ProtoReflect()
				} else {
					root = frame.Server.ProtoReflect()
				}
				eventMessage := schemaMessage(t, root, 1)
				headerMessage := schemaMessage(t, eventMessage, 1)
				for _, check := range []schemaCheck{{[]protoreflect.FieldNumber{1}, "schema-test"}, {[]protoreflect.FieldNumber{2}, "source-request"}, {[]protoreflect.FieldNumber{4}, int64(31)}} {
					check.assert(t, headerMessage)
				}
				payloadMessage := schemaMessage(t, eventMessage, protoreflect.FieldNumber(tc.number))
				for _, check := range tc.checks {
					check.assert(t, payloadMessage)
				}
				for _, check := range []struct {
					message protoreflect.Message
					unknown []byte
				}{{root, rootUnknown}, {eventMessage, eventUnknown}, {payloadMessage, payloadUnknown}} {
					if !bytes.Equal(check.message.GetUnknown(), check.unknown) {
						t.Fatalf("round %d: %s lost unknown fields", round, check.message.Descriptor().FullName())
					}
				}
				if round == 0 {
					out, err := frame.Marshal()
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.HasPrefix(out, []byte("AAPI")) {
						t.Fatal("lost AAPI marker")
					}
					frame = Decode(out, tc.direction)
				}
			}
		})
	}
}

type schemaCheck struct {
	path []protoreflect.FieldNumber
	want any
}

func (check schemaCheck) assert(t *testing.T, message protoreflect.Message) {
	t.Helper()
	for _, number := range check.path[:len(check.path)-1] {
		message = schemaMessage(t, message, number)
	}
	number := check.path[len(check.path)-1]
	field := message.Descriptor().Fields().ByNumber(number)
	if field == nil || !message.Has(field) {
		t.Fatalf("%s missing field %d", message.Descriptor().FullName(), number)
	}
	if got := message.Get(field).Interface(); got != check.want {
		t.Fatalf("%s = %v, want %v", field.FullName(), got, check.want)
	}
}

func schemaMessage(t *testing.T, parent protoreflect.Message, number protoreflect.FieldNumber) protoreflect.Message {
	t.Helper()
	field := parent.Descriptor().Fields().ByNumber(number)
	if field == nil || field.Kind() != protoreflect.MessageKind || !parent.Has(field) {
		t.Fatalf("%s missing message field %d", parent.Descriptor().FullName(), number)
	}
	return parent.Get(field).Message()
}

func TestLegacySpeechkitSchemaSurvivesRoundTrip(t *testing.T) {
	header := fieldBytes(fieldBytes(nil, 1, []byte("legacy-test")), 10, []byte("LegacyProbe"))
	value := fieldBytes(nil, 1, []byte("hello"))
	payload := fieldBytes(fieldBytes(nil, 1, []byte("type.googleapis.com/example.LegacyPayload")), 2, value)
	unknown := fieldBytes(nil, 901, []byte("future-legacy-field"))
	raw := append(fieldBytes(fieldBytes(nil, 1, header), 2, payload), unknown...)
	for _, prefix := range []string{"", "AAPI"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			frame := Decode(append([]byte(prefix), raw...), ClientToServer)
			if frame.Legacy == nil || frame.Kind() != "LegacyProbe" {
				t.Fatalf("legacy envelope decoded as %q", frame.Kind())
			}
			frame.Legacy.Header.Name = new("LegacyChanged")
			out, err := frame.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			got := Decode(out, ClientToServer)
			if string(got.Prefix) != prefix || got.Legacy == nil || got.Kind() != "LegacyChanged" || got.Legacy.GetHeader().GetMessageId() != "legacy-test" {
				t.Fatalf("legacy envelope changed during round trip: %+v", got)
			}
			if got.Legacy.GetPayload().GetTypeUrl() != "type.googleapis.com/example.LegacyPayload" || !bytes.Equal(got.Legacy.GetPayload().GetValue(), value) || !bytes.Equal(got.Legacy.ProtoReflect().GetUnknown(), unknown) {
				t.Fatal("legacy payload or unknown fields changed during round trip")
			}
		})
	}
}
