package bridge

import (
	"bytes"
	"context"
	"testing"

	"github.com/coder/websocket"
	guest "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/guest"
	directives "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	response "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	push "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/push"
	syncpush "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/sync_push"
	syncrequest "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/sync_push/sync_request"
	execute "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/sync_push/sync_request/execute_directive"
	chain "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/sync_push/sync_request/execute_directive_chain"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	endpoint "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/endpoint"
	bio "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/bio"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"quasarbridge/internal/identity"
)

func identityAddPerson(t *testing.T, id, name, encoding string) *directives.TDirective {
	t.Helper()
	d := &directives.TDirective{DirectiveType: endpoint.TCapability_AddEnrolledPersonDirectiveType, Name: "add_enrolled_person_directive"}
	if encoding == "struct" {
		payload, err := structpb.NewStruct(map[string]any{"pers_id": id, "user_name": name, "code": "private-code"})
		if err != nil {
			t.Fatal(err)
		}
		d.PayloadType = &directives.TDirective_Payload{Payload: payload}
	} else {
		payload, err := proto.Marshal(&bio.TBioCapability_TAddEnrolledPersonDirective{PersId: id, UserName: name, Code: "private-code", EnrollmentAudio: []byte("private-enrollment-audio")})
		if err != nil {
			t.Fatal(err)
		}
		d.PayloadType = &directives.TDirective_PayloadRaw{PayloadRaw: payload}
	}
	return d
}

func identityDirectiveFrame(carrier string, ds ...*directives.TDirective) []byte {
	e := &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "voice-one"}}
	switch carrier {
	case "push":
		e.Event = &server.TServerMessage_TEvent_Push{Push: &push.TPush{Directives: ds}}
	case "sync_one":
		e.Event = &server.TServerMessage_TEvent_SyncPush{SyncPush: &syncpush.TSyncPush{SyncRequest: &syncrequest.TSyncRequest{Request: &syncrequest.TSyncRequest_ExecuteDirective{ExecuteDirective: &execute.TExecuteDirective{Directive: ds[0]}}}}}
	case "sync_chain":
		e.Event = &server.TServerMessage_TEvent_SyncPush{SyncPush: &syncpush.TSyncPush{SyncRequest: &syncrequest.TSyncRequest{Request: &syncrequest.TSyncRequest_ExecuteDirectiveChain{ExecuteDirectiveChain: &chain.TExecuteDirectiveChain{Directives: ds}}}}}
	case "response":
		e.Event = &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{Response: &response.TAliceResponseProto_TResponse{Directives: ds}}}
	}
	b := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: e}})
	b = protowire.AppendTag(b, 1800, protowire.BytesType)
	return protowire.AppendString(b, "unknown-server-field")
}

func TestServerSpeakerDirectivesNameOnlyExactMatchedPerson(t *testing.T) {
	for _, carrier := range []string{"push", "sync_one", "sync_chain", "response"} {
		for _, encoding := range []string{"raw", "struct"} {
			t.Run(carrier+"/"+encoding, func(t *testing.T) {
				down := make(chan []byte, 8)
				n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(_ context.Context, _ websocket.MessageType, data []byte) error {
					down <- bytes.Clone(data)
					return nil
				}, nil)
				defer n.Close()
				for _, person := range []struct{ id, name string }{{catalogueHandle, "FirstPerson"}, {"second-opaque-profile", "SecondPerson"}} {
					raw := identityDirectiveFrame(carrier, identityAddPerson(t, person.id, person.name, encoding))
					if err := n.Server(t.Context(), websocket.MessageBinary, raw); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(identityNext(t, down), raw) {
						t.Fatal("catalogue observation mutated server frame")
					}
				}
				for i, person := range []struct{ id, name string }{{catalogueHandle, "FirstPerson"}, {"second-opaque-profile", "SecondPerson"}, {"not-in-catalogue", ""}} {
					requestID := []string{"voice-one", "voice-two", "voice-three"}[i]
					options := identityOptions()
					options.UsersOptions = nil
					options.GuestUserOptions.PersId = new(person.id)
					options.GuestUserOptions.RefMessageId = new(requestID)
					if err := n.Client(t.Context(), websocket.MessageBinary, identityVoice(requestID, options)); err != nil {
						t.Fatal(err)
					}
					n.mu.Lock()
					value := identity.FromContext(n.requests[requestID].ctx)
					n.mu.Unlock()
					if value.Status != "matched" || value.ID != speakerID(person.id) || value.Name != person.name {
						t.Fatal("name was not tied to the exact independently matched PersId")
					}
				}
			})
		}
	}
}

