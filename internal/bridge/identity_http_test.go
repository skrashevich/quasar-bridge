package bridge

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quasarbridge/internal/identity"
	"quasarbridge/internal/llm"
)

func TestSpeakerNameReachesLLMHTTPBody(t *testing.T) {
	for _, snapshot := range []identity.Snapshot{
		{Status: "matched", Name: "FixtureName", Confidence: "high"},
		{Status: "uncertain", CandidateName: "FixtureCandidate", Confidence: "low"},
	} {
		t.Run(snapshot.Status, func(t *testing.T) {
			captured := make(chan []llm.Message, 1)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []llm.Message `json:"messages"`
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || json.UnmarshalRead(r.Body, &body) != nil {
					http.Error(w, "invalid completion request", http.StatusBadRequest)
					return
				}
				captured <- body.Messages
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fixture answer"}}]}`))
			}))
			t.Cleanup(endpoint.Close)
			e := &Engine{LLM: &llm.Client{BaseURL: endpoint.URL + "/v1", Model: "fixture-model"}}
			ctx := identity.WithState(t.Context())
			identity.Update(ctx, snapshot)
			if _, err := e.Fallback(ctx, "Как меня зовут?"); err != nil {
				t.Fatal(err)
			}
			messages := <-captured
			if len(messages) != 2 || messages[0].Role != "system" || messages[1].Content != "Как меня зовут?" {
				t.Fatalf("unexpected HTTP messages: %+v", messages)
			}
			_, encoded, ok := strings.Cut(messages[0].Content, "speaker_recognition=")
			var got identity.Snapshot
			if !ok || json.Unmarshal([]byte(encoded), &got) != nil || got != snapshot {
				t.Fatal("speaker name or uncertainty did not reach actual HTTP request")
			}
		})
	}
}
