package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	fullinfo "github.com/skrashevich/quasar-go-genproto/proto/alice/library/blackbox/proto"
	blackbox "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/blackbox"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	guest "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/guest"
	user "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/user"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	asr "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	synchronize "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/synchronize_state_request"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/encoding/protowire"
	"quasarbridge/internal/identity"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

func identityOptions() *request.TAliceRequestProto_TRequest_TAdditionalOptions {
	return &request.TAliceRequestProto_TRequest_TAdditionalOptions{GuestUserOptions: &guest.TGuestOptions{PersId: new("private-pers"), GuestOrigin: new(guest.TGuestOptions_VoiceBiometry), Status: new(guest.TGuestOptions_Match), MatchConfidence: new(guest.TGuestOptions_High), RefMessageId: new("voice-one"), OAuthToken: new("private-token"), YandexUID: new("private-uid")}, UsersOptions: []*user.TUserOptions{identityProfile("private-pers", "Алексей", nil)}}
}

func identityProfile(id, name string, bio *user.TUserTags_TBiometrical) *user.TUserOptions {
	return &user.TUserOptions{PersId: id, OAuthToken: "private-user-token", Puid: "private-puid", Tags: &user.TUserTags{Biometrical: bio}, Data: &user.TUserData{UserInfo: &blackbox.TBlackBoxUserInfo{FirstName: name, LastName: "PrivateSurname"}}}
}

func identityRequest(options *request.TAliceRequestProto_TRequest_TAdditionalOptions) *request.TAliceRequestProto {
	return &request.TAliceRequestProto{Header: &request.TAliceRequestProto_THeader{RequestId: new("application-one")}, Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: new(common.EEventType_voice_input)}, AdditionalOptions: options}}
}

