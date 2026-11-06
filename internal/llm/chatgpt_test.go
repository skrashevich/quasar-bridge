package llm

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChatGPTRequiresSubscriptionCredentials(t *testing.T) {
	c := Client{API: "chatgpt", APIKey: "must-not-use-api-key"}
	if _, err := c.Complete(t.Context(), nil, false); err == nil || !strings.Contains(err.Error(), "ChatGPT sign-in") {
		t.Fatalf("expected ChatGPT sign-in error, got %v", err)
	}
}

func chatGPTFixtureClient(t *testing.T, handler func(*http.Request) string) *Client {
	t.Helper()
	return &Client{
		API: "chatgpt", Model: "fixture", APIKey: "must-not-use-api-key",
		AccessToken: func(context.Context) (string, error) { return "subscription-fixture", nil },
		HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://api.openai.com/v1/responses" || r.Header.Get("Authorization") != "Bearer subscription-fixture" || r.Header.Get("Accept") != "text/event-stream" {
				t.Errorf("incorrect subscription request contract")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(handler(r))), Header: http.Header{"Content-Type": []string{"text/event-stream"}}}, nil
		})},
	}
}

func completedEvent(response string) string {
	return "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
}

func TestChatGPTRequestContract(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(fmt.Sprint(structured), func(t *testing.T) {
			c := chatGPTFixtureClient(t, func(r *http.Request) string {
				var body map[string]any
				if err := json.UnmarshalRead(r.Body, &body); err != nil {
					t.Fatal(err)
				}
				if body["stream"] != true || body["store"] != false || body["model"] != "fixture" {
					t.Errorf("request body=%v", body)
				}
				for _, field := range []string{"max_output_tokens", "max_tokens", "temperature", "text", "response_format", "previous_response_id", "conversation"} {
					if _, ok := body[field]; ok {
						t.Errorf("unsupported field %s", field)
					}
				}
				want := []any{map[string]any{"role": "developer", "content": "Reply in JSON"}, map[string]any{"role": "user", "content": "Привет"}}
				if !reflect.DeepEqual(body["input"], want) {
					t.Errorf("input=%v", body["input"])
				}
				return ": keepalive\r\nevent: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\r\n\r\n" + completedEvent(responseAnswer)
			})
			c.ReasoningEffort = "low"
			c.MaxOutputTokens = 999
			messages := []Message{{Role: "system", Content: "Reply in JSON"}, {Role: "user", Content: "Привет"}}
			got, err := c.Complete(t.Context(), messages, structured)
			if err != nil || got != "Ответ" || messages[0].Role != "system" {
				t.Fatalf("answer=%q error=%v original=%+v", got, err, messages)
			}
		})
	}
}

func TestChatGPTNamespacedToolCycle(t *testing.T) {
	const output = `[{"type":"reasoning","encrypted_content":"private-reasoning","summary":[]},{"type":"function_call","namespace":"quasar","name":"current_time","call_id":"call-1","arguments":"{}","status":"completed"}]`
	calls := 0
	c := chatGPTFixtureClient(t, func(r *http.Request) string {
		calls++
		var body map[string]any
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		definitions := body["tools"].([]any)
		namespace := definitions[0].(map[string]any)
		if namespace["type"] != "namespace" || namespace["name"] != "quasar" || len(namespace["tools"].([]any)) != 1 || len(definitions) != 2 {
			t.Errorf("tool definitions=%v", definitions)
		}
		if calls == 1 {
			return completedEvent(`{"status":"completed","output":` + output + `}`)
		}
		var replay []any
		if err := json.Unmarshal([]byte(output), &replay); err != nil {
			t.Fatal(err)
		}
		want := append([]any{map[string]any{"role": "user", "content": "time"}}, replay...)
		want = append(want, map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "12:00"})
		if !reflect.DeepEqual(body["input"], want) {
			t.Errorf("replay=%v want=%v", body["input"], want)
		}
		return completedEvent(responseAnswer)
	})
	c.HostedTools = []string{"web_search"}
	tools := []Tool{{Type: "function", Function: Function{Name: "current_time", Parameters: map[string]any{"type": "object"}}}}
	messages := []Message{{Role: "user", Content: "time"}}
	m, err := c.Chat(t.Context(), messages, tools)
	if err != nil || len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "current_time" {
		t.Fatalf("message=%+v error=%v", m, err)
	}
	messages = append(messages, m, Message{Role: "tool", ToolCallID: "call-1", Content: "12:00"})
	if answer, err := c.Chat(t.Context(), messages, tools); err != nil || answer.Content != "Ответ" {
		t.Fatalf("message=%+v error=%v", answer, err)
	}
}

