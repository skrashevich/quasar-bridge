package bridge

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/llm"
)

type configuredClassifierTransport struct{ target *url.URL }

func (tr configuredClassifierTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	local := req.Clone(req.Context())
	local.URL = req.URL.Clone()
	local.URL.Scheme, local.URL.Host = tr.target.Scheme, tr.target.Host
	return http.DefaultTransport.RoundTrip(local)
}

func TestClassifierUsesConfiguredChatParameters(t *testing.T) {
	for _, tc := range []struct{ name, answer, route, source string }{
		{"valid", `{"route":"llm"}`, "llm", "classifier"},
		{"plain_text", "Use the LLM", "yandex", "classifier_error"},
		{"invalid_route", `{"route":"unknown"}`, "yandex", "classifier_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.UnmarshalRead(r.Body, &body); err != nil {
					t.Error(err)
				}
				for _, key := range []string{"temperature", "response_format", "chat_template_kwargs"} {
					if _, ok := body[key]; ok {
						t.Errorf("disabled chat parameter: %s", key)
						http.Error(w, "unsupported parameter", http.StatusBadRequest)
						return
					}
				}
				messages, _ := body["messages"].([]any)
				if len(messages) < 1 || !strings.Contains(messages[0].(map[string]any)["content"].(string), "JSON") {
					t.Error("classifier lost its JSON instruction")
				}
				if err := json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": tc.answer}, "finish_reason": "stop"}}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			engine := &Engine{LLM: &llm.Client{BaseURL: "https://proxy.example.invalid/v1", Model: "fixture", ChatParameters: map[string]any{"temperature": nil, "response_format": nil, "chat_template_kwargs": nil}, HTTP: &http.Client{Transport: configuredClassifierTransport{target}}}}
			cfg := DefaultRoutingConfig()
			cfg.Rules = nil
			if err := engine.SetRouting(cfg); err != nil {
				t.Fatal(err)
			}
			decision := engine.Decide(t.Context(), "запрос для классификации")
			if decision.Route != tc.route || decision.Source != tc.source {
				t.Fatalf("decision = %+v, want %s/%s", decision, tc.route, tc.source)
			}
		})
	}
}