func TestTypedSpeakerIdentityRequiresUniqueFreshVoiceMatch(t *testing.T) {
	for _, tc := range []struct {
		name, status, confidence, personName string
		change                               func(*request.TAliceRequestProto_TRequest_TAdditionalOptions)
		exact                                bool
	}{
		{"guest_high", "matched", "high", "Алексей", func(*request.TAliceRequestProto_TRequest_TAdditionalOptions) {}, false},
		{"guest_low", "uncertain", "low", "", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.GuestUserOptions.MatchConfidence = new(guest.TGuestOptions_Low)
		}, false},
		{"guest_undefined", "matched", "undefined", "Алексей", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.GuestUserOptions.MatchConfidence = new(guest.TGuestOptions_Undefined)
		}, false},
		{"guest_no_confidence", "matched", "not_provided", "Алексей", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.GuestUserOptions.MatchConfidence = nil
		}, false},
		{"user_tag", "matched", "not_provided", "Алексей", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.GuestUserOptions = nil
			o.UsersOptions[0].Tags.Biometrical = &user.TUserTags_TBiometrical{Origin: user.TUserTags_TBiometrical_VoiceBiometry, Status: user.TUserTags_TBiometrical_Match, RefMessageId: "voice-one"}
		}, false},
		{"owner_alone", "unknown", "", "", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) { o.GuestUserOptions = nil }, false},
		{"remote_control", "unknown", "", "", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.GuestUserOptions.GuestOrigin = new(guest.TGuestOptions_RemoteControl)
		}, false},
		{"no_match", "no_match", "", "", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.GuestUserOptions.Status = new(guest.TGuestOptions_NoMatch)
		}, false},
		{"stale_ref", "unknown", "", "", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.GuestUserOptions.RefMessageId = new("old-voice")
		}, true},
		{"application_ref", "unknown", "", "", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.GuestUserOptions.RefMessageId = new("application-one")
		}, true},
		{"initial_missing_ref", "unknown", "", "", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) { o.GuestUserOptions.RefMessageId = nil }, false},
		{"exact_matched_user_missing_ref", "matched", "high", "Алексей", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) { o.GuestUserOptions.RefMessageId = nil }, true},
		{"different_matches", "ambiguous", "", "", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.UsersOptions = append(o.UsersOptions, identityProfile("other-person", "Другой", &user.TUserTags_TBiometrical{Origin: user.TUserTags_TBiometrical_VoiceBiometry, Status: user.TUserTags_TBiometrical_Match, RefMessageId: "voice-one"}))
		}, false},
		{"same_person_no_match", "ambiguous", "", "", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.UsersOptions[0].Tags.Biometrical = &user.TUserTags_TBiometrical{Origin: user.TUserTags_TBiometrical_VoiceBiometry, Status: user.TUserTags_TBiometrical_NoMatch, RefMessageId: "voice-one"}
		}, false},
		{"other_person_no_match", "matched", "high", "Алексей", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.UsersOptions = append(o.UsersOptions, identityProfile("other-person", "Другой", &user.TUserTags_TBiometrical{Origin: user.TUserTags_TBiometrical_VoiceBiometry, Status: user.TUserTags_TBiometrical_NoMatch, RefMessageId: "voice-one"}))
		}, false},
		{"conflicting_names", "matched", "high", "", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.UsersOptions = append(o.UsersOptions, identityProfile("private-pers", "Иное имя", nil))
		}, false},
		{"unsupported_confidence", "uncertain", "undefined", "", func(o *request.TAliceRequestProto_TRequest_TAdditionalOptions) {
			o.GuestUserOptions.MatchConfidence = new(guest.TGuestOptions_EMatchConfidence(99))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := identityOptions()
			tc.change(o)
			state := new(speakerState)
			got, _ := state.observe(identityRequest(o), "voice-one", tc.exact, "voice_input")
			if got.Status != tc.status || got.Confidence != tc.confidence || got.Name != tc.personName {
				t.Fatalf("metadata=%+v", got)
			}
			if tc.status == "matched" {
				want := fmt.Sprintf("%x", sha256.Sum256([]byte("quasar-bridge/voice-biometry/v1\x00private-pers")))
				if got.ID != want {
					t.Fatal("raw/wrong profile identifier")
				}
			} else if got.ID != "" || got.Name != "" {
				t.Fatal("unconfirmed identity exposed")
			}
			encoded, _ := json.Marshal(got)
			for _, secret := range []string{"private-pers", "private-token", "private-uid", "private-user-token", "private-puid", "PrivateSurname"} {
				if bytes.Contains(encoded, []byte(secret)) {
					t.Fatal("credential/raw profile/surname leaked")
				}
			}
		})
	}
}

func TestSpeakerNameFallsBackToSameProfileFullUserInfo(t *testing.T) {
	options := identityOptions()
	options.UsersOptions[0].Data.UserInfo = nil
	options.UsersOptions[0].Data.FullUserInfo = &fullinfo.TBlackBoxFullUserInfoProto{OptionalUserInfo: &fullinfo.TBlackBoxFullUserInfoProto_UserInfo{UserInfo: &fullinfo.TBlackBoxFullUserInfoProto_TUserInfo{FirstName: "Алексей", LastName: "PrivateSurname"}}}
	value, _ := new(speakerState).observe(identityRequest(options), "voice-one", true, "matched_user")
	if value.Status != "matched" || value.Name != "Алексей" {
		t.Fatal("typed FullUserInfo first name was ignored")
	}
}

func identitySync(profiles ...*user.TUserOptions) []byte {
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "sync-event"}, Event: &client.TClientMessage_TEvent_SynchronizeStateRequest{SynchronizeStateRequest: &synchronize.TSynchronizeStateRequest{UsersOptions: profiles}}}}})
}