func TestServerSpeakerDirectiveBeforeSuppressionAndLowCandidate(t *testing.T) {
	n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
	defer n.Close()
	options := identityOptions()
	options.UsersOptions = nil
	options.GuestUserOptions.MatchConfidence = new(guest.TGuestOptions_Low)
	if err := n.Client(t.Context(), websocket.MessageBinary, identityVoice("voice-one", options)); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	p := n.requests["voice-one"]
	p.local = true
	n.mu.Unlock()
	raw := identityDirectiveFrame("response", identityAddPerson(t, "private-pers", "CandidatePerson", "raw"))
	if err := n.Server(t.Context(), websocket.MessageBinary, raw); err != nil {
		t.Fatal(err)
	}
	value := identity.FromContext(p.ctx)
	if value.Status != "uncertain" || value.CandidateName != "CandidatePerson" || value.Name != "" || value.ID != "" {
		t.Fatal("suppressed response catalogue lost or promoted candidate")
	}
}

func TestServerSpeakerDirectiveRejectsMalformedAndConflictingNames(t *testing.T) {
	wrongType := identityAddPerson(t, "private-pers", "WrongType", "raw")
	wrongType.DirectiveType = endpoint.TCapability_ShareEnrolledPersonDirectiveType
	wrongShape, _ := structpb.NewStruct(map[string]any{"pers_id": "private-pers", "user_name": 42})
	aliasConflict, _ := structpb.NewStruct(map[string]any{"pers_id": "private-pers", "PersId": "different", "user_name": "WrongPerson"})
	for _, directive := range []*directives.TDirective{
		wrongType,
		{DirectiveType: endpoint.TCapability_AddEnrolledPersonDirectiveType, PayloadType: &directives.TDirective_PayloadRaw{PayloadRaw: []byte{0xff}}},
		{DirectiveType: endpoint.TCapability_AddEnrolledPersonDirectiveType, PayloadType: &directives.TDirective_Payload{Payload: wrongShape}},
		{DirectiveType: endpoint.TCapability_AddEnrolledPersonDirectiveType, PayloadType: &directives.TDirective_Payload{Payload: aliasConflict}},
		identityAddPerson(t, "private-pers", "Invalid\nName", "raw"),
	} {
		n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
		if err := n.Server(t.Context(), websocket.MessageBinary, identityDirectiveFrame("push", directive)); err != nil {
			t.Fatal(err)
		}
		if len(n.speakerCatalog) != 0 {
			t.Fatal("malformed catalogue fact accepted")
		}
		n.Close()
	}
	n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
	defer n.Close()
	if err := n.Server(t.Context(), websocket.MessageBinary, identityDirectiveFrame("push", identityAddPerson(t, "private-pers", "First", "raw"), identityAddPerson(t, "private-pers", "Conflicting", "struct"))); err != nil {
		t.Fatal(err)
	}
	options := identityOptions()
	options.UsersOptions = nil
	if err := n.Client(t.Context(), websocket.MessageBinary, identityVoice("voice-one", options)); err != nil {
		t.Fatal(err)
	}
	value := identity.FromContext(n.requests["voice-one"].ctx)
	if value.Name != "" || value.Status != "matched" {
		t.Fatal("conflicting names must not select an arbitrary name")
	}
}

func TestAutomaticSpeakerDirectoryAcrossConnectionsAndConflicts(t *testing.T) {
	directory := map[string]string{}
	send := func(context.Context, websocket.MessageType, []byte) error { return nil }
	first := NewNative(t.Context(), send, send, nil)
	first.OnSpeakerName = func(id, name string) {
		if len(id) != 64 || id == catalogueHandle {
			t.Fatal("directory callback exposed raw PersId")
		}
		directory[id] = name
	}
	if err := first.Server(t.Context(), websocket.MessageBinary, identityDirectiveFrame("push", identityAddPerson(t, catalogueHandle, "FirstPerson", "raw"), identityAddPerson(t, "second-person", "SecondPerson", "struct"))); err != nil {
		t.Fatal(err)
	}
	first.Close()
	if len(directory) != 2 {
		t.Fatal("automatic directory facts not published")
	}
	second := NewNative(t.Context(), send, send, nil)
	defer second.Close()
	second.ResolveSpeakerName = func(id string) string { return directory[id] }
	for i, person := range []struct{ id, name string }{{catalogueHandle, "FirstPerson"}, {"second-person", "SecondPerson"}} {
		id := []string{"voice-one", "voice-two"}[i]
		options := identityOptions()
		options.UsersOptions = nil
		options.GuestUserOptions.PersId = new(person.id)
		options.GuestUserOptions.RefMessageId = new(id)
		if err := second.Client(t.Context(), websocket.MessageBinary, identityVoice(id, options)); err != nil {
			t.Fatal(err)
		}
		if value := identity.FromContext(second.requests[id].ctx); value.Name != person.name || value.ID != speakerID(person.id) {
			t.Fatal("automatic cache confused independently matched people")
		}
	}
	if err := second.Server(t.Context(), websocket.MessageBinary, identityDirectiveFrame("push", identityAddPerson(t, catalogueHandle, "NewConflictingName", "raw"), identityAddPerson(t, catalogueHandle, "AnotherConflictingName", "struct"))); err != nil {
		t.Fatal(err)
	}
	if value := identity.FromContext(second.requests["voice-one"].ctx); value.Name != "" || value.Status != "matched" {
		t.Fatal("lookup restored a name despite conflicting catalogue evidence")
	}
}
