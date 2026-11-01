package main

import (
	"encoding/json/v2"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/llm"
)

func TestConfiguredPromptsProcess(t *testing.T) {
	if path := os.Getenv("QUASAR_PROMPTS_TEST_CONFIG"); path != "" {
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", path, "-query", "спроси нейросеть: расскажи о звёздах"}
		main()
		return
	}
	for _, api := range []string{"chat_completions", "responses"} {
		for _, enabled := range []bool{false, true} {
			for _, tc := range []struct {
				name, system, tools string
			}{
				{name: "custom", system: "Answer in English.\nExplain in detail.", tools: "Use tools only when necessary."},
				{name: "custom_general", system: "Answer in English.\nExplain in detail."},
			} {
				t.Run(api+"/tools_"+map[bool]string{false: "off", true: "on"}[enabled]+"/"+tc.name, func(t *testing.T) {
					var calls atomic.Int64
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						var request struct {
							Messages []llm.Message `json:"messages"`
							Input    []llm.Message `json:"input"`
						}
						if err := json.UnmarshalRead(r.Body, &request); err != nil {
							t.Error(err)
							return
						}
						messages := request.Messages
						if api == "responses" {
							messages = request.Input
						}
						if len(messages) != 2 || messages[0].Role != "system" || messages[1].Role != "user" {
							t.Errorf("expected one initial system message and user question: %+v", messages)
							return
						}
						prompt := messages[0].Content
						if !strings.Contains(prompt, tc.system) || strings.Contains(prompt, "Отвечай по-русски") || strings.Contains(prompt, "Отвечай кратко по-русски") {
							t.Errorf("custom system prompt was lost or conflicts with default style: %q", prompt)
						}
						if enabled {
							if strings.TrimSpace(tc.tools) == "" {
								if !strings.Contains(prompt, "Используй доступные инструменты") {
									t.Errorf("default tool prompt missing: %q", prompt)
								}
							} else if !strings.Contains(prompt, tc.tools) || strings.Contains(prompt, "Используй доступные инструменты") {
								t.Errorf("custom tool prompt did not replace default: %q", prompt)
							}
						} else if strings.Contains(prompt, "Используй доступные инструменты") || (tc.tools != "" && strings.Contains(prompt, tc.tools)) {
							t.Errorf("disabled tool prompt reached API: %q", prompt)
						}
						w.Header().Set("Content-Type", "application/json")
						if api == "responses" {
							json.MarshalWrite(w, map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Configured answer"}}}}})
						} else {
							json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": llm.Message{Role: "assistant", Content: "Configured answer"}, "finish_reason": "stop"}}})
						}
					}))
					defer server.Close()
					body, err := json.Marshal(map[string]any{"mode": "hybrid", "airplay": map[string]bool{"disabled": true}, "llm_api": api, "llm_base_url": server.URL + "/v1", "llm_model": "fixture", "llm_system_prompt": tc.system, "assistant_tools": map[string]any{"enabled": enabled, "system_prompt": tc.tools, "web": false, "history": false, "time": false, "updates": false}})
					if err != nil {
						t.Fatal(err)
					}
					child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConfiguredPromptsProcess$")
					for _, entry := range os.Environ() {
						if !strings.HasPrefix(entry, "QUASAR_") {
							child.Env = append(child.Env, entry)
						}
					}
					child.Env = append(child.Env, "QUASAR_HOME_MCP_URL=", "QUASAR_PROMPTS_TEST_CONFIG="+configFile(t, string(body)))
					output, err := child.CombinedOutput()
					if err != nil || !strings.Contains(string(output), "Configured answer") || calls.Load() != 1 {
						t.Fatalf("configured prompt query failed: %v, calls=%d, %s", err, calls.Load(), output)
					}
				})
			}
		}
	}
}

func TestReadConfigSystemPromptEnvironmentOverride(t *testing.T) {
	t.Setenv("QUASAR_LLM_SYSTEM_PROMPT", "Answer in English.\nBe detailed.")
	cfg, err := readConfig(configFile(t, `{"llm_system_prompt":"previous"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMSystemPrompt != "Answer in English.\nBe detailed." {
		t.Fatalf("system prompt env override lost: %q", cfg.LLMSystemPrompt)
	}
}

func TestReloadPromptsRequiresRestart(t *testing.T) {
	for _, body := range []string{`{"llm_system_prompt":"custom"}`, `{"assistant_tools":{"system_prompt":"custom"}}`} {
		path := configFile(t, `{}`)
		initial, err := readConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := reloadRouting(path, initial, new(bridge.Engine)); err == nil || !strings.Contains(err.Error(), "restart required") {
			t.Fatalf("changed prompt did not require restart: %v", err)
		}
	}
}
