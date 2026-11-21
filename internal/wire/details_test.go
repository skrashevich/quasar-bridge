package wire

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestMatchedUserDetails(t *testing.T) {
	payload, err := structpb.NewStruct(map[string]any{
		"pair":   map[string]any{"name": "Authorization", "value": "fixture-header-secret"},
		"labels": []any{nil, true, "matched"},
	})
	if err != nil {
		t.Fatal(err)
	}
	structBytes, err := proto.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	score := fieldBytes(nil, 1, []byte("fixture-user"))
	score = protowire.AppendFixed64(protowire.AppendTag(score, 2, protowire.Fixed64Type), 0x3fec000000000000) // 0.875
	for _, tc := range []struct {
		name  string
		field protowire.Number
		data  []byte
		want  []string
	}{
		{"Struct", 8, structBytes, []string{`"labels":[null,true,"matched"]`, `"value":"[скрыто]"`}},
		{"RawStruct", 24, structBytes, []string{`"labels":[null,true,"matched"]`, `"value":"[скрыто]"`}},
		{"RawJSON", 24, []byte(`{"user_id":9223372036854775807,"score":0.75,"cookie":"fixture-cookie"}`), []string{`"user_id":"9223372036854775807"`, `"score":0.75`, `"cookie":"[скрыто]"`}},
		{"RawBinary", 24, []byte{0xff, 0xfe}, []string{"двоичные данные, 2 байт"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := fieldBytes(nil, 11, fieldBytes(nil, 1, score))
			event = fieldBytes(event, tc.field, tc.data)
			request := fieldBytes(fieldBytes(nil, 1, event), 5, fieldBytes(nil, 1, []byte("fixture-oauth-secret")))
			matched := fieldBytes(nil, 3, request)
			header := fieldBytes(nil, 1, []byte("fixture-message"))
			client := fieldBytes(fieldBytes(nil, 1, header), 7, matched)
			raw := fieldBytes(fieldBytes(nil, 1, client), 901, []byte("fixture-unknown"))
			frame := Decode(append([]byte("AAPI"), raw...), ClientToServer)
			if frame.Kind() != "MatchedUser" {
				t.Fatalf("kind = %q", frame.Kind())
			}
			details, err := json.Marshal(frame.Details())
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range append(tc.want, `"MatchedUser":`, `"MessageId":"fixture-message"`, `"UserId":"fixture-user"`, `"Score":0.875`, `"OAuthToken":"[скрыто]"`, `"_unknown_bytes":`) {
				if !bytes.Contains(details, []byte(want)) {
					t.Errorf("missing %s in %s", want, details)
				}
			}
			for _, forbidden := range []string{"fixture-oauth-secret", "fixture-header-secret", "fixture-cookie", "fixture-unknown"} {
				if bytes.Contains(details, []byte(forbidden)) {
					t.Errorf("unexpected %s in packet details", forbidden)
				}
			}
		})
	}
}

func TestLegacyPacketDetails(t *testing.T) {
	header := fieldBytes(nil, 10, []byte("LegacyProbe"))
	for _, tc := range []struct{ name, want string }{
		{"NAlice.NAliceApi.TEventHeader", `"MessageId":"fixture-id"`},
		{"example.UnregisteredPayload", "двоичные данные"},
	} {
		anyValue := fieldBytes(fieldBytes(nil, 1, []byte("type.googleapis.com/"+tc.name)), 2, fieldBytes(nil, 1, []byte("fixture-id")))
		f := Decode(fieldBytes(fieldBytes(nil, 1, header), 2, anyValue), ServerToClient)
		got, err := json.Marshal(f.Details())
		if err != nil || !bytes.Contains(got, []byte(tc.name)) || !bytes.Contains(got, []byte(tc.want)) {
			t.Fatalf("%s: %s (%v)", tc.name, got, err)
		}
	}
}
