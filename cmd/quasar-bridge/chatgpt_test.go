package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

type chatGPTTransport func(*http.Request) (*http.Response, error)

func (f chatGPTTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestChatGPTConfiguredClientUsesAccountInsteadOfAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	const credentials = `{"version":1,"ext_agent_host_id":"host-fixture","issuer":"https://auth.openai.com","subject":"user-fixture","client_id":"oaiapp_fixture","access_token":"subscription-fixture","refresh_token":"refresh-fixture","scope":"chatgpt.tokens.use.direct offline_access","expires_at":"2100-01-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(credentials), 0600); err != nil {
		t.Fatal(err)
	}
	encodedPath, err := json.Marshal(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(configFile(t, `{"llm_api":"chatgpt","llm_model":"account-model","llm_chatgpt_auth_file":`+string(encodedPath)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	c := newLLMClient(cfg, "unrelated-paid-api-key")
	c.HTTP = &http.Client{Transport: chatGPTTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.openai.com/v1/responses" || r.Header.Get("Authorization") != "Bearer subscription-fixture" {
			t.Fatal("subscription request used the wrong endpoint or credential")
		}
		body := `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Связь"}]}]}}` + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	answer, err := c.Complete(t.Context(), []llm.Message{{Role: "user", Content: "проверка"}}, false)
	if err != nil || answer != "Связь" {
		t.Fatalf("configured subscription client: answer=%q, error=%v", answer, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(t.Context(), nil, false); err == nil {
		t.Fatal("missing sign-in fell back to a paid API key")
	}
}

func TestChatGPTModelsUsesAccountCatalog(t *testing.T) {
	var out bytes.Buffer
	hc := &http.Client{Transport: chatGPTTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.String() != "https://api.openai.com/v1/models" || r.Header.Get("Authorization") != "Bearer account-fixture" {
			t.Fatal("model catalog request used the wrong endpoint or credential")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"available-model","display_name":"Available","visibility":"list"},{"slug":"hidden-model","display_name":"Hidden","visibility":"hidden"}]}`))}, nil
	})}
	err := listChatGPTModels(t.Context(), func(context.Context) (string, error) { return "account-fixture", nil }, hc, &out)
	if err != nil || !strings.Contains(out.String(), `"available-model"`) || strings.Contains(out.String(), "hidden-model") {
		t.Fatalf("catalog: %q, error=%v", out.String(), err)
	}
}

func TestChatGPTModelsRejectsRedirectAndMalformedCatalog(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"redirect", "", http.StatusFound},
		{"API key catalog", `{"data":[{"id":"paid-model"}]}`, http.StatusOK},
		{"oversized", strings.Repeat(" ", (2<<20)+1), http.StatusOK},
		{"untrusted server error", `{"error":{"message":"private-token"}}`, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			hc := &http.Client{Transport: chatGPTTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": {"https://other.example.invalid/private"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			var out bytes.Buffer
			err := listChatGPTModels(t.Context(), func(context.Context) (string, error) { return "private-token", nil }, hc, &out)
			if err == nil || strings.Contains(err.Error(), "private-token") || calls != 1 || out.Len() != 0 {
				t.Fatalf("catalog failure: error=%v, calls=%d, output=%q", err, calls, out.String())
			}
		})
	}
}

func TestChatGPTStatusDoesNotRequireSignIn(t *testing.T) {
	var out bytes.Buffer
	err := runChatGPTAuth(t.Context(), "status", filepath.Join(t.TempDir(), "missing.json"), "", &out)
	if err != nil || !strings.Contains(out.String(), `"signed_in":false`) {
		t.Fatalf("unsigned status: %q, %v", out.String(), err)
	}
	if err := runChatGPTAuth(t.Context(), "invalid", filepath.Join(t.TempDir(), "missing.json"), "", &out); err == nil {
		t.Fatal("unknown auth action accepted")
	}
}

func TestChatGPTModelsDoesNotRequestWithoutToken(t *testing.T) {
	hc := &http.Client{Transport: chatGPTTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("request sent without account credentials")
		return nil, errors.New("unexpected network access")
	})}
	if err := listChatGPTModels(t.Context(), func(context.Context) (string, error) { return "", nil }, hc, io.Discard); err == nil {
		t.Fatal("empty credential accepted")
	}
}
