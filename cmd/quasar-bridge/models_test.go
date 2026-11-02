package main

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChatGPTAuthenticationCanRunBeforeModelSelection(t *testing.T) {
	for _, api := range []string{"chatgpt", "responses"} {
		t.Run(api, func(t *testing.T) {
			path := configFile(t, `{"mode":"hybrid","llm_api":"`+api+`","llm_base_url":"https://api.openai.com/v1"}`)
			cfg, err := readConfigForCommand(path, true)
			if err != nil || cfg.LLMModel != "" {
				t.Fatalf("cannot prepare model discovery: %v", err)
			}
			if _, err := readConfig(path); err == nil {
				t.Fatal("normal service accepted an unselected model")
			}
		})
	}
	path := configFile(t, `{"mode":"hybrid","llm_api":"chatgpt","llm_base_url":"https://other.example.invalid/v1"}`)
	if _, err := readConfigForCommand(path, true); err == nil {
		t.Fatal("discovery skipped endpoint validation")
	}
	// A real status command also works before selection, without a backend call.
	var out strings.Builder
	if err := runChatGPTAuth(t.Context(), "status", filepath.Join(t.TempDir(), "account.json"), "", &out); err != nil || !strings.Contains(out.String(), `"signed_in":false`) {
		t.Fatalf("pre-selection status: %q, %v", out.String(), err)
	}
}

func TestListModelsProcessBeforeModelSelection(t *testing.T) {
	if path := os.Getenv("QUASAR_TEST_MODELS_CONFIG"); path != "" {
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", path, "-list-models"}
		if os.Getenv("QUASAR_TEST_MODELS_CONFLICT") != "" {
			os.Args = append(os.Args, "-query", "must not run")
		}
		main()
		os.Exit(0)
	}
	for _, tc := range []struct {
		name, body, want string
		conflict         bool
		wantExit         int
	}{
		{"catalog", `{"data":[{"id":"first","name":"First model"},{"id":"second"}]}`, "first\t\"First model\"\nsecond\t\"second\"\n", false, 0},
		{"empty catalog", `{"data":[]}`, "", false, 1},
		{"bad response", `{"error":{"message":"private fixture response"}}`, "", false, 1},
		{"exclusive diagnostics", `{"data":[{"id":"first"}]}`, "", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer catalog-fixture" {
					t.Errorf("unexpected catalog request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer api.Close()
			path := configFile(t, `{"mode":"hybrid","llm_base_url":"`+api.URL+`/v1"}`)
			if _, err := readConfig(path); err == nil {
				t.Fatal("normal service configuration accepted an unselected model")
			}
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestListModelsProcessBeforeModelSelection$")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "QUASAR_") {
					child.Env = append(child.Env, entry)
				}
			}
			child.Env = append(child.Env, "QUASAR_TEST_MODELS_CONFIG="+path, "QUASAR_LLM_API_KEY=catalog-fixture")
			if tc.conflict {
				child.Env = append(child.Env, "QUASAR_TEST_MODELS_CONFLICT=1")
			}
			var stdout, stderr strings.Builder
			child.Stdout, child.Stderr = &stdout, &stderr
			err := child.Run()
			if child.ProcessState.ExitCode() != tc.wantExit || stdout.String() != tc.want {
				t.Fatalf("model listing: exit=%d error=%v stdout=%q stderr=%q", child.ProcessState.ExitCode(), err, stdout.String(), stderr.String())
			}
			wantCalls := int64(1)
			if tc.conflict {
				wantCalls = 0
			}
			if calls.Load() != wantCalls || strings.Contains(stderr.String(), "private fixture response") {
				t.Fatalf("catalog requests=%d, stderr=%q", calls.Load(), stderr.String())
			}
		})
	}
}
