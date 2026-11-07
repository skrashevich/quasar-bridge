package journal

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestSpeakerMetadataIsOptionalAndStructured(t *testing.T) {
	plain, err := json.Marshal(Event{Event: "request"})
	if err != nil || strings.Contains(string(plain), "speaker") {
		t.Fatalf("existing event changed: %s %v", plain, err)
	}
	event := Event{Event: "speaker_recognition", Request: "request-hash", Speaker: &Speaker{
		Status: "matched", ID: "profile-hash", Source: "user_biometrical", Confidence: "not_provided", NameAvailable: true,
		Scoring: true, CandidateCount: 2,
	}}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Event
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Speaker == nil || *decoded.Speaker != *event.Speaker || decoded.Request != "request-hash" {
		t.Fatalf("speaker evidence lost: %s", raw)
	}
}
