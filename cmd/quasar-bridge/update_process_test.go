package main

import (
	"encoding/json/v2"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

func TestUpdateToolsProcess(t *testing.T) {
	if path := os.Getenv("QUASAR_UPDATE_TEST_CONFIG"); path != "" {
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", path, "-query", "спроси нейросеть: обнови Quasar Bridge"}
		main()
		return
	}
	for _, apiName := range []string{"chat_completions", "responses"} {
		t.Run(apiName, func(t *testing.T) {
			calls := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request struct {
					Tools []struct {
						Name     string       `json:"name"`
						Function llm.Function `json:"function"`
					} `json:"tools"`
					Messages []llm.Message `json:"messages"`
					Input    []struct {
						Type   string `json:"type"`
						Output string `json:"output"`
					} `json:"input"`
				}
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if calls == 1 {
					found := map[string]bool{}
					for _, tool := range request.Tools {
						name := tool.Name
						if name == "" {
							name = tool.Function.Name
						}
						found[name] = true
					}
					if !found["check_quasar_bridge_update"] || !found["install_quasar_bridge_update"] {
						t.Errorf("missing update tools: %v", found)
					}
					// A URL argument must be rejected before any networking or installation.
					if apiName == "responses" {
						json.MarshalWrite(w, map[string]any{"status": "completed", "output": []any{map[string]any{"type": "function_call", "call_id": "update-1", "name": "install_quasar_bridge_update", "arguments": `{"url":"https://example.org/evil"}`}}})
					} else {
						json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "update-1", Type: "function", Function: llm.FunctionCall{Name: "install_quasar_bridge_update", Arguments: `{"url":"https://example.org/evil"}`}}}}, "finish_reason": "tool_calls"}}})
					}
					return
				}
				output := ""
				if apiName == "responses" {
					output = request.Input[len(request.Input)-1].Output
				} else {
					output = request.Messages[len(request.Messages)-1].Content
				}
				if !strings.Contains(output, `"error"`) {
					t.Errorf("invalid update call succeeded: %s", output)
				}
				if apiName == "responses" {
					json.MarshalWrite(w, map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Обновление не запущено"}}}}})
				} else {
					json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": llm.Message{Role: "assistant", Content: "Обновление не запущено"}, "finish_reason": "stop"}}})
				}
			}))
			defer api.Close()
			body, _ := json.Marshal(map[string]any{"mode": "hybrid", "llm_api": apiName, "llm_base_url": api.URL + "/v1", "llm_model": "fixture", "assistant_tools": map[string]any{"enabled": true, "web": false, "history": false, "time": false}})
			path := configFile(t, string(body))
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUpdateToolsProcess$")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "QUASAR_") {
					child.Env = append(child.Env, entry)
				}
			}
			child.Env = append(child.Env, "QUASAR_HOME_MCP_URL=", "QUASAR_UPDATE_TEST_CONFIG="+path)
			output, err := child.CombinedOutput()
			if err != nil || !strings.Contains(string(output), "Обновление не запущено") || calls != 2 {
				t.Fatalf("CLI update tool flow: %v, calls=%d, %s", err, calls, output)
			}
		})
	}
}
