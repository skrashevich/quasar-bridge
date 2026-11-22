package llm

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompleteUsesOpenAIContractAndRejectsEmptyAnswers(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-key" {
			http.Error(w, "bad request", 400)
			return
		}
		var body struct {
			Model          string            `json:"model"`
			Messages       []Message         `json:"messages"`
			ResponseFormat map[string]string `json:"response_format"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil || body.Model != "fixture-model" || len(body.Messages) != 1 || body.Messages[0].Content != "Привет" || body.ResponseFormat["type"] != "json_object" {
			http.Error(w, "bad payload", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"route\":\"llm\"}"},"finish_reason":"stop"}]}`))
	}))
	defer s.Close()
	c := Client{BaseURL: s.URL + "/v1", Model: "fixture-model", APIKey: "fixture-key"}
	got, err := c.Complete(t.Context(), []Message{{Role: "user", Content: "Привет"}}, true)
	if err != nil || got != `{"route":"llm"}` {
		t.Fatalf("answer=%q error=%v", got, err)
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"choices":[]}`)) }))
	defer empty.Close()
	c.BaseURL = empty.URL
	if _, err := c.Complete(t.Context(), nil, false); err == nil {
		t.Fatal("empty answer accepted")
	}
}

func TestCompleteHonorsCancellation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(250 * time.Millisecond):
		}
	}))
	defer s.Close()
	c := Client{BaseURL: s.URL, Model: "fixture-model"}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Complete(ctx, []Message{{Role: "user", Content: "test"}}, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error=%v", err)
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTransportErrorDoesNotRevealConfiguredURL(t *testing.T) {
	endpoint := "https://llm.example.invalid/capability-placeholder"
	c := Client{BaseURL: endpoint, HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection failed") })}}
	_, err := c.Complete(t.Context(), nil, false)
	if err == nil || strings.Contains(err.Error(), endpoint) {
		t.Fatalf("endpoint leaked in error: %v", err)
	}
}

func TestChatSupportsToolCallsAndToolResults(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Messages []Message `json:"messages"`
			Tools    []Tool    `json:"tools"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
			http.Error(w, "bad", 400)
			return
		}
		if len(body.Tools) != 1 || body.Tools[0].Function.Name != "current_time" {
			t.Error("missing tool definitions")
		}
		if calls == 1 {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"current_time","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		if len(body.Messages) != 3 || body.Messages[2].Role != "tool" || body.Messages[2].ToolCallID != "call-1" {
			t.Error("missing correlated tool result")
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Сейчас полдень"},"finish_reason":"stop"}]}`))
	}))
	defer s.Close()
	c := Client{BaseURL: s.URL, Model: "fixture"}
	tools := []Tool{{Type: "function", Function: Function{Name: "current_time", Parameters: map[string]any{"type": "object"}}}}
	messages := []Message{{Role: "user", Content: "который час"}}
	msg, err := c.Chat(t.Context(), messages, tools)
	if err != nil || len(msg.ToolCalls) != 1 {
		t.Fatalf("tool call: %+v %v", msg, err)
	}
	messages = append(messages, msg, Message{Role: "tool", ToolCallID: msg.ToolCalls[0].ID, Content: `{"hour":12}`})
	msg, err = c.Chat(t.Context(), messages, tools)
	if err != nil || msg.Content != "Сейчас полдень" {
		t.Fatalf("answer: %+v %v", msg, err)
	}
}

func TestChatCompletionsRejectsErrorFinishReason(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Provider said: HTTP 404"},"finish_reason":"error"}]}`))
	}))
	defer s.Close()
	c := Client{BaseURL: s.URL + "/v1", Model: "fixture-model", APIKey: "fixture-key"}
	if got, err := c.Complete(t.Context(), []Message{{Role: "user", Content: "Привет"}}, false); err == nil {
		t.Fatalf("error finish_reason accepted as answer %q", got)
	}
}