func TestChatGPTRejectsUnfinishedOrFailedStreams(t *testing.T) {
	for _, tc := range []struct{ name, stream, want string }{
		{"empty", "", "not completed"},
		{"done only", "data: [DONE]\n\n", "not completed"},
		{"delta only", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", "not completed"},
		{"failed", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"subscription_sharing_usage_limit_exceeded\",\"message\":\"secret\"}}}\n\n", "usage limit"},
		{"incomplete", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n", "incomplete"},
		{"error", "data: {\"type\":\"error\",\"error\":{\"message\":\"secret\"}}\n\n", "failed"},
		{"error code", "data: {\"type\":\"error\",\"code\":\"subscription_sharing_usage_limit_exceeded\",\"message\":\"secret\"}\n\n", "usage limit"},
		{"malformed", "data: {\n\n", "invalid"},
		{"missing response", "data: {\"type\":\"response.completed\"}\n\n", "invalid"},
		{"wrong status", completedEvent(`{"status":"in_progress"}`), "not completed"},
		{"oversized line", "data: " + strings.Repeat("x", (2<<20)+1), "size limit"},
		{"oversized comments", strings.Repeat(": keepalive\n", (2<<20)/10), "size limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := chatGPTFixtureClient(t, func(*http.Request) string { return tc.stream })
			m, err := c.Chat(t.Context(), nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") || m.Content != "" || len(m.ToolCalls) != 0 {
				t.Fatalf("message=%+v error=%v", m, err)
			}
		})
	}
}

func TestResponseStreamFraming(t *testing.T) {
	for _, stream := range []string{
		strings.TrimRight(completedEvent(responseAnswer), "\n"),
		"event: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":" + responseAnswer + "}\r\n\r\n",
		"\xef\xbb\xbf" + completedEvent(responseAnswer),
	} {
		c := chatGPTFixtureClient(t, func(*http.Request) string { return stream })
		if got, err := c.Complete(t.Context(), nil, false); err != nil || got != "Ответ" {
			t.Fatalf("answer=%q error=%v", got, err)
		}
	}
}

