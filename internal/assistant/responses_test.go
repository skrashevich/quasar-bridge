package assistant

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/llm"
)

func TestResponsesAgentDisablesHostedToolsAfterHistoryAndAtBudget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		function  string
		maxRounds int
		maxCalls  int
	}{
		{"private_history", "search_history", 3, 3},
		{"last_round", "current_time", 1, 3},
		{"call_budget", "current_time", 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != "/v1/responses" {
					t.Errorf("endpoint = %q", r.URL.Path)
				}
				var request struct {
					Input []struct {
						Type             string `json:"type"`
						CallID           string `json:"call_id"`
						Output           string `json:"output"`
						EncryptedContent string `json:"encrypted_content"`
					} `json:"input"`
					Tools []struct {
						Type string `json:"type"`
						Name string `json:"name"`
					} `json:"tools"`
				}
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					http.Error(w, "bad JSON", http.StatusBadRequest)
					return
				}
				hosted := 0
				for _, tool := range request.Tools {
					if tool.Type == "web_search" || tool.Type == "code_interpreter" {
						hosted++
					}
					if requests == 2 && tc.function == "search_history" && tool.Name == "read_webpage" {
						t.Error("web function offered after private history")
					}
				}
				if requests == 1 {
					if hosted != 2 {
						t.Errorf("initial hosted tools = %d, want 2", hosted)
					}
					fmt.Fprintf(w, `{"status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"opaque"},{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":%q,"arguments":"{}"}]}`, tc.function)
					return
				}
				if hosted != 0 {
					t.Errorf("hosted tools bypassed policy: %d tools", hosted)
				}
				if tc.function != "search_history" && len(request.Tools) != 0 {
					t.Error("tools offered after tool budget")
				}
				n := len(request.Input)
				if n < 3 || request.Input[n-3].EncryptedContent != "opaque" || request.Input[n-2].Type != "function_call" || request.Input[n-2].CallID != "call_1" || request.Input[n-1].Type != "function_call_output" || request.Input[n-1].CallID != "call_1" || !strings.Contains(request.Input[n-1].Output, "private history") {
					t.Errorf("lost reasoning or correlated tool result: %+v", request.Input)
				}
				fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Ответ получен"}]}]}`)
			}))
			defer server.Close()
			tools := &privacyTools{}
			client := &llm.Client{API: "responses", BaseURL: server.URL + "/v1", Model: "fixture", HostedTools: []string{"web_search", "code_interpreter"}}
			agent := Agent{Client: client, Tools: tools, MaxRounds: tc.maxRounds, MaxCalls: tc.maxCalls}
			answer, private, err := agent.CompleteDialogue(t.Context(), []llm.Message{{Role: "user", Content: "вопрос"}}, false)
			if err != nil || answer != "Ответ получен" || requests != 2 || private != (tc.function == "search_history") {
				t.Fatalf("answer=%q private=%v requests=%d error=%v", answer, private, requests, err)
			}
			if len(tools.calls) != 1 || tools.calls[0] != tc.function {
				t.Fatalf("executed tools = %v", tools.calls)
			}
		})
	}
}

func TestResponsesAgentHostedPolicyIsPerRequest(t *testing.T) {
	for _, private := range []bool{true, false} {
		t.Run(fmt.Sprint(private), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var request struct {
					Tools []struct {
						Type string `json:"type"`
					} `json:"tools"`
				}
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
				}
				hosted := 0
				for _, tool := range request.Tools {
					if tool.Type == "web_search" {
						hosted++
					}
				}
				wantHosted := 0
				if requests == 2 {
					wantHosted = 1
				}
				if hosted != wantHosted {
					t.Errorf("request %d: hosted=%d want=%d", requests, hosted, wantHosted)
				}
				fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Ответ"}]}]}`)
			}))
			defer server.Close()
			client := &llm.Client{API: "responses", BaseURL: server.URL, HostedTools: []string{"web_search"}}
			agent := Agent{Client: client, Tools: &privacyTools{}, MaxRounds: 2, MaxCalls: 2}
			if !private {
				agent.MaxRounds = 0
			}
			messages := []llm.Message{{Role: "user", Content: "вопрос"}}
			if _, _, err := agent.CompleteDialogue(t.Context(), messages, private); err != nil {
				t.Fatal(err)
			}
			// Sharing the client must not disable hosted tools for other requests.
			if _, err := client.Complete(t.Context(), messages, false); err != nil {
				t.Fatal(err)
			}
			if requests != 2 {
				t.Fatalf("requests = %d, want 2", requests)
			}
		})
	}
}
