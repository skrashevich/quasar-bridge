package bridge

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"quasarbridge/internal/identity"
	"quasarbridge/internal/journal"
)

func TestConversationSpeakerCorrelatesWithoutProfileName(t *testing.T) {
	w := new(journal.Writer)
	t.Cleanup(func() { _ = w.Close() })
	cfg := journal.DefaultConfig()
	cfg.Enabled = true
	cfg.Path = filepath.Join(t.TempDir(), "turns.jsonl")
	if err := w.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	log := newConversation(w)
	input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: "private-original-id"},
		Event:  &client.TClientMessage_TEvent_VoiceInput{VoiceInput: new(request.TAliceRequestProto)},
	}}}
	log.client(aa(input))
	log.route("private-original-id", "llm")
	log.speaker("private-original-id", identity.Snapshot{
		Status: "matched", ID: strings.Repeat("a", 64), Name: "private-profile-name",
		Source: "matched_user", Confidence: "high", Scoring: true, CandidateCount: 2,
	})
	log.speaker("private-original-id", identity.Snapshot{
		Status: "uncertain", CandidateID: strings.Repeat("b", 64), CandidateName: "private-candidate-name",
		Source: "matched_user", Confidence: "low", CandidateCount: 1,
	})
	log.speaker("private-original-id", identity.Snapshot{Status: "ambiguous", Source: "matched_user"})
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-") {
		t.Fatal("profile name or raw request ID leaked into journal")
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 5 {
		t.Fatalf("events=%d", len(lines))
	}
	var e journal.Event
	if err := json.Unmarshal([]byte(lines[2]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Event != "speaker_recognition" || e.Request != journalID("private-original-id") || e.Route != "llm" || e.Session == "" {
		t.Fatalf("speaker event lost request correlation: %+v", e)
	}
	if e.Speaker == nil || e.Speaker.Status != "matched" || !e.Speaker.NameAvailable || e.Speaker.CandidateCount != 2 || e.Speaker.ID != strings.Repeat("a", 64) {
		t.Fatalf("speaker evidence missing: %+v", e.Speaker)
	}
	e = journal.Event{}
	if err := json.Unmarshal([]byte(lines[3]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Speaker.Status != "uncertain" || e.Speaker.ID != "" || e.Speaker.NameAvailable || e.Speaker.CandidateID != strings.Repeat("b", 64) || !e.Speaker.CandidateNameAvailable {
		t.Fatalf("candidate presented as confirmed identity: %+v", e.Speaker)
	}
	e = journal.Event{}
	if err := json.Unmarshal([]byte(lines[4]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Speaker.ID != "" || e.Speaker.NameAvailable || e.Speaker.CandidateID != "" || e.Speaker.CandidateNameAvailable {
		t.Fatalf("conflicting identity was retained: %+v", e.Speaker)
	}
}
