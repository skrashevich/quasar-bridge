package llm

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOpenRouterWebSearchMapping(t *testing.T) {
	c := &Client{API: "openrouter", BaseURL: "https://openrouter.ai/api/v1", HostedTools: []string{"web_search"}, HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/api/v1/responses" || !strings.Contains(string(body), `"type":"openrouter:web_search"`) || strings.Contains(string(body), "reasoning.encrypted_content") {
			t.Fatalf("invalid OpenRouter search request: %s", body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(responseAnswer))}, nil
	})}}
	if _, err := c.Complete(t.Context(), []Message{{Role: "user", Content: "новости"}}, false); err != nil {
		t.Fatal(err)
	}
}
