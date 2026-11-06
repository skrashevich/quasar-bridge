package llm

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// Route the configured endpoint through loopback without any external request.
func endpointTestClient(t *testing.T, endpoint string, handler http.HandlerFunc) Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	if endpoint == "" {
		endpoint = server.URL + "/v1"
	}
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return Client{BaseURL: endpoint, Model: "anthropic/claude-opus-4-7", HTTP: &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != strings.TrimRight(endpoint, "/")+"/chat/completions" {
			t.Errorf("unexpected request endpoint")
		}
		local := req.Clone(req.Context())
		local.URL.Scheme = target.Scheme
		local.URL.Host = target.Host
		return server.Client().Transport.RoundTrip(local)
	})}}
}

func readChatRequest(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		t.Error(err)
	}
	return body
}

func writeChatAnswer(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}`))
}

// Only the official Anthropic endpoint gets different defaults (see
// anthropic_test.go); the host name of any other provider and the model name,
// even a Claude model served by a proxy, change nothing.
func TestChatDefaultsDoNotDependOnProxyEndpointOrModel(t *testing.T) {
	for _, endpoint := range []string{"", "https://openrouter.example.invalid/api/v1"} {
		t.Run(endpoint, func(t *testing.T) {
			c := endpointTestClient(t, endpoint, func(w http.ResponseWriter, r *http.Request) {
				body := readChatRequest(t, r)
				if body["temperature"] != float64(0) || body["max_tokens"] != float64(1024) || body["stream"] != false || body["model"] != "anthropic/claude-opus-4-7" {
					t.Errorf("request defaults = %v", body)
				}
				if !reflect.DeepEqual(body["chat_template_kwargs"], map[string]any{"enable_thinking": false}) || !reflect.DeepEqual(body["response_format"], map[string]any{"type": "json_object"}) {
					t.Errorf("structured request defaults = %v", body)
				}
				writeChatAnswer(w)
			})
			if _, err := c.Complete(t.Context(), []Message{{Role: "user", Content: "hello"}}, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestChatParametersOmitDefaultsOnLoopbackAndProxy(t *testing.T) {
	for _, endpoint := range []string{"", "http://proxy.example.invalid/v1"} {
		t.Run(endpoint, func(t *testing.T) {
			c := endpointTestClient(t, endpoint, func(w http.ResponseWriter, r *http.Request) {
				body := readChatRequest(t, r)
				for _, key := range []string{"temperature", "response_format", "chat_template_kwargs", "max_tokens"} {
					if _, ok := body[key]; ok {
						t.Errorf("explicitly omitted parameter %q remains", key)
					}
				}
				if body["max_completion_tokens"] != float64(2048) || body["reasoning_effort"] != "low" {
					t.Errorf("explicit parameters missing: %v", body)
				}
				writeChatAnswer(w)
			})
			c.ChatParameters = map[string]any{"temperature": nil, "response_format": nil, "chat_template_kwargs": nil, "max_tokens": nil, "max_completion_tokens": 2048, "reasoning_effort": "low"}
			if _, err := c.Complete(t.Context(), nil, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestChatParametersReplaceDefaultsWithoutMutation(t *testing.T) {
	parameters := map[string]any{
		"temperature": 0.7, "max_tokens": 123,
		"response_format":      map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "answer"}},
		"chat_template_kwargs": map[string]any{"enable_thinking": true},
	}
	before, err := json.Marshal(parameters, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"structured", "plain", "chat"} {
		t.Run(mode, func(t *testing.T) {
			c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
				body := readChatRequest(t, r)
				if body["temperature"] != 0.7 || body["max_tokens"] != float64(123) || !reflect.DeepEqual(body["chat_template_kwargs"], map[string]any{"enable_thinking": true}) {
					t.Errorf("parameter replacements missing: %v", body)
				}
				if mode == "structured" {
					if !reflect.DeepEqual(body["response_format"], parameters["response_format"]) {
						t.Errorf("response_format = %v", body["response_format"])
					}
				} else if _, ok := body["response_format"]; ok {
					t.Error("plain request forces structured output")
				}
				writeChatAnswer(w)
			})
			c.ChatParameters = parameters
			for range 2 {
				var err error
				if mode == "chat" {
					_, err = c.Chat(t.Context(), nil, nil)
				} else {
					_, err = c.Complete(t.Context(), nil, mode == "structured")
				}
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	after, err := json.Marshal(parameters, json.Deterministic(true))
	if err != nil || string(before) != string(after) {
		t.Fatalf("parameters mutated: %s -> %s (%v)", before, after, err)
	}
}

func TestEmptyChatParametersRetainDefaults(t *testing.T) {
	for _, parameters := range []map[string]any{nil, {}} {
		c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
			body := readChatRequest(t, r)
			if body["temperature"] != float64(0) || body["max_tokens"] != float64(1024) || !reflect.DeepEqual(body["chat_template_kwargs"], map[string]any{"enable_thinking": false}) {
				t.Errorf("empty parameters changed defaults: %v", body)
			}
			if _, ok := body["response_format"]; ok {
				t.Error("plain request includes response_format")
			}
			writeChatAnswer(w)
		})
		c.ChatParameters = parameters
		if _, err := c.Complete(t.Context(), nil, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidateChatParametersAllowsOnlySafeKeys(t *testing.T) {
	// Value schemas belong to the API; the bridge validates key membership only.
	for _, key := range []string{"temperature", "max_tokens", "max_completion_tokens", "response_format", "chat_template_kwargs", "reasoning_effort"} {
		for _, value := range []any{nil, "provider-specific-value", map[string]any{"custom": true}} {
			if err := ValidateChatParameters(map[string]any{key: value}); err != nil {
				t.Errorf("allowed key %q rejected: %v", key, err)
			}
		}
	}
	for _, parameters := range []map[string]any{nil, {}} {
		if err := ValidateChatParameters(parameters); err != nil {
			t.Errorf("empty parameters rejected: %v", err)
		}
	}
	for _, key := range []string{"tools", "stream", "model", "functions", "tool_choice", "parallel_tool_calls", "messages", "unknown", "https://secret.invalid/key"} {
		t.Run(key, func(t *testing.T) {
			const secret = "secret-parameter-value"
			parameters := map[string]any{key: secret}
			err := ValidateChatParameters(parameters)
			if err == nil || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), secret) {
				t.Fatalf("unsafe parameter validation error = %v", err)
			}
			c := Client{BaseURL: "https://secret.invalid/endpoint", ChatParameters: parameters, HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				t.Error("invalid parameters reached HTTP")
				return nil, errors.New("unexpected request")
			})}}
			_, err = c.Chat(t.Context(), nil, nil)
			if err == nil || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), c.BaseURL) {
				t.Fatalf("unsafe direct client error = %v", err)
			}
		})
	}
}

func TestResponsesRejectsChatParametersBeforeHTTP(t *testing.T) {
	c := Client{API: "responses", ChatParameters: map[string]any{"temperature": nil}, HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("incompatible parameters reached HTTP")
		return nil, errors.New("unexpected request")
	})}}
	if _, err := c.Complete(t.Context(), nil, true); err == nil {
		t.Fatal("responses ignored chat parameters")
	}
}

func TestChatParametersPreserveFunctionCycle(t *testing.T) {
	calls := 0
	c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		calls++
		body := readChatRequest(t, r)
		for _, key := range []string{"temperature", "response_format", "chat_template_kwargs"} {
			if _, ok := body[key]; ok {
				t.Errorf("explicitly omitted parameter %q remains", key)
			}
		}
		wantTools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "current_time", "description": "Current time", "parameters": map[string]any{"type": "object"}}}}
		if !reflect.DeepEqual(body["tools"], wantTools) || body["tool_choice"] != "auto" || body["parallel_tool_calls"] != false || body["stream"] != false {
			t.Errorf("function tool settings changed: %v", body)
		}
		if calls == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"current_time","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) != 3 {
			t.Errorf("missing tool cycle messages: %v", body["messages"])
		} else if !reflect.DeepEqual(messages[2], map[string]any{"role": "tool", "tool_call_id": "call-1", "content": `{"hour":12}`}) {
			t.Errorf("tool result lost correlation: %v", messages[2])
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Noon"},"finish_reason":"stop"}]}`))
	})
	c.ChatParameters = map[string]any{"temperature": nil, "response_format": nil, "chat_template_kwargs": nil}
	tools := []Tool{{Type: "function", Function: Function{Name: "current_time", Description: "Current time", Parameters: map[string]any{"type": "object"}}}}
	messages := []Message{{Role: "user", Content: "What time is it?"}}
	first, err := c.Chat(t.Context(), messages, tools)
	if err != nil || first.Role != "assistant" || len(first.ToolCalls) != 1 || first.ToolCalls[0].Function.Name != "current_time" {
		t.Fatalf("tool call = %+v, error = %v", first, err)
	}
	messages = append(messages, first, Message{Role: "tool", ToolCallID: first.ToolCalls[0].ID, Content: `{"hour":12}`})
	last, err := c.Chat(t.Context(), messages, tools)
	if err != nil || last.Content != "Noon" || calls != 2 {
		t.Fatalf("answer = %+v, calls = %d, error = %v", last, calls, err)
	}
}

func TestChatParametersMalformedEndpointIsRedacted(t *testing.T) {
	c := Client{BaseURL: "https://api.anthropic.com/%zz?secret=fixture-secret", ChatParameters: map[string]any{"temperature": nil}}
	_, err := c.Complete(t.Context(), nil, true)
	if err == nil || err.Error() != "invalid LLM endpoint" {
		t.Fatalf("malformed endpoint error = %v", err)
	}
}
