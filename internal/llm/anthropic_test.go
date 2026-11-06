package llm

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIsAnthropicMatchesOnlyTheOfficialHTTPSHost(t *testing.T) {
	for url, want := range map[string]bool{
		"https://api.anthropic.com/v1":              true,
		"https://api.anthropic.com/v1/":             true,
		"https://API.Anthropic.com/v1":              true,
		"https://api.anthropic.com:443/v1":          true,
		"http://api.anthropic.com/v1":               false,
		"https://api.anthropic.com.evil.example/v1": false,
		"https://evil.example/api.anthropic.com/v1": false,
		"https://user@api.anthropic.com/v1":         false,
		"https://anthropic.com/v1":                  false,
		"https://api.openai.com/v1":                 false,
		"":                                          false,
	} {
		if got := isAnthropic(url); got != want {
			t.Errorf("isAnthropic(%q)=%v, want %v", url, got, want)
		}
	}
}

func TestAnthropicModelsSendAnthropicHeadersAndNoBearer(t *testing.T) {
	c := Client{
		BaseURL: AnthropicBaseURL + "/", APIKey: "anthropic-fixture",
		HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet || r.URL.String() != "https://api.anthropic.com/v1/models?limit=1000" {
				t.Errorf("request=%s %s", r.Method, r.URL)
			}
			if r.Header.Get("x-api-key") != "anthropic-fixture" || r.Header.Get("anthropic-version") != "2023-06-01" {
				t.Errorf("Anthropic headers were not sent: %v", r.Header)
			}
			if r.Header.Get("Authorization") != "" {
				t.Error("bearer token must not be sent to Anthropic")
			}
			body := `{"data":[{"id":"claude-haiku-5-5","display_name":"Claude Haiku 5.5","type":"model"},{"id":"claude-opus-5","display_name":"Claude Opus 5"}],"has_more":false}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
	got, err := c.Models(t.Context())
	want := []Model{{ID: "claude-haiku-5-5", Name: "Claude Haiku 5.5"}, {ID: "claude-opus-5", Name: "Claude Opus 5"}}
	if err != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("models=%+v error=%v", got, err)
	}
}

func TestNonAnthropicProvidersKeepBearerAndNeverReceiveAnthropicHeaders(t *testing.T) {
	for _, base := range []string{"https://api.openai.com/v1", "https://api.anthropic.com.evil.example/v1", "http://api.anthropic.com/v1"} {
		c := Client{
			BaseURL: base, APIKey: "provider-fixture",
			HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer provider-fixture" || r.Header.Get("x-api-key") != "" || r.Header.Get("anthropic-version") != "" {
					t.Errorf("%s: unexpected headers %v", base, r.Header)
				}
				if r.URL.RawQuery != "" {
					t.Errorf("%s: unexpected query %q", base, r.URL.RawQuery)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"m"}]}`))}, nil
			})},
		}
		if _, err := c.Models(t.Context()); err != nil {
			t.Errorf("%s: %v", base, err)
		}
	}
}

func anthropicChatClient(t *testing.T, seen *map[string]any) Client {
	t.Helper()
	return Client{
		BaseURL: AnthropicBaseURL, Model: "claude-haiku-5-5", APIKey: "anthropic-fixture",
		HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodPost || r.URL.String() != "https://api.anthropic.com/v1/chat/completions" {
				t.Errorf("request=%s %s", r.Method, r.URL)
			}
			if r.Header.Get("x-api-key") != "anthropic-fixture" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
				t.Errorf("Anthropic headers were not respected: %v", r.Header)
			}
			*seen = nil
			if err := json.UnmarshalRead(r.Body, seen); err != nil {
				t.Errorf("request body: %v", err)
			}
			body := `{"choices":[{"message":{"role":"assistant","content":"Привет"},"finish_reason":"stop"}]}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
}

func TestAnthropicChatSendsHeadersAndOmitsFieldsItRejects(t *testing.T) {
	for _, structured := range []bool{false, true} {
		var body map[string]any
		c := anthropicChatClient(t, &body)
		got, err := c.Complete(t.Context(), []Message{{Role: "user", Content: "Привет"}}, structured)
		if err != nil || got != "Привет" {
			t.Fatalf("structured=%v answer=%q error=%v", structured, got, err)
		}
		// Verified against the live API: these fields are rejected with HTTP 400
		// (temperature, response_format) or have no meaning (chat_template_kwargs).
		for _, key := range []string{"temperature", "response_format", "chat_template_kwargs"} {
			if _, ok := body[key]; ok {
				t.Errorf("structured=%v: %s must not be sent to Anthropic", structured, key)
			}
		}
		if body["model"] != "claude-haiku-5-5" || body["max_tokens"] != float64(1024) || body["stream"] != false {
			t.Errorf("structured=%v: body=%v", structured, body)
		}
	}
}

func TestAnthropicChatParametersStillOverrideDefaults(t *testing.T) {
	var body map[string]any
	c := anthropicChatClient(t, &body)
	c.ChatParameters = map[string]any{"max_tokens": 256, "response_format": map[string]any{"type": "json_schema"}}
	if _, err := c.Complete(t.Context(), []Message{{Role: "user", Content: "Привет"}}, true); err != nil {
		t.Fatal(err)
	}
	if body["max_tokens"] != float64(256) || body["response_format"] == nil {
		t.Errorf("explicit llm_chat_parameters were ignored: %v", body)
	}
}

func TestAnthropicRequestsDoNotFollowRedirectsWithTheKey(t *testing.T) {
	leaked := false
	c := Client{
		BaseURL: AnthropicBaseURL, Model: "claude-haiku-5-5", APIKey: "anthropic-fixture",
		HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "api.anthropic.com" {
				leaked = r.Header.Get("x-api-key") != ""
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}
			h := http.Header{"Location": {"https://redirect.example/steal"}}
			return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: h, Body: http.NoBody}, nil
		})},
	}
	if _, err := c.Complete(t.Context(), []Message{{Role: "user", Content: "Привет"}}, false); err == nil {
		t.Fatal("redirect was treated as success")
	}
	if leaked {
		t.Fatal("x-api-key was forwarded by a redirect")
	}
}
