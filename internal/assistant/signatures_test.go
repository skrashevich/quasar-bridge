package assistant

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"quasarbridge/internal/llm"
)

// Decode requests independently of llm.Message so a field dropped by the client
// cannot also disappear from the assertion's view of the outgoing request.
type signatureWireMessage struct {
	Role         string         `json:"role"`
	Content      string         `json:"content"`
	ExtraContent jsontext.Value `json:"extra_content"`
	ToolCalls    []struct {
		ID           string           `json:"id"`
		Function     llm.FunctionCall `json:"function"`
		ExtraContent jsontext.Value   `json:"extra_content"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
}

func TestAgentRoundTripsToolMetadata(t *testing.T) {
	for _, tc := range []struct {
		name     string
		signed   bool
		parallel bool
	}{
		{"sequential signatures", true, false},
		{"parallel first signature only", true, true},
		{"ordinary sequential", false, false},
		{"ordinary parallel", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const extraA = `{"google":{"thought_signature":"opaque+/A==","future":[null,9007199254740993]},"vendor":{"unknown":true}}`
			const extraB = `{"google":{"thought_signature":"opaque/B=="}}`
			const messageExtra = `{"vendor":{"state":"opaque-message"}}`
			first := `{"id":"first","type":"function","function":{"name":"current_time","arguments":"{}"}`
			second := `{"id":"second","type":"function","function":{"name":"current_time","arguments":"{}"}`
			messageField := ""
			wantMessageExtra := ""
			if tc.signed {
				first += `,"extra_content":` + extraA
				if !tc.parallel {
					second += `,"extra_content":` + extraB
				}
				messageField = `,"extra_content":` + messageExtra
				wantMessageExtra = messageExtra
			}
			first += "}"
			second += "}"
			requests := 0
			client := &llm.Client{Model: "fixture", BaseURL: "https://fixture.invalid/v1", HTTP: &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				var body struct {
					Messages []signatureWireMessage `json:"messages"`
					Tools    []llm.Tool             `json:"tools"`
				}
				if err := json.UnmarshalRead(r.Body, &body); err != nil {
					t.Fatal(err)
				}
				var roles, ids, outputs []string
				for _, m := range body.Messages {
					roles = append(roles, m.Role)
					if m.Role == "tool" {
						outputs = append(outputs, m.ToolCallID)
					}
					if m.Role == "assistant" && string(m.ExtraContent) != wantMessageExtra {
						t.Errorf("message metadata lost or invented: %s", m.ExtraContent)
					}
					for _, call := range m.ToolCalls {
						ids = append(ids, call.ID)
						if call.Function.Name != "current_time" || call.Function.Arguments != "{}" {
							t.Errorf("function changed during replay: %+v", call.Function)
						}
						want := ""
						if tc.signed && call.ID == "first" {
							want = extraA
						} else if tc.signed && !tc.parallel {
							want = extraB
						}
						if string(call.ExtraContent) != want {
							t.Errorf("%s metadata = %s, want %s", call.ID, call.ExtraContent, want)
						}
					}
				}
				wantIDs := []string(nil)
				wantRoles := []string{"system", "user"}
				if requests > 0 {
					wantIDs = []string{"first"}
					wantRoles = append(wantRoles, "assistant", "tool")
					if tc.parallel {
						wantIDs = append(wantIDs, "second")
						wantRoles = append(wantRoles, "tool")
					} else if requests > 1 {
						wantIDs = append(wantIDs, "second")
						wantRoles = append(wantRoles, "assistant", "tool")
					}
				}
				if !slices.Equal(ids, wantIDs) || !slices.Equal(outputs, wantIDs) || !slices.Equal(roles, wantRoles) {
					t.Errorf("tool order/correlation: calls=%v outputs=%v roles=%v", ids, outputs, roles)
				}
				if len(wantIDs) == 2 && len(body.Tools) != 0 {
					t.Error("tools offered after exhausting call budget")
				}
				response := `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`
				if requests == 0 {
					batch := first
					if tc.parallel {
						batch += "," + second
					}
					response = `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[` + batch + `]` + messageField + `},"finish_reason":"tool_calls"}]}`
				} else if requests == 1 && !tc.parallel {
					response = `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[` + second + `]` + messageField + `},"finish_reason":"tool_calls"}]}`
				}
				requests++
				return signatureResponse(response), nil
			})}}
			// This fixture has no prefetch; exercise the real Agent and HTTP client
			// through two model steps, including a parallel batch despite the hint.
			tools := &privacyTools{}
			agent := Agent{Client: client, Tools: tools, MaxRounds: 2, MaxCalls: 2}
			answer, err := agent.Complete(t.Context(), []llm.Message{{Role: "user", Content: "time"}}, false)
			wantRequests := 3
			if tc.parallel {
				wantRequests = 2
			}
			if err != nil || answer != "done" || requests != wantRequests || !slices.Equal(tools.calls, []string{"current_time", "current_time"}) {
				t.Fatalf("answer=%q requests=%d tools=%v err=%v", answer, requests, tools.calls, err)
			}
		})
	}
}

func signatureResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

// Deliberately plans a history read followed by a web request: neither prefetch
// nor a signed model batch may bypass the Agent's existing history barrier.
type signaturePrefetchTools struct{ privacyTools }

func (*signaturePrefetchTools) InitialCalls(string) []llm.FunctionCall {
	return []llm.FunctionCall{{Name: "search_history", Arguments: `{}`}, {Name: "read_webpage", Arguments: `{}`}}
}

func TestGeminiDefersPrefetchToSignedModelCalls(t *testing.T) {
	for _, tc := range []struct {
		name, baseURL, model string
		deferPrefetch        bool
	}{
		{"Google endpoint with alias", "https://generativelanguage.googleapis.com/v1beta/openai/", "alias", true},
		{"Gemini proxy", "https://proxy.invalid/v1", "models/gemini-3.5-flash-lite", true},
		{"Gemini plain name", "https://proxy.invalid/v1", "gemini-3-pro", true},
		{"Gemini router", "https://proxy.invalid/v1", "google/gemini-3-pro", true},
		{"ordinary endpoint", "https://ordinary.invalid/v1", "qwen", false},
		{"similar hostname", "https://generativelanguage.googleapis.com.invalid/v1", "qwen", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := &signaturePrefetchTools{}
			requests := 0
			client := &llm.Client{BaseURL: tc.baseURL, Model: tc.model, HTTP: &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				var body struct {
					Messages []signatureWireMessage `json:"messages"`
					Tools    []llm.Tool             `json:"tools"`
				}
				if err := json.UnmarshalRead(r.Body, &body); err != nil {
					t.Fatal(err)
				}
				if requests == 0 && tc.deferPrefetch {
					if len(body.Messages) != 2 || len(tools.calls) != 0 {
						t.Error("Gemini received unsigned synthetic calls or prefetch executed early")
					}
					requests++
					return signatureResponse(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"history","type":"function","function":{"name":"search_history","arguments":"{}"},"extra_content":{"google":{"thought_signature":"opaque"}}},{"id":"web","type":"function","function":{"name":"read_webpage","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`), nil
				}
				for _, m := range body.Messages {
					if m.Role != "tool" && strings.Contains(m.Content, "private history") {
						t.Error("tool data promoted to instructions")
					}
					for _, call := range m.ToolCalls {
						if tc.deferPrefetch && strings.HasPrefix(call.ID, "context-") {
							t.Error("unsigned synthetic Gemini tool call")
						}
						if !tc.deferPrefetch && len(call.ExtraContent) != 0 {
							t.Error("provider metadata invented for ordinary endpoint")
						}
					}
				}
				last := body.Messages[len(body.Messages)-1]
				if last.Role != "tool" || !strings.Contains(last.Content, "blocked after accessing private history") {
					t.Error("history barrier did not reject the following network call")
				}
				if !slices.Equal(tools.calls, []string{"search_history"}) {
					t.Errorf("executed tools = %v", tools.calls)
				}
				for _, def := range body.Tools {
					if def.Function.Name == "read_webpage" {
						t.Error("network offered after history read")
					}
				}
				requests++
				return signatureResponse(`{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`), nil
			})}}
			agent := Agent{Client: client, Tools: tools, MaxRounds: 2, MaxCalls: 2}
			answer, private, err := agent.CompleteDialogue(t.Context(), []llm.Message{{Role: "user", Content: "history"}}, false)
			wantRequests := 1
			if tc.deferPrefetch {
				wantRequests = 2
			}
			if err != nil || answer != "done" || !private || requests != wantRequests {
				t.Fatalf("answer=%q private=%v requests=%d err=%v", answer, private, requests, err)
			}
		})
	}
}

