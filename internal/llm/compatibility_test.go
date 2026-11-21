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
	"testing"
)

func writeCompatibilityAnswer(w http.ResponseWriter, body map[string]any) {
	content := "ok"
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if text, _ := message["content"].(string); strings.Contains(text, "JSON") {
			content = `{"ok":true}`
		}
	}
	_ = json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
}

func TestProbeCompatibilityPreservesDefaultsAndChecksJSON(t *testing.T) {
	calls := 0
	c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		calls++
		body := readChatRequest(t, r)
		if body["chat_template_kwargs"] == nil || body["temperature"] != float64(0) || body["max_tokens"] != float64(1024) {
			t.Errorf("defaults changed: %v", body)
		}
		if calls == 2 && !reflect.DeepEqual(body["response_format"], map[string]any{"type": "json_object"}) {
			t.Errorf("structured request absent: %v", body)
		}
		writeCompatibilityAnswer(w, body)
	})
	parameters, err := c.ProbeCompatibility(t.Context(), nil)
	if err != nil || len(parameters) != 0 || calls != 2 {
		t.Fatalf("parameters=%v error=%v calls=%d", parameters, err, calls)
	}
}

func TestProbeCompatibilityOmitsGeminiTemplateParameter(t *testing.T) {
	calls := 0
	c := endpointTestClient(t, "https://generativelanguage.googleapis.com/v1beta/openai", func(w http.ResponseWriter, r *http.Request) {
		calls++
		body := readChatRequest(t, r)
		if _, present := body["chat_template_kwargs"]; present {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"Unknown name \"chat_template_kwargs\": Cannot find field.","status":"INVALID_ARGUMENT"}}`)
			return
		}
		if body["temperature"] != float64(0) || body["max_tokens"] != float64(1024) {
			t.Errorf("unnecessary defaults removed: %v", body)
		}
		writeCompatibilityAnswer(w, body)
	})
	parameters, err := c.ProbeCompatibility(t.Context(), nil)
	if err != nil || !reflect.DeepEqual(parameters, map[string]any{"chat_template_kwargs": nil}) || calls != 3 {
		t.Fatalf("parameters=%v error=%v calls=%d", parameters, err, calls)
	}
}

func TestProbeCompatibilityHandlesGoogleErrorArrays(t *testing.T) {
	const parameterBody = `[{"error":{"code":400,"message":"Invalid JSON payload received. Unknown name \"chat_template_kwargs\": Cannot find field.","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"description":"Invalid JSON payload received. Unknown name \"chat_template_kwargs\": Cannot find field."}]}]}}]`
	const regionBody = `[{"error":{"code":400,"message":"User location is not supported for the API use.","status":"FAILED_PRECONDITION"}}]`
	for _, tc := range []struct {
		name        string
		body        string
		wantSuccess bool
	}{
		{"parameter", parameterBody, true},
		{"region", regionBody, false},
		{"mixed region", `[{"error":{"param":"temperature","message":"Unsupported parameter"}},` + regionBody[1:], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
				calls++
				request := readChatRequest(t, r)
				if request["chat_template_kwargs"] != nil || !tc.wantSuccess {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				writeCompatibilityAnswer(w, request)
			})
			reports := 0
			parameters, err := c.ProbeCompatibility(t.Context(), func(err error) {
				reports++
				failure, ok := errors.AsType[*HTTPError](err)
				if !ok || failure.Body != tc.body {
					t.Errorf("Google error array was not preserved: %v", err)
				}
			})
			if tc.wantSuccess {
				if err != nil || calls != 3 || reports != 1 || !reflect.DeepEqual(parameters, map[string]any{"chat_template_kwargs": nil}) {
					t.Fatalf("parameters=%v error=%v calls=%d reports=%d", parameters, err, calls, reports)
				}
			} else if err == nil || calls != 1 || reports != 1 {
				t.Fatalf("parameters=%v error=%v calls=%d reports=%d", parameters, err, calls, reports)
			}
		})
	}
}

func TestProbeCompatibilityCombinesOmissionsAndKeepsOriginal(t *testing.T) {
	initial := map[string]any{"reasoning_effort": "low", "temperature": 0.7}
	before, _ := json.Marshal(initial, json.Deterministic(true))
	c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		body := readChatRequest(t, r)
		for _, key := range []string{"chat_template_kwargs", "temperature", "response_format"} {
			if _, present := body[key]; present {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.MarshalWrite(w, map[string]any{"error": map[string]any{"param": key, "message": "Unsupported parameter"}})
				return
			}
		}
		if body["reasoning_effort"] != "low" {
			t.Errorf("custom parameter lost: %v", body)
		}
		writeCompatibilityAnswer(w, body)
	})
	c.ChatParameters = initial
	parameters, err := c.ProbeCompatibility(t.Context(), nil)
	if err != nil || !reflect.DeepEqual(parameters, map[string]any{"chat_template_kwargs": nil, "temperature": nil, "response_format": nil, "reasoning_effort": "low"}) {
		t.Fatalf("parameters=%v error=%v", parameters, err)
	}
	after, _ := json.Marshal(initial, json.Deterministic(true))
	if string(before) != string(after) {
		t.Fatalf("original mutated: %s -> %s", before, after)
	}
}

func TestProbeCompatibilityReplacesTokenParameterWithoutRemovingOthers(t *testing.T) {
	c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		body := readChatRequest(t, r)
		if _, present := body["max_tokens"]; present {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"param":"max_tokens","code":"unsupported_parameter","message":"Use max_completion_tokens instead."}}`)
			return
		}
		if body["max_completion_tokens"] != float64(2048) || body["chat_template_kwargs"] == nil || body["temperature"] != float64(0) {
			t.Errorf("wrong token replacement: %v", body)
		}
		writeCompatibilityAnswer(w, body)
	})
	c.ChatParameters = map[string]any{"max_tokens": 2048}
	parameters, err := c.ProbeCompatibility(t.Context(), nil)
	if err != nil || !reflect.DeepEqual(parameters, map[string]any{"max_tokens": nil, "max_completion_tokens": 2048}) {
		t.Fatalf("parameters=%v error=%v", parameters, err)
	}
}

