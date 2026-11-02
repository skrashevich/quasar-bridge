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

func TestRoutingToolsProcess(t *testing.T) {
	if os.Getenv("QUASAR_ROUTING_TOOLS_CHILD") == "1" {
		os.Args = []string{"quasar-bridge", "-config", os.Getenv("QUASAR_ROUTING_TOOLS_PATH"), "-query", "Запросы о погоде отправляй нейросети"}
		main()
		return
	}
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			return
		}
		last := request.Messages[len(request.Messages)-1]
		message := llm.Message{Role: "assistant"}
		switch calls.Add(1) {
		case 1:
			var state struct {
				SavedRevision string `json:"saved_revision"`
			}
			if last.Role != "tool" || json.Unmarshal([]byte(last.Content), &state) != nil || state.SavedRevision == "" {
				t.Errorf("routing snapshot not prefetched: %+v", last)
				return
			}
			args, _ := json.Marshal(map[string]any{"expected_revision": state.SavedRevision, "rule": map[string]any{"name": "voice_weather", "route": "llm", "match": map[string]any{"contains_any": []string{"погода"}}}})
			message.ToolCalls = []llm.ToolCall{{ID: "route-set", Type: "function", Function: llm.FunctionCall{Name: "set_quasar_bridge_routing_rule", Arguments: string(args)}}}
		case 2:
			if !strings.Contains(last.Content, `"status":"applied"`) {
				t.Errorf("rule was not applied: %s", last.Content)
			}
			message.ToolCalls = []llm.ToolCall{{ID: "route-test", Type: "function", Function: llm.FunctionCall{Name: "test_quasar_bridge_routing", Arguments: `{"text":"погода завтра"}`}}}
		case 3:
			var preview struct {
				Active struct {
					Route string `json:"route"`
					Rule  string `json:"rule"`
				} `json:"active"`
			}
			if json.Unmarshal([]byte(last.Content), &preview) != nil || preview.Active.Route != "llm" || preview.Active.Rule != "voice_weather" {
				t.Errorf("wrong active rule: %s", last.Content)
			}
			message.Content = "Правило применено."
		default:
			t.Error("unexpected model request")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": message}}})
	}))
	defer api.Close()
	path := configFile(t, fmt.Sprintf(`{"mode":"hybrid","airplay":{"disabled":true},"llm_base_url":%q,"llm_model":"fixture","assistant_tools":{"enabled":true,"time":false},"routing":{"rules":[{"name":"weather","route":"yandex","match":{"contains_any":["погода"]}}],"default_route":"yandex"}}`, api.URL))
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRoutingToolsProcess$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "QUASAR_") {
			child.Env = append(child.Env, entry)
		}
	}
	child.Env = append(child.Env, "QUASAR_ROUTING_TOOLS_CHILD=1", "QUASAR_ROUTING_TOOLS_PATH="+path)
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err != nil {
		t.Fatalf("CLI routing flow failed: %v %s", err, stderr.String())
	}
	if calls.Load() != 3 || !strings.Contains(stdout.String(), "Правило применено") {
		t.Fatalf("incomplete routing tool flow: %d %s", calls.Load(), stdout.String())
	}
	cfg, err := readConfig(path)
	if err != nil || len(cfg.Routing.Rules) != 2 || cfg.Routing.Rules[0].Name != "voice_weather" || cfg.Routing.Rules[1].Name != "weather" {
		t.Fatalf("rule/neighbor not persisted: %+v %v", cfg.Routing, err)
	}
}