func TestGeminiRequestsClockWithinBudget(t *testing.T) {
	for _, tc := range []struct {
		name                string
		maxRounds, maxCalls int
		wantExecutions      int
	}{
		{"normal", 1, 1, 1},
		{"no rounds", 0, 1, 0},
		{"no calls", 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Timezone = "UTC"
			executions, requests := 0, 0
			tools := &Tools{Config: cfg, Now: func() time.Time {
				executions++
				return time.Date(2026, 11, 15, 12, 30, 0, 0, time.UTC)
			}}
			client := &llm.Client{Model: "models/gemini-3.5-flash-lite", BaseURL: "https://fixture.invalid/v1", HTTP: &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				var body struct {
					Messages []signatureWireMessage `json:"messages"`
					Tools    []llm.Tool             `json:"tools"`
				}
				if err := json.UnmarshalRead(r.Body, &body); err != nil {
					t.Fatal(err)
				}
				requests++
				if requests == 1 {
					if len(body.Messages) != 2 || executions != 0 {
						t.Error("clock was prefetched without a model signature")
					}
					if (len(body.Tools) > 0) != (tc.wantExecutions > 0) {
						t.Error("tool definitions did not respect budget")
					}
					return signatureResponse(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"clock","type":"function","function":{"name":"current_time","arguments":"{}"},"extra_content":{"google":{"thought_signature":"opaque-clock"}}}]},"finish_reason":"tool_calls"}]}`), nil
				}
				last := body.Messages[len(body.Messages)-1]
				if last.Role != "tool" || last.ToolCallID != "clock" || !strings.Contains(last.Content, "2026-11-15T12:30:00Z") {
					t.Error("real clock result missing or promoted to instructions")
				}
				return signatureResponse(`{"choices":[{"message":{"content":"12:30"},"finish_reason":"stop"}]}`), nil
			})}}
			agent := Agent{Client: client, Tools: tools, MaxRounds: tc.maxRounds, MaxCalls: tc.maxCalls}
			answer, err := agent.Complete(t.Context(), []llm.Message{{Role: "user", Content: "time"}}, false)
			if executions != tc.wantExecutions || (err == nil) != (tc.wantExecutions > 0) {
				t.Fatalf("executions=%d answer=%q err=%v", executions, answer, err)
			}
			if tc.wantExecutions > 0 && (answer != "12:30" || requests != 2) {
				t.Fatalf("answer=%q requests=%d", answer, requests)
			}
		})
	}
}
