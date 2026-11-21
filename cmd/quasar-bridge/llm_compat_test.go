package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChatParametersEnvironmentOverride(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        map[string]any
		invalid     bool
	}{
		{"omit field", `{"chat_template_kwargs":null}`, map[string]any{"chat_template_kwargs": nil}, false},
		{"replace map", `{"max_tokens":null,"max_completion_tokens":2048}`, map[string]any{"max_tokens": nil, "max_completion_tokens": float64(2048)}, false},
		{"reset", `{}`, nil, false},
		{"null", `null`, nil, true},
		{"array", `[]`, nil, true},
		{"empty", ``, nil, true},
		{"malformed", `{"private-secret":`, nil, true},
		{"unsafe field", `{"model":"private-secret"}`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QUASAR_LLM_CHAT_PARAMETERS", tc.value)
			cfg, err := readConfig(configFile(t, `{"llm_chat_parameters":{"temperature":0.7}}`))
			if tc.invalid {
				if err == nil || strings.Contains(err.Error(), "private-secret") {
					t.Fatalf("invalid override error = %v", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(cfg.LLMChatParameters, tc.want) {
				t.Fatalf("parameters = %v, error = %v; want %v", cfg.LLMChatParameters, err, tc.want)
			}
		})
	}
}

func TestChatParametersEnvironmentOverrideCannotBeVoiceEdited(t *testing.T) {
	t.Setenv("QUASAR_LLM_CHAT_PARAMETERS", `{"chat_template_kwargs":null}`)
	c := configurationFixture(t, `{}`)
	_, err := c.Set(t.Context(), map[string]jsontext.Value{"llm_chat_parameters": jsontext.Value(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "QUASAR_LLM_CHAT_PARAMETERS") {
		t.Fatalf("ineffective voice edit accepted: %v", err)
	}
}

func TestProbeLLMCompatibilityProcess(t *testing.T) {
	if path := os.Getenv("QUASAR_TEST_COMPAT_CONFIG"); path != "" {
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", path, "-probe-llm-compat"}
		if os.Getenv("QUASAR_TEST_COMPAT_BACKEND_CHECK") != "" {
			os.Args[len(os.Args)-1] = "-check-backends"
		}
		if os.Getenv("QUASAR_TEST_COMPAT_CONFLICT") != "" {
			os.Args = append(os.Args, "-check-backends")
		}
		main()
		os.Exit(0)
	}
	for _, tc := range []struct {
		name, api string
		status    int
		conflict  bool
		wantExit  int
		check     bool
	}{
		{"compatible after omission", "chat_completions", 400, false, 0, false},
		{"unauthorized", "chat_completions", 401, false, 1, false},
		{"responses skipped", "responses", 400, false, 1, false},
		{"exclusive diagnostics", "chat_completions", 400, true, 2, false},
		{"full backend check failure", "chat_completions", 401, false, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer compat-fixture" {
					t.Errorf("unexpected compatibility request: %s %s", r.Method, r.URL.Path)
				}
				var request map[string]any
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
				}
				if _, exists := request["chat_template_kwargs"]; exists || tc.status == 401 {
					w.WriteHeader(tc.status)
					w.Write([]byte(`{"error":{"param":"chat_template_kwargs","message":"Unknown parameter chat_template_kwargs; private-fixture"}}`))
					return
				}
				w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}]}`))
			}))
			defer api.Close()
			path := configFile(t, `{"mode":"hybrid","llm_api":"`+tc.api+`","llm_base_url":"`+api.URL+`/v1","llm_model":"fixture"}`)
			original, _ := os.ReadFile(path)
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProbeLLMCompatibilityProcess$")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "QUASAR_") {
					child.Env = append(child.Env, entry)
				}
			}
			child.Env = append(child.Env, "QUASAR_TEST_COMPAT_CONFIG="+path, "QUASAR_LLM_API_KEY=compat-fixture")
			if tc.conflict {
				child.Env = append(child.Env, "QUASAR_TEST_COMPAT_CONFLICT=yes")
			}
			if tc.check {
				child.Env = append(child.Env, "QUASAR_TEST_COMPAT_BACKEND_CHECK=yes")
			}
			var stdout, stderr strings.Builder
			child.Stdout, child.Stderr = &stdout, &stderr
			err := child.Run()
			if child.ProcessState.ExitCode() != tc.wantExit {
				t.Fatalf("probe exit=%d error=%v stdout=%q stderr=%q", child.ProcessState.ExitCode(), err, stdout.String(), stderr.String())
			}
			if tc.wantExit == 0 {
				var parameters map[string]any
				if err := json.Unmarshal([]byte(stdout.String()), &parameters); err != nil || !reflect.DeepEqual(parameters, map[string]any{"chat_template_kwargs": nil}) {
					t.Fatalf("probe output=%q error=%v", stdout.String(), err)
				}
				if calls.Load() != 3 {
					t.Fatalf("probe requests=%d, want plain failure then plain and JSON success", calls.Load())
				}
			} else {
				wantCalls := int64(0)
				if tc.status == 401 {
					wantCalls = 1
				}
				if stdout.Len() != 0 || calls.Load() != wantCalls {
					t.Fatalf("failed probe output=%q requests=%d", stdout.String(), calls.Load())
				}
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(original) || strings.Contains(stderr.String(), "compat-fixture") {
				t.Fatalf("probe mutated configuration or leaked data: %s", stderr.String())
			}
			if calls.Load() > 0 && !strings.Contains(stderr.String(), `{"error":{"param":"chat_template_kwargs","message":"Unknown parameter chat_template_kwargs; private-fixture"}}`) {
				t.Fatalf("full provider error missing from diagnostics: %s", stderr.String())
			}
		})
	}
}
