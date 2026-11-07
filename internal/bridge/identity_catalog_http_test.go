package bridge

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	fullinfo "github.com/skrashevich/quasar-go-genproto/proto/alice/library/blackbox/proto"
	user "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/user"
	asr "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"quasarbridge/internal/identity"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

const catalogueHandle = "PersId-11111111-1111-1111-1111-111111111111#OriginDeviceId-device-one001"

func catalogueProfile(id, name string) *user.TUserOptions {
	profile := identityProfile(id, "", nil)
	profile.Data.UserInfo = nil
	profile.Data.FullUserInfo = &fullinfo.TBlackBoxFullUserInfoProto{OptionalUserInfo: &fullinfo.TBlackBoxFullUserInfoProto_UserInfo{UserInfo: &fullinfo.TBlackBoxFullUserInfoProto_TUserInfo{FirstName: name, LastName: "PrivateSurname"}}}
	return profile
}

func catalogueASRFinal() []byte {
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "voice-one"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("спроси нейросеть: как меня зовут")}}}}}}})
}

func TestNativeCatalogueNameReachesActualLLMHTTP(t *testing.T) {
	if len(catalogueHandle) != 72 {
		t.Fatal("enrollment fixture must retain full opaque 72-byte handle")
	}
	for _, source := range []string{"voice_input", "synchronize_state", "matched_user", "server_directive", "late_automatic_lookup"} {
		t.Run(source, func(t *testing.T) {
			requests := make(chan []llm.Message, 2)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []llm.Message `json:"messages"`
				}
				raw, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Error(err)
				}
				requests <- body.Messages
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ответ"}}]}`)
			}))
			defer endpoint.Close()
			engine := &Engine{LLM: &llm.Client{BaseURL: endpoint.URL, Model: "fixture", HTTP: endpoint.Client()}}
			up := make(chan []byte, 16)
			n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, engine)
			defer n.Close()
			var warmed bool
			if source == "late_automatic_lookup" {
				n.ResolveSpeakerName = func(id string) string {
					if warmed && id == speakerID(catalogueHandle) {
						return "Алексей"
					}
					return ""
				}
			}
			if source == "server_directive" {
				if err := n.Server(t.Context(), websocket.MessageBinary, identityDirectiveFrame("push", identityAddPerson(t, catalogueHandle, "Алексей", "raw"))); err != nil {
					t.Fatal(err)
				}
			}
			profile := catalogueProfile(catalogueHandle, "Алексей")
			if source == "synchronize_state" {
				raw := identitySync(profile)
				if err := n.Client(t.Context(), websocket.MessageBinary, raw); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(identityNext(t, up), raw) {
					t.Fatal("sync frame changed")
				}
			}
			initial := identityOptions()
			initial.GuestUserOptions = nil
			initial.UsersOptions = nil
			if source == "voice_input" {
				initial.UsersOptions = []*user.TUserOptions{profile}
			}
			voice := identityVoice("voice-one", initial)
			if err := n.Client(t.Context(), websocket.MessageBinary, voice); err != nil {
				t.Fatal(err)
			}
			want, _ := wire.EnableApproval(wire.Decode(voice, wire.ClientToServer))
			if !bytes.Equal(identityNext(t, up), want) {
				t.Fatal("original ASR request changed")
			}
			n.mu.Lock()
			before := identity.FromContext(n.requests["voice-one"].ctx)
			n.mu.Unlock()
			if before.ID != "" || before.Name != "" {
				t.Fatal("profile catalogue selected the speaker")
			}
			matched := identityOptions()
			matched.GuestUserOptions.PersId = new(catalogueHandle)
			matched.GuestUserOptions.RefMessageId = nil
			matched.UsersOptions = nil
			if source == "matched_user" {
				matched.UsersOptions = []*user.TUserOptions{profile}
			}
			raw := identityMatched("voice-one", "", matched)
			if err := n.Client(t.Context(), websocket.MessageBinary, raw); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(identityNext(t, up), raw) {
				t.Fatal("compact matched-user frame changed")
			}
			warmed = true
			if err := n.Server(t.Context(), websocket.MessageBinary, catalogueASRFinal()); err != nil {
				t.Fatal(err)
			}
			select {
			case messages := <-requests:
				_, encoded, ok := strings.Cut(messages[0].Content, "speaker_recognition=")
				var value identity.Snapshot
				if !ok || json.Unmarshal([]byte(encoded), &value) != nil || value.Name != "Алексей" || value.ID != speakerID(catalogueHandle) || value.Status != "matched" {
					t.Fatal("catalogue firstname missing from actual outbound HTTP messages")
				}
				for _, secret := range []string{catalogueHandle, "private-token", "private-user-token", "PrivateSurname"} {
					if strings.Contains(encoded, secret) {
						t.Fatal("raw SDK profile data reached LLM")
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no LLM HTTP request")
			}
		})
	}
}

func TestLateSyncEnrichesContextWithoutRewritingInflightLLMHTTP(t *testing.T) {
	requests := make(chan []llm.Message, 2)
	release := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []llm.Message `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		requests <- body.Messages
		<-release
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ответ"}}]}`)
	}))
	defer endpoint.Close()
	engine := &Engine{LLM: &llm.Client{BaseURL: endpoint.URL, Model: "fixture", HTTP: endpoint.Client()}}
	up := make(chan []byte, 8)
	n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, engine)
	defer func() { close(release); n.Close() }()
	options := identityOptions()
	options.GuestUserOptions.PersId = new(catalogueHandle)
	options.UsersOptions = nil
	if err := n.Client(t.Context(), websocket.MessageBinary, identityVoice("voice-one", options)); err != nil {
		t.Fatal(err)
	}
	identityNext(t, up)
	if err := n.Server(t.Context(), websocket.MessageBinary, catalogueASRFinal()); err != nil {
		t.Fatal(err)
	}
	var sent []llm.Message
	select {
	case sent = <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("no LLM request")
	}
	raw := identitySync(catalogueProfile(catalogueHandle, "Алексей"))
	if err := n.Client(t.Context(), websocket.MessageBinary, raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(identityNext(t, up), raw) {
		t.Fatal("late sync frame changed")
	}
	n.mu.Lock()
	value := identity.FromContext(n.requests["voice-one"].ctx)
	n.mu.Unlock()
	if value.Name != "Алексей" || value.Source != "voice_input" {
		t.Fatal("late profile did not enrich matched context/provenance")
	}
	if strings.Contains(sent[0].Content, "Алексей") {
		t.Fatal("already-sent HTTP messages changed")
	}
	select {
	case <-requests:
		t.Fatal("late sync retried LLM")
	default:
	}
}
