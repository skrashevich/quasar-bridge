package llm

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestQuotaFallbackConditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"quotaHTTP", 429, `{"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`, true},
		{"quotaSSE", 200, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"subscription_sharing_usage_limit_exceeded\"}}}\n\n", true},
		{"success", 200, completedEvent(responseAnswer), false},
		{"rateLimit", 429, `{"error":{"code":"rate_limit_exceeded"}}`, false},
		{"bare429", 429, `{}`, false},
		{"auth", 401, `{}`, false},
		{"server", 503, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			primary := chatGPTFixtureClient(t, func(*http.Request) string { return "" })
			primary.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer subscription-fixture" {
					t.Error("wrong primary credential")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			secondary := &Client{API: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "openai/gpt-6.1-terra", APIKey: "router-key", HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "Bearer router-key" || r.URL.Host != "openrouter.ai" {
					t.Error("credentials crossed providers")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(responseAnswer))}, nil
			})}}
			f := &QuotaFallback{Primary: primary, Secondary: secondary}
			_, err := f.Complete(t.Context(), []Message{{Role: "user", Content: "вопрос"}}, false)
			if (calls == 1) != tc.want {
				t.Fatalf("fallback calls=%d, want=%v", calls, tc.want)
			}
			if tc.want && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQuotaFallbackPreservesToolResultsAndDeadline(t *testing.T) {
	primary := chatGPTFixtureClient(t, func(*http.Request) string {
		return "data: {\"type\":\"error\",\"code\":\"subscription_sharing_usage_limit_exceeded\"}\n\n"
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	secondary := &Client{API: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "fallback", ReasoningEffort: "medium", HostedTools: []string{"web_search"}, HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		got, _ := r.Context().Deadline()
		if !got.Equal(deadline) {
			t.Error("fallback reset deadline")
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "primary-private") || strings.Contains(string(body), "namespace") {
			t.Error("foreign response items leaked")
		}
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		if request["reasoning"].(map[string]any)["effort"] != "medium" {
			t.Error("reasoning effort lost")
		}
		if strings.Contains(string(body), "openrouter:web_search") {
			t.Error("privacy barrier lost")
		}
		if !strings.Contains(string(body), "function_call_output") || !strings.Contains(string(body), "tool-result-once") {
			t.Error("completed tool result missing")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(responseAnswer))}, nil
	})}}
	previous, err := parseResponse([]byte(`{"status":"completed","output":[{"type":"reasoning","encrypted_content":"primary-private"},{"type":"function_call","namespace":"quasar","name":"current_time","call_id":"call1","arguments":"{}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	previous.responseAPI = "chatgpt"
	f := &QuotaFallback{Primary: primary, Secondary: secondary}
	_, err = f.Chat(WithoutHostedTools(ctx), []Message{{Role: "user", Content: "время"}, previous, {Role: "tool", ToolCallID: "call1", Content: "tool-result-once"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestQuotaFallbackCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	primary := chatGPTFixtureClient(t, func(*http.Request) string {
		cancel()
		return "data: {\"type\":\"error\",\"code\":\"subscription_sharing_usage_limit_exceeded\"}\n\n"
	})
	secondary := &Client{API: "openrouter", HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("canceled request reached fallback")
		return nil, errors.New("unexpected")
	})}}
	f := &QuotaFallback{Primary: primary, Secondary: secondary}
	_, err := f.Complete(ctx, nil, false)
	if err == nil {
		t.Fatal("expected cancellation or quota error")
	}
}