func TestNativeSyncCatalogueSuppliesNameForExactEnrollmentHandle(t *testing.T) {
	const handle = catalogueHandle
	up := make(chan []byte, 8)
	n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
	defer n.Close()
	raw := identitySync(identityProfile(handle, "Алексей", nil))
	if err := n.Client(t.Context(), websocket.MessageBinary, raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(identityNext(t, up), raw) {
		t.Fatal("sync bytes changed")
	}
	options := identityOptions()
	options.GuestUserOptions.PersId = new(handle)
	options.UsersOptions = nil
	if err := n.Client(t.Context(), websocket.MessageBinary, identityVoice("voice-one", options)); err != nil {
		t.Fatal(err)
	}
	identityNext(t, up)
	n.mu.Lock()
	value := identity.FromContext(n.requests["voice-one"].ctx)
	n.mu.Unlock()
	if value.Status != "matched" || value.Name != "Алексей" {
		t.Fatal("exact enrolled profile was not named from sync")
	}
}

func TestNativeCatalogueRejectsWrongOpaqueIDAndConflictingNames(t *testing.T) {
	for _, mode := range []string{"bare_id", "conflicting_names", "direct_name"} {
		t.Run(mode, func(t *testing.T) {
			n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
			defer n.Close()
			id := catalogueHandle
			if mode == "bare_id" {
				id = "11111111-1111-1111-1111-111111111111"
			}
			if err := n.Client(t.Context(), websocket.MessageBinary, identitySync(catalogueProfile(id, "Каталог"))); err != nil {
				t.Fatal(err)
			}
			options := identityOptions()
			options.GuestUserOptions.PersId = new(catalogueHandle)
			options.UsersOptions = nil
			if mode == "direct_name" {
				options.UsersOptions = []*user.TUserOptions{identityProfile(catalogueHandle, "Текущее имя", nil)}
			}
			if err := n.Client(t.Context(), websocket.MessageBinary, identityVoice("voice-one", options)); err != nil {
				t.Fatal(err)
			}
			if mode == "conflicting_names" {
				if err := n.Client(t.Context(), websocket.MessageBinary, identitySync(catalogueProfile(id, "Иное имя"))); err != nil {
					t.Fatal(err)
				}
				if err := n.Client(t.Context(), websocket.MessageBinary, identitySync(catalogueProfile(id, "Каталог"))); err != nil {
					t.Fatal(err)
				}
			}
			n.mu.Lock()
			value := identity.FromContext(n.requests["voice-one"].ctx)
			n.mu.Unlock()
			wantName := ""
			if mode == "direct_name" {
				wantName = "Текущее имя"
			}
			if value.Status != "matched" || value.ID != speakerID(catalogueHandle) || value.Name != wantName {
				t.Fatal("catalogue guessed another identifier/name or replaced direct profile data")
			}
		})
	}
}

func TestSpeakerCatalogueIsBoundedMergeOnlyAndTextRemainsUnidentified(t *testing.T) {
	n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
	defer n.Close()
	text := wire.Decode(identityVoice("text-one", identityOptions()), wire.ClientToServer).Client
	req := text.GetEvent().GetVoiceInput()
	req.Request.Event.Text = new("question")
	text.GetEvent().Event = &client.TClientMessage_TEvent_TextInput{TextInput: req}
	if err := n.Client(t.Context(), websocket.MessageBinary, aa(text)); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	count := len(n.speakerCatalog)
	n.mu.Unlock()
	if count != 1 || identity.FromContext(n.requests["text-one"].ctx) != (identity.Snapshot{}) {
		t.Fatal("TextInput must provide catalogue data without choosing its speaker")
	}
	profiles := make([]*user.TUserOptions, 0, 66)
	for i := range 65 {
		profiles = append(profiles, catalogueProfile(fmt.Sprintf("profile-%d", i), "Имя"))
	}
	if err := n.Client(t.Context(), websocket.MessageBinary, identitySync(profiles...)); err != nil {
		t.Fatal(err)
	}
	if err := n.Client(t.Context(), websocket.MessageBinary, identitySync()); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	count = len(n.speakerCatalog)
	first := n.speakerCatalog[speakerID("profile-0")]
	n.mu.Unlock()
	if count != 64 || first.name != "Имя" {
		t.Fatal("catalogue exceeded bounds or treated absent profiles as revocation")
	}
	if err := n.Client(t.Context(), websocket.MessageBinary, identitySync(catalogueProfile("profile-0", "Другое имя"))); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	first = n.speakerCatalog[speakerID("profile-0")]
	n.mu.Unlock()
	if first.name != "" || !first.conflict {
		t.Fatal("duplicate profile names were not marked ambiguous")
	}
}

func TestSpeakerCatalogueLogsHashedNamedProfilesOnly(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
	defer n.Close()
	if err := n.Client(t.Context(), websocket.MessageBinary, identitySync(catalogueProfile(catalogueHandle, "Алексей"))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"profile_count":1`) || !strings.Contains(logs.String(), `"name_count":1`) || !strings.Contains(logs.String(), speakerID(catalogueHandle)) {
		t.Fatal("named catalogue hashes/counts unavailable")
	}
	for _, secret := range []string{catalogueHandle, "Алексей", "private-user-token", "private-puid", "PrivateSurname"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("raw profile catalogue leaked into operational logs")
		}
	}
}

func TestSpeakerEvidenceSurvivesMetadataOnlyAndDetectsLaterConflict(t *testing.T) {
	s := new(speakerState)
	first, _ := s.observe(identityRequest(identityOptions()), "voice-one", false, "voice_input")
	metadata := identityRequest(nil)
	metadata.Request.Event.BiometryScoring = &common.TBiometryScoring{Scores: []*common.TBiometryScoring_TScore{{UserId: new("untrusted-candidate"), Score: new(float64(999))}}}
	metadata.Request.Event.BiometryClassification = new(common.TBiometryClassification)
	got, _ := s.observe(metadata, "voice-one", true, "matched_user")
	if got.ID != first.ID || got.Name != first.Name || !got.Scoring || !got.Classification {
		t.Fatal("metadata-only event erased typed match")
	}
	o := identityOptions()
	o.GuestUserOptions.Status = new(guest.TGuestOptions_NoMatch)
	got, _ = s.observe(identityRequest(o), "voice-one", true, "matched_user")
	if got.Status != "ambiguous" || got.ID != "" || got.Name != "" {
		t.Fatal("cross-frame contradiction ignored")
	}
	s = new(speakerState)
	got, _ = s.observe(metadata, "voice-one", true, "matched_user")
	if got.Status != "unknown" || got.ID != "" || got.Name != "" || got.CandidateCount != 1 {
		t.Fatal("score became an identity")
	}
}

func TestLowSpeakerMatchExposesOnlyCandidateAndClearsOnConflict(t *testing.T) {
	for _, conflict := range []string{"same_person_no_match", "unscoped_no_match", "different_match"} {
		t.Run(conflict, func(t *testing.T) {
			options := identityOptions()
			options.GuestUserOptions.MatchConfidence = new(guest.TGuestOptions_Low)
			state := new(speakerState)
			value, _ := state.observe(identityRequest(options), "voice-one", true, "matched_user")
			want := fmt.Sprintf("%x", sha256.Sum256([]byte("quasar-bridge/voice-biometry/v1\x00private-pers")))
			if value.Status != "uncertain" || value.Confidence != "low" || value.ID != "" || value.Name != "" || value.CandidateID != want || value.CandidateName != "Алексей" {
				t.Fatal("low match did not remain a candidate")
			}
			negative := identityOptions()
			switch conflict {
			case "same_person_no_match":
				negative.GuestUserOptions.Status = new(guest.TGuestOptions_NoMatch)
			case "unscoped_no_match":
				negative.GuestUserOptions.Status = new(guest.TGuestOptions_NoMatch)
				negative.GuestUserOptions.PersId = nil
			case "different_match":
				negative.GuestUserOptions.PersId = new("another-person")
			}
			value, _ = state.observe(identityRequest(negative), "voice-one", true, "matched_user")
			if value.Status != "ambiguous" || value.ID != "" || value.Name != "" || value.CandidateID != "" || value.CandidateName != "" {
				t.Fatal("conflicting evidence retained candidate identity")
			}
		})
	}
}

func TestSpeakerCandidateDoesNotBorrowOwnerName(t *testing.T) {
	options := identityOptions()
	options.GuestUserOptions.MatchConfidence = new(guest.TGuestOptions_Low)
	options.GuestUserOptions.PersId = new("person-without-name")
	value, _ := new(speakerState).observe(identityRequest(options), "voice-one", true, "matched_user")
	if value.Status != "uncertain" || value.CandidateID == "" || value.CandidateName != "" || value.ID != "" || value.Name != "" {
		t.Fatal("candidate borrowed the device owner's profile name")
	}
	for _, status := range []guest.TGuestOptions_EStatus{guest.TGuestOptions_NoMatch, guest.TGuestOptions_Match} {
		options = identityOptions()
		options.GuestUserOptions.Status = new(status)
		value, _ = new(speakerState).observe(identityRequest(options), "voice-one", false, "voice_input")
		if value.CandidateID != "" || value.CandidateName != "" {
			t.Fatal("non-uncertain result duplicated candidate fields")
		}
	}
}

func TestEmptyPersonNoMatchConflictsWithCurrentMatchInEitherOrder(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, ref := range []string{"voice-one", ""} {
			match := identityRequest(identityOptions())
			options := identityOptions()
			options.GuestUserOptions.PersId = nil
			options.GuestUserOptions.Status = new(guest.TGuestOptions_NoMatch)
			options.GuestUserOptions.RefMessageId = new(ref)
			negative := identityRequest(options)
			observations := []*request.TAliceRequestProto{match, negative}
			if reverse {
				observations[0], observations[1] = observations[1], observations[0]
			}
			state := new(speakerState)
			for _, req := range observations {
				state.observe(req, "voice-one", true, "matched_user")
			}
			if state.current.Status != "ambiguous" || state.current.ID != "" || state.current.Name != "" {
				t.Fatal("global NoMatch left a person visible")
			}
		}
	}
}

func TestSpeakerSourceTracksTypedEvidenceInsteadOfLaterMetadata(t *testing.T) {
	state := new(speakerState)
	state.observe(identityRequest(identityOptions()), "voice-one", false, "voice_input")
	metadata := identityRequest(nil)
	metadata.Request.Event.BiometryClassification = new(common.TBiometryClassification)
	value, _ := state.observe(metadata, "voice-one", true, "matched_user")
	if value.Source != "voice_input" || value.Status != "matched" {
		t.Fatal("metadata-only frame claimed a typed match")
	}
	unrelated := identityRequest(&request.TAliceRequestProto_TRequest_TAdditionalOptions{UsersOptions: []*user.TUserOptions{identityProfile("other-person", "Другое имя", &user.TUserTags_TBiometrical{Origin: user.TUserTags_TBiometrical_VoiceBiometry, Status: user.TUserTags_TBiometrical_NoMatch, RefMessageId: "voice-one"})}})
	value, _ = state.observe(unrelated, "voice-one", true, "matched_user")
	if value.Source != "voice_input" || value.Status != "matched" {
		t.Fatal("another person's NoMatch claimed this match's provenance")
	}
	value, _ = state.observe(identityRequest(identityOptions()), "voice-one", true, "matched_user")
	if value.Source != "matched_user" {
		t.Fatal("fresh typed match provenance not updated")
	}
}

func TestSpeakerNameEnrichmentRetainsMatchSource(t *testing.T) {
	state := new(speakerState)
	options := identityOptions()
	options.UsersOptions = nil
	state.observe(identityRequest(options), "voice-one", false, "voice_input")
	nameOnly := identityRequest(&request.TAliceRequestProto_TRequest_TAdditionalOptions{UsersOptions: []*user.TUserOptions{identityProfile("private-pers", "Алексей", nil)}})
	value, _ := state.observe(nameOnly, "voice-one", true, "matched_user")
	if value.Source != "voice_input" || value.Name != "Алексей" || value.Status != "matched" {
		t.Fatal("profile enrichment changed identity provenance")
	}
}

func identityVoice(id string, options *request.TAliceRequestProto_TRequest_TAdditionalOptions) []byte {
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: identityRequest(options)}}}})
}

func identityMatched(voiceID, ref string, options *request.TAliceRequestProto_TRequest_TAdditionalOptions) []byte {
	req := identityRequest(options)
	req.VoiceInputMessageId = new(voiceID)
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "metadata-event", RefMessageId: ref}, Event: &client.TClientMessage_TEvent_MatchedUser{MatchedUser: req}}}})
}

func identityNext(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case data := <-ch:
		return data
	case <-time.After(time.Second):
		t.Fatal("missing native output")
		return nil
	}
}

func TestNativeIdentityMetadataPreservesFramesAndASRPath(t *testing.T) {
	up := make(chan []byte, 16)
	calls := make(chan identity.Snapshot, 4)
	events := make(chan identity.Snapshot, 8)
	n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, resolverFunc(func(ctx context.Context, _ string) (Resolution, error) {
		calls <- identity.FromContext(ctx)
		return Resolution{Route: "yandex"}, nil
	}))
	defer n.Close()
	n.OnSpeaker = func(_ string, s identity.Snapshot) { events <- s }
	voice := identityVoice("voice-one", nil)
	if err := n.Client(t.Context(), websocket.MessageBinary, voice); err != nil {
		t.Fatal(err)
	}
	want, _ := wire.EnableApproval(wire.Decode(voice, wire.ClientToServer))
	got := identityNext(t, up)
	if !bytes.Equal(got, want) || wire.Decode(got, wire.ClientToServer).Kind() != "VoiceInput" {
		t.Fatal("voice ASR method/flags changed")
	}
	o := identityOptions()
	o.GuestUserOptions.RefMessageId = nil
	raw := identityMatched("voice-one", "", o)
	raw = protowire.AppendString(protowire.AppendTag(raw, 777, protowire.BytesType), "unknown-wire-field")
	if err := n.Client(t.Context(), websocket.MessageBinary, raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(identityNext(t, up), raw) {
		t.Fatal("MatchedUser raw bytes changed")
	}
	if (<-events).Name != "Алексей" {
		t.Fatal("fresh typed speaker unavailable")
	}
	select {
	case <-calls:
		t.Fatal("metadata triggered resolver")
	default:
	}
	final := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "voice-one"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("question")}}}}}}})
	if err := n.Server(t.Context(), websocket.MessageBinary, final); err != nil {
		t.Fatal(err)
	}
	if (<-calls).Name != "Алексей" {
		t.Fatal("identity context lost before resolver")
	}
	identityNext(t, up)
	o.GuestUserOptions.MatchConfidence = new(guest.TGuestOptions_Low)
	late := identityMatched("voice-one", "", o)
	if err := n.Client(t.Context(), websocket.MessageBinary, late); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(identityNext(t, up), late) || (<-events).Status != "uncertain" {
		t.Fatal("late observation lost")
	}
	select {
	case <-calls:
		t.Fatal("late metadata retriggered resolver")
	default:
	}
}

func TestNativeIdentityLogsOnlyMetadata(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
	defer n.Close()
	if err := n.Client(t.Context(), websocket.MessageBinary, identityVoice("voice-one", identityOptions())); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "native_speaker_identity") || !strings.Contains(logs.String(), `"matched":true`) {
		t.Fatal("missing operational metadata")
	}
	for _, secret := range []string{"private-pers", "private-token", "private-uid", "private-user-token", "private-puid", "PrivateSurname", "Алексей"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("sensitive speaker data in operational logs")
		}
	}
}

func TestNativeCandidateLogsOnlyAvailability(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	value := identity.Snapshot{Status: "uncertain", Confidence: "low", CandidateID: strings.Repeat("d", 64), CandidateName: "PrivateCandidateName"}
	new(Native).publishSpeaker("request", value)
	if !strings.Contains(logs.String(), `"candidate_present":true`) || !strings.Contains(logs.String(), `"candidate_name_available":true`) {
		t.Fatal("candidate availability not observable")
	}
	if strings.Contains(logs.String(), value.CandidateID) || strings.Contains(logs.String(), value.CandidateName) {
		t.Fatal("candidate values leaked into operational logs")
	}
}

func TestNativeSpeakerCorrelationRejectsStaleConflictingAndTextOwner(t *testing.T) {
	for _, mode := range []string{"stale14", "conflicting14", "application_only", "header_missing_inner_ref", "text_owner", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			up := make(chan []byte, 16)
			n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
			defer n.Close()
			voice := identityVoice("voice-one", nil)
			if mode == "text_owner" {
				msg := wire.Decode(voice, wire.ClientToServer).Client
				req := identityRequest(identityOptions())
				req.Request.Event.Text = new("question")
				msg.GetEvent().Event = &client.TClientMessage_TEvent_TextInput{TextInput: req}
				voice = aa(msg)
			}
			if err := n.Client(t.Context(), websocket.MessageBinary, voice); err != nil {
				t.Fatal(err)
			}
			identityNext(t, up)
			if mode == "conflicting14" {
				if err := n.Client(t.Context(), websocket.MessageBinary, identityVoice("voice-two", nil)); err != nil {
					t.Fatal(err)
				}
				identityNext(t, up)
			}
			updates := 0
			n.OnSpeaker = func(string, identity.Snapshot) { updates++ }
			o := identityOptions()
			voiceID, ref := "voice-one", ""
			switch mode {
			case "stale14":
				voiceID, ref = "expired-voice", "voice-one"
			case "conflicting14":
				ref = "voice-two"
			case "application_only":
				voiceID, ref = "", "application-one"
			case "header_missing_inner_ref":
				voiceID, ref = "", "voice-one"
				o.GuestUserOptions.RefMessageId = nil
			case "canceled":
				n.mu.Lock()
				n.requests["voice-one"].canceled = true
				n.requests["voice-one"].cancel()
				n.mu.Unlock()
			}
			raw := identityMatched(voiceID, ref, o)
			if err := n.Client(t.Context(), websocket.MessageBinary, raw); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(identityNext(t, up), raw) {
				t.Fatal("rejected identity changed original wire frame")
			}
			n.mu.Lock()
			snapshot := identity.FromContext(n.requests["voice-one"].ctx)
			n.mu.Unlock()
			if snapshot.ID != "" || snapshot.Name != "" {
				t.Fatal("stale/conflicting/nonvoice metadata became a person")
			}
			if mode != "header_missing_inner_ref" && updates != 0 {
				t.Fatal("rejected correlation emitted identity callback")
			}
		})
	}
}

func TestNativeSpeakerContextReachesActualEngineAndLLM(t *testing.T) {
	requests := make(chan []llm.Message, 2)
	engine := &Engine{LLM: completionFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
		if structured {
			t.Error("explicit model request unexpectedly classified")
		}
		requests <- messages
		return "ответ", nil
	})}
	n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, engine)
	defer n.Close()
	if err := n.Client(t.Context(), websocket.MessageBinary, identityVoice("voice-one", identityOptions())); err != nil {
		t.Fatal(err)
	}
	final := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "voice-one"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("спроси нейросеть: как меня зовут")}}}}}}})
	if err := n.Server(t.Context(), websocket.MessageBinary, final); err != nil {
		t.Fatal(err)
	}
	select {
	case messages := <-requests:
		_, encoded, ok := strings.Cut(messages[0].Content, "speaker_recognition=")
		var value identity.Snapshot
		if !ok || json.Unmarshal([]byte(encoded), &value) != nil || value.Status != "matched" || value.Name != "Алексей" || len(value.ID) != 64 {
			t.Fatal("SDK identity did not reach actual Engine/LLM")
		}
		if strings.Contains(encoded, "private-pers") || strings.Contains(encoded, "private-token") {
			t.Fatal("raw SDK identity/credential reached LLM")
		}
	case <-time.After(time.Second):
		t.Fatal("LLM not invoked")
	}
}
