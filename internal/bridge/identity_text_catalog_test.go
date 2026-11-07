package bridge

import (
	"context"
	"testing"

	"github.com/coder/websocket"
	user "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/user"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"quasarbridge/internal/identity"
)

func TestTextProfileCatalogueNeverSelectsTextSpeaker(t *testing.T) {
	send := func(context.Context, websocket.MessageType, []byte) error { return nil }
	n := NewNative(t.Context(), send, send, nil)
	defer n.Close()
	options := identityOptions()
	options.GuestUserOptions.PersId = new(catalogueHandle)
	options.GuestUserOptions.RefMessageId = new("text-one")
	profile := identityProfile(catalogueHandle, "CataloguePerson", &user.TUserTags_TBiometrical{Origin: user.TUserTags_TBiometrical_VoiceBiometry, Status: user.TUserTags_TBiometrical_Match, RefMessageId: "text-one"})
	profile.Puid = "101"
	options.UsersOptions = []*user.TUserOptions{profile}
	req := identityRequest(options)
	req.Request.Event.Text = new("question")
	text := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "text-one"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: req}}}})
	if err := n.Client(t.Context(), websocket.MessageBinary, text); err != nil {
		t.Fatal(err)
	}
	if n.speakerCatalog[speakerID(catalogueHandle)].name != "CataloguePerson" {
		t.Fatal("TextInput did not supply same-profile catalogue data")
	}
	if value := identity.FromContext(n.requests["text-one"].ctx); value != (identity.Snapshot{}) || n.requests["text-one"].speaker != nil {
		t.Fatal("TextInput profile or biometric tags selected the current speaker")
	}
	voiceOptions := identityOptions()
	voiceOptions.UsersOptions = nil
	voiceOptions.GuestUserOptions.PersId = new(catalogueHandle)
	if err := n.Client(t.Context(), websocket.MessageBinary, identityVoice("voice-one", voiceOptions)); err != nil {
		t.Fatal(err)
	}
	if value := identity.FromContext(n.requests["voice-one"].ctx); value.Name != "CataloguePerson" || value.Status != "matched" || value.ID != speakerID(catalogueHandle) {
		t.Fatal("future exact VoiceMatch did not receive its own catalogued name")
	}
}