func TestProbeCompatibilityCanOmitAllTokenLimits(t *testing.T) {
	c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		body := readChatRequest(t, r)
		for _, key := range []string{"max_tokens", "max_completion_tokens"} {
			if _, present := body[key]; present {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.MarshalWrite(w, map[string]any{"error": map[string]any{"param": key, "code": "unsupported_parameter"}})
				return
			}
		}
		writeCompatibilityAnswer(w, body)
	})
	parameters, err := c.ProbeCompatibility(t.Context(), nil)
	if err != nil || !reflect.DeepEqual(parameters, map[string]any{"max_tokens": nil, "max_completion_tokens": nil}) {
		t.Fatalf("parameters=%v error=%v", parameters, err)
	}
}

func TestProbeCompatibilityStopsNonParameterErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"auth", 401, `{"error":{"message":"secret key"}}`},
		{"forbidden", 403, `{}`},
		{"quota", 429, `{}`},
		{"unavailable", 503, `{}`},
		{"region", 400, `{"error":{"status":"FAILED_PRECONDITION","message":"User location is not supported for the API use. secret"}}`},
		{"invalid key", 400, `{"error":{"status":"INVALID_ARGUMENT","message":"API key not valid. Please pass a valid API key. secret"}}`},
		{"billing", 400, `{"error":{"message":"Billing must be enabled. secret"}}`},
		{"quota400", 400, `{"error":{"code":"insufficient_quota","message":"secret"}}`},
		{"model", 400, `{"error":{"param":"model","message":"The model is not supported. secret"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			})
			parameters, err := c.ProbeCompatibility(t.Context(), nil)
			if err == nil || parameters != nil || calls != 1 || strings.Contains(err.Error(), "secret") {
				t.Fatalf("parameters=%v error=%v calls=%d", parameters, err, calls)
			}
		})
	}
}

func TestProbeCompatibilityRejectsInvalidJSONWithoutFurtherRequests(t *testing.T) {
	for _, answer := range []string{"not json", `[]`, `null`, `"text"`} {
		t.Run(answer, func(t *testing.T) {
			calls := 0
			c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
				calls++
				_ = readChatRequest(t, r)
				_ = json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": answer}}}})
			})
			parameters, err := c.ProbeCompatibility(t.Context(), nil)
			if err == nil || parameters != nil || calls != 2 {
				t.Fatalf("parameters=%v error=%v calls=%d", parameters, err, calls)
			}
		})
	}
}

func TestProbeCompatibilityStopsTransportAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"transport", errors.New("secret transport")},
		{"deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := Client{BaseURL: "https://secret.invalid", HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, tc.err })}}
			parameters, err := c.ProbeCompatibility(t.Context(), nil)
			if err == nil || parameters != nil || calls != 1 || strings.Contains(err.Error(), "secret") {
				t.Fatalf("parameters=%v error=%v calls=%d", parameters, err, calls)
			}
		})
	}
}

func TestProbeCompatibilityExhaustionIsBounded(t *testing.T) {
	calls := 0
	c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = readChatRequest(t, r)
		w.WriteHeader(http.StatusBadRequest)
	})
	parameters, err := c.ProbeCompatibility(t.Context(), nil)
	if err == nil || parameters != nil || calls < 2 || calls > 10 {
		t.Fatalf("parameters=%v error=%v calls=%d", parameters, err, calls)
	}
}

func TestProbeCompatibilityRejectsOtherAPIsBeforeHTTP(t *testing.T) {
	for _, api := range []string{"responses", "chatgpt"} {
		t.Run(api, func(t *testing.T) {
			c := Client{API: api, HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				t.Error("unexpected HTTP")
				return nil, errors.New("unexpected HTTP")
			})}}
			if _, err := c.ProbeCompatibility(t.Context(), nil); err == nil {
				t.Fatal("unsupported API accepted")
			}
		})
	}
}

func TestProbeCompatibilityReportsEachFullProviderError(t *testing.T) {
	const body = `{"error":{"message":"Unsupported parameter temperature; secret upstream context","param":"temperature"}}`
	c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		request := readChatRequest(t, r)
		if request["temperature"] != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, body)
			return
		}
		writeCompatibilityAnswer(w, request)
	})
	reports := 0
	parameters, err := c.ProbeCompatibility(t.Context(), func(err error) {
		reports++
		failure, ok := errors.AsType[*HTTPError](err)
		if !ok || failure.Body != body || strings.Contains(err.Error(), "secret") {
			t.Errorf("unsafe or incomplete error: %v", err)
		}
	})
	if err != nil || reports != 1 || !reflect.DeepEqual(parameters, map[string]any{"temperature": nil}) {
		t.Fatalf("parameters=%v error=%v reports=%d", parameters, err, reports)
	}
}

func TestProviderHTTPErrorPreservesLongBodiesAndMarksOverflow(t *testing.T) {
	for _, size := range []int{20000, (2 << 20) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := strings.Repeat("x", size)
			failure := llmHTTPError(&http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(body))}).(*HTTPError)
			if size <= 2<<20 {
				if failure.Body != body {
					t.Fatalf("error body lost: got %d, want %d", len(failure.Body), size)
				}
			} else if !strings.HasPrefix(failure.Body, body[:2<<20]) || !strings.Contains(failure.Body, "remaining body omitted") {
				t.Fatal("oversized error silently truncated")
			}
		})
	}
}

func TestModelCatalogPreservesProviderErrorBody(t *testing.T) {
	const body = `{"error":{"message":"secret invalid credentials"}}`
	c := Client{BaseURL: "https://provider.example.invalid", HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	_, err := c.Models(t.Context())
	failure, ok := errors.AsType[*HTTPError](err)
	if !ok || failure.Body != body || strings.Contains(err.Error(), "secret") {
		t.Fatalf("catalog error body missing or exposed: %v", err)
	}
}

func TestProbeCompatibilitySelectsFirstMentionedRejectedTokenParameter(t *testing.T) {
	c := endpointTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		request := readChatRequest(t, r)
		if _, present := request["max_tokens"]; present {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter max_tokens. Use max_completion_tokens instead."}}`)
			return
		}
		writeCompatibilityAnswer(w, request)
	})
	parameters, err := c.ProbeCompatibility(t.Context(), nil)
	if err != nil || !reflect.DeepEqual(parameters, map[string]any{"max_tokens": nil, "max_completion_tokens": 1024}) {
		t.Fatalf("parameters=%v error=%v", parameters, err)
	}
}
