package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"quasarbridge/internal/bridge"
)

func TestChatGPTConfigDefaultsAndOverrides(t *testing.T) {
	t.Setenv("QUASAR_HOME_MCP_URL", "")
	path := configFile(t, `{"mode":"hybrid","llm_api":"chatgpt","llm_model":"account-model","llm_reasoning_effort":"low","llm_hosted_tools":["web_search"]}`)
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMBaseURL != "https://api.openai.com/v1" {
		t.Fatalf("subscription endpoint = %q", cfg.LLMBaseURL)
	}
	if cfg.LLMChatGPTAuthFile == "" {
		t.Fatal("missing protected credential path")
	}
	t.Setenv("QUASAR_LLM_CHATGPT_AUTH_FILE", filepath.Join(t.TempDir(), "account.json"))
	cfg, err = readConfig(path)
	if err != nil || cfg.LLMChatGPTAuthFile != os.Getenv("QUASAR_LLM_CHATGPT_AUTH_FILE") {
		t.Fatalf("credential path override: %q, %v", cfg.LLMChatGPTAuthFile, err)
	}
}

func TestChatGPTConfigRejectsUnsupportedSettings(t *testing.T) {
	for _, settings := range []string{
		`"llm_base_url":"https://other.example.invalid/v1"`,
		`"llm_base_url":"http://api.openai.com/v1"`,
		`"llm_base_url":"https://api.openai.com/v1?key=private"`,
		`"llm_base_url":"https://api.openai.com/v1/responses"`,
		`"llm_hosted_tools":["code_interpreter"]`,
		`"llm_chat_parameters":{"temperature":null}`,
	} {
		t.Run(settings, func(t *testing.T) {
			if _, err := readConfig(configFile(t, `{"llm_api":"chatgpt",`+settings+`}`)); err == nil {
				t.Fatal("unsupported ChatGPT settings accepted")
			}
		})
	}
	if _, err := readConfig(configFile(t, `{"mode":"hybrid","llm_api":"chatgpt"}`)); err == nil {
		t.Fatal("missing account model accepted")
	}
}

func TestChatGPTCredentialsChangeRequiresRestart(t *testing.T) {
	path := configFile(t, `{"llm_api":"chatgpt","llm_chatgpt_auth_file":"first.json"}`)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"llm_api":"chatgpt","llm_chatgpt_auth_file":"second.json"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reloadRouting(path, initial, new(bridge.Engine)); err == nil || !strings.Contains(err.Error(), "restart required") {
		t.Fatalf("changed account silently reloaded: %v", err)
	}
}
