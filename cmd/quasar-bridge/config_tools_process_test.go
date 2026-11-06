package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"quasarbridge/internal/llm"
)

// Removing tool registration, persistence or reload breaks the observable flow.
func TestConfigurationToolsProcess(t *testing.T) {
	if os.Getenv("QUASAR_CONFIG_TOOLS_CHILD") == "1" {
		os.Args = []string{"quasar-bridge", "-config", os.Getenv("QUASAR_CONFIG_TOOLS_PATH"), "-query", "Измени настройки квазар бридж"}
		main()
		return
	}
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools    []llm.Tool    `json:"tools"`
			Messages []llm.Message `json:"messages"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			return
		}
		step := calls.Add(1)
		message := llm.Message{Role: "assistant"}
		switch step {
		case 1:
			found := map[string]bool{}
			for _, tool := range request.Tools {
				found[tool.Function.Name] = true
			}
			for _, name := range []string{"get_quasar_bridge_config", "set_quasar_bridge_config", "apply_quasar_bridge_config"} {
				if !found[name] {
					t.Errorf("missing configuration tool %s", name)
				}
			}
			message.ToolCalls = []llm.ToolCall{{ID: "set", Type: "function", Function: llm.FunctionCall{Name: "set_quasar_bridge_config", Arguments: `{"changes":{"routing.default_route":"llm","routing.rules":[]}}`}}}
		case 2:
			if result := request.Messages[len(request.Messages)-1].Content; !strings.Contains(result, `"status":"saved"`) {
				t.Errorf("configuration not saved: %s", result)
			}
			message.ToolCalls = []llm.ToolCall{{ID: "apply", Type: "function", Function: llm.FunctionCall{Name: "apply_quasar_bridge_config", Arguments: `{"action":"reload"}`}}}
		case 3:
			if result := request.Messages[len(request.Messages)-1].Content; !strings.Contains(result, `"status":"applied"`) {
				t.Errorf("configuration not applied: %s", result)
			}
			message.ToolCalls = []llm.ToolCall{{ID: "get", Type: "function", Function: llm.FunctionCall{Name: "get_quasar_bridge_config", Arguments: `{"section":"routing.default_route"}`}}}
		case 4:
			result := request.Messages[len(request.Messages)-1].Content
			if !strings.Contains(result, `"active":"llm"`) || !strings.Contains(result, `"saved":"llm"`) {
				t.Errorf("wrong active/saved configuration: %s", result)
			}
			message.Content = "Настройки применены."
		default:
			t.Error("unexpected LLM request")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": message}}})
	}))
	defer api.Close()
	path := configFile(t, fmt.Sprintf(`{"mode":"hybrid","airplay":{"disabled":true},"llm_base_url":%q,"llm_model":"fixture","assistant_tools":{"enabled":true,"time":false},"routing":{"rules":[],"default_route":"yandex"}}`, api.URL))
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConfigurationToolsProcess$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "QUASAR_") {
			child.Env = append(child.Env, entry)
		}
	}
	child.Env = append(child.Env, "QUASAR_CONFIG_TOOLS_CHILD=1", "QUASAR_CONFIG_TOOLS_PATH="+path)
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err != nil {
		t.Fatalf("query failed: %v %s", err, stderr.String())
	}
	if calls.Load() != 4 || !strings.Contains(stdout.String(), "Настройки применены") {
		t.Fatalf("incomplete tool flow: calls=%d output=%s", calls.Load(), stdout.String())
	}
	cfg, err := readConfig(path)
	if err != nil || cfg.Routing.DefaultRoute != "llm" || len(cfg.Routing.Rules) != 0 {
		t.Fatalf("configuration not persisted: %v %+v", err, cfg.Routing)
	}
}