func TestChatGPTRejectsUnsupportedConfigurationBeforeAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Client)
	}{
		{"endpoint", func(c *Client) { c.BaseURL = "https://example.invalid/v1" }},
		{"code interpreter", func(c *Client) { c.HostedTools = []string{"code_interpreter"} }},
		{"chat parameters", func(c *Client) { c.ChatParameters = map[string]any{"temperature": 0} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Client{API: "chatgpt", AccessToken: func(context.Context) (string, error) {
				t.Error("authentication called with unsupported configuration")
				return "", errors.New("secret")
			}}
			tc.set(&c)
			if _, err := c.Complete(t.Context(), nil, false); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestChatGPTUsesFreshCredentialsForEachRequest(t *testing.T) {
	tokens := 0
	c := chatGPTFixtureClient(t, func(*http.Request) string { return completedEvent(responseAnswer) })
	c.AccessToken = func(context.Context) (string, error) { tokens++; return "subscription-fixture", nil }
	for range 2 {
		if _, err := c.Complete(t.Context(), nil, false); err != nil {
			t.Fatal(err)
		}
	}
	if tokens != 2 {
		t.Fatalf("credential requests=%d", tokens)
	}
}

func TestChatGPTSanitizesCredentialFailures(t *testing.T) {
	c := Client{API: "chatgpt", AccessToken: func(context.Context) (string, error) { return "", errors.New("secret token") }}
	if _, err := c.Complete(t.Context(), nil, false); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error=%v", err)
	}
}

func TestChatGPTHonorsCancellation(t *testing.T) {
	c := Client{API: "chatgpt", AccessToken: func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.Complete(ctx, nil, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
}

func TestChatGPTHTTPFailuresAreActionableAndSanitized(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"error":{"message":"secret"}}`, "authentication rejected"},
		{403, `{"error":{"code":"subscription_sharing_user_not_eligible","message":"secret"}}`, "unavailable for this account"},
		{429, `{"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`, "usage limit"},
		{503, `{"error":{"code":"subscription_sharing_usage_unavailable"}}`, "temporarily unavailable"},
		{400, `{"error":{"code":"subscription_sharing_unsupported_capability","param":"secret"}}`, "unsupported model"},
		{500, `{"error":{"code":"unknown-secret-code","message":"secret"}}`, "HTTP 500"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			c := Client{API: "chatgpt", AccessToken: func(context.Context) (string, error) { return "fixture", nil }, HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}}
			if _, err := c.Complete(t.Context(), nil, false); err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestChatGPTDoesNotFollowRedirects(t *testing.T) {
	requests := 0
	c := Client{API: "chatgpt", AccessToken: func(context.Context) (string, error) { return "fixture", nil }, HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": []string{"https://api.openai.com/other"}}}, nil
	})}}
	if _, err := c.Complete(t.Context(), nil, false); err == nil || !strings.Contains(err.Error(), "HTTP 307") || requests != 1 {
		t.Fatalf("requests=%d error=%v", requests, err)
	}
}

func TestChatGPTSyntheticToolContextUsesNamespace(t *testing.T) {
	c := chatGPTFixtureClient(t, func(r *http.Request) string {
		var body struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Input) != 2 || body.Input[0]["namespace"] != "quasar" || body.Input[0]["name"] != "current_time" || body.Input[1]["call_id"] != "call-1" {
			t.Errorf("input=%v", body.Input)
		}
		return completedEvent(responseAnswer)
	})
	messages := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Function: FunctionCall{Name: "current_time", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "call-1", Content: "12:00"},
	}
	if _, err := c.Chat(t.Context(), messages, nil); err != nil {
		t.Fatal(err)
	}
}

func TestChatGPTHostedToolsRespectPrivacyAndStructuredPlanning(t *testing.T) {
	for _, structured := range []bool{false, true} {
		for _, private := range []bool{false, true} {
			t.Run(fmt.Sprintf("structured=%t/private=%t", structured, private), func(t *testing.T) {
				c := chatGPTFixtureClient(t, func(r *http.Request) string {
					var body map[string]any
					if err := json.UnmarshalRead(r.Body, &body); err != nil {
						t.Fatal(err)
					}
					_, gotTools := body["tools"]
					if gotTools != (!structured && !private) {
						t.Errorf("tools=%v", body["tools"])
					}
					return completedEvent(responseAnswer)
				})
				c.HostedTools = []string{"web_search"}
				ctx := t.Context()
				if private {
					ctx = WithoutHostedTools(ctx)
				}
				if _, err := c.Complete(ctx, nil, structured); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestChatGPTConcurrentResponsesKeepOutputLocal(t *testing.T) {
	var tokens atomic.Int64
	c := chatGPTFixtureClient(t, func(r *http.Request) string {
		var body struct {
			Input []Message `json:"input"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil || len(body.Input) != 1 {
			t.Errorf("unexpected input: %v", err)
			return ""
		}
		encoded, err := json.Marshal(body.Input[0].Content)
		if err != nil {
			t.Error(err)
		}
		return completedEvent(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + string(encoded) + `}]}]}`)
	})
	c.AccessToken = func(context.Context) (string, error) { tokens.Add(1); return "subscription-fixture", nil }
	var wg sync.WaitGroup
	for _, input := range []string{"first", "second"} {
		wg.Go(func() {
			answer, err := c.Complete(t.Context(), []Message{{Role: "user", Content: input}}, false)
			if err != nil || answer != input {
				t.Errorf("input=%q answer=%q error=%v", input, answer, err)
			}
		})
	}
	wg.Wait()
	if tokens.Load() != 2 {
		t.Fatalf("token requests=%d", tokens.Load())
	}
}
