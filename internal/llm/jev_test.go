package llm

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestJevDecisionContract(t *testing.T) {
	for _, choice := range []string{"llm", "yandex", "home", "invalid"} {
		t.Run(choice, func(t *testing.T) {
			j := &Jev{BaseURL: "https://openrouter.ai/api", Model: "~typesafe/jev-latest", APIKey: "router-only", HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://openrouter.ai/api/alpha/decisions" || r.Header.Get("Authorization") != "Bearer router-only" {
					t.Error("wrong endpoint or key")
				}
				var request struct {
					Model string `json:"model"`
					State struct {
						Conversation []Message `json:"conversation"`
					} `json:"state"`
					Questions map[string]struct {
						Type         string            `json:"type"`
						Instructions string            `json:"instructions"`
						Criteria     map[string]string `json:"criteria"`
					} `json:"questions"`
				}
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Fatal(err)
				}
				if request.Model != "~typesafe/jev-latest" || request.Questions["route"].Instructions != "policy" || request.Questions["route"].Type != "choice" || len(request.Questions["route"].Criteria) != 3 {
					t.Error("lost decision policy")
				}
				if len(request.State.Conversation) != 2 || request.State.Conversation[1].Content != "full question" {
					t.Error("lost dialogue context")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"answers":{"route":{"type":"choice","choice":"` + choice + `"}}}`))}, nil
			})}}
			got, err := j.Complete(t.Context(), []Message{{Role: "system", Content: "policy"}, {Role: "assistant", Content: "prior"}, {Role: "user", Content: "full question"}}, true)
			if choice == "invalid" {
				if err == nil {
					t.Fatal("accepted invalid route")
				}
				return
			}
			if err != nil || !strings.Contains(got, choice) {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}
