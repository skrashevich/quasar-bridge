package bridge

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"strings"
	"testing"

	"github.com/coder/websocket"
	guest "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/guest"
	user "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/user"
	"quasarbridge/internal/identity"
)

func TestAutomaticLookupNamesEnrichOnlySelectedSDKPerson(t *testing.T) {
	for _, mode := range []string{"matched", "low", "wrong_id", "unknown", "no_match", "ambiguous", "direct_name", "catalogue_name"} {
		t.Run(mode, func(t *testing.T) {
			n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
			defer n.Close()
			id := speakerID("private-pers")
			var lookups []string
			n.ResolveSpeakerName = func(selected string) string {
				lookups = append(lookups, selected)
				if selected == id && mode != "wrong_id" {
					return "  AutomaticallyNamedPerson  "
				}
				return ""
			}
			if mode == "catalogue_name" {
				if err := n.Client(t.Context(), websocket.MessageBinary, identitySync(identityProfile("private-pers", "CataloguePerson", nil))); err != nil {
					t.Fatal(err)
				}
			}
			options := identityOptions()
			if mode != "direct_name" {
				options.UsersOptions = nil
			}
			switch mode {
			case "low":
				options.GuestUserOptions.MatchConfidence = new(guest.TGuestOptions_Low)
			case "unknown":
				options.GuestUserOptions = nil
			case "no_match":
				options.GuestUserOptions.Status = new(guest.TGuestOptions_NoMatch)
			case "ambiguous":
				options.UsersOptions = []*user.TUserOptions{identityProfile("another-person", "Another", &user.TUserTags_TBiometrical{Origin: user.TUserTags_TBiometrical_VoiceBiometry, Status: user.TUserTags_TBiometrical_Match, RefMessageId: "voice-one"})}
			}
			if err := n.Client(t.Context(), websocket.MessageBinary, identityVoice("voice-one", options)); err != nil {
				t.Fatal(err)
			}
			n.mu.Lock()
			value := identity.FromContext(n.requests["voice-one"].ctx)
			n.mu.Unlock()
			switch mode {
			case "matched":
				if value.Name != "AutomaticallyNamedPerson" || value.ID != id || value.Status != "matched" || value.Confidence != "high" {
					t.Fatal("automatic lookup changed SDK match")
				}
			case "low":
				if value.CandidateName != "AutomaticallyNamedPerson" || value.CandidateID != id || value.ID != "" || value.Name != "" || value.Status != "uncertain" || value.Confidence != "low" {
					t.Fatal("fallback promoted SDK hypothesis")
				}
			case "direct_name":
				if value.Name != "Алексей" {
					t.Fatal("lookup replaced direct profile name")
				}
			case "catalogue_name":
				if value.Name != "CataloguePerson" {
					t.Fatal("lookup replaced catalogue name")
				}
			default:
				if value.Name != "" || value.CandidateName != "" {
					t.Fatal("lookup selected wrong/unrecognized person")
				}
			}
			wantCalls := 0
			if mode == "matched" || mode == "low" || mode == "wrong_id" {
				wantCalls = 1
			}
			if len(lookups) != wantCalls || (len(lookups) > 0 && lookups[0] != id) {
				t.Fatal("lookup must receive only the exact SDK-selected hash")
			}
		})
	}
}

func TestAutomaticSpeakerNameStaysOutOfOperationalLogs(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	n := NewNative(t.Context(), func(context.Context, websocket.MessageType, []byte) error { return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, nil)
	defer n.Close()
	n.ResolveSpeakerName = func(string) string { return "PrivateAutomaticName" }
	options := identityOptions()
	options.UsersOptions = nil
	if err := n.Client(t.Context(), websocket.MessageBinary, identityVoice("voice-one", options)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "PrivateAutomaticName") || strings.Contains(logs.String(), "private-pers") {
		t.Fatal("automatic name/raw ID reached operational logs")
	}
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatal(err)
		}
		if value["msg"] == "native_speaker_identity" && value["name_available"] != true {
			t.Fatal("automatic name availability not observable")
		}
	}
}
