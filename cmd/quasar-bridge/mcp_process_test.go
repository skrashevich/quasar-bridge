package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"quasarbridge/internal/llm"
)

func TestCustomMCPProcess(t *testing.T) {
	if action := os.Getenv("QUASAR_MCP_CHECK_CHILD"); action != "" {
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", os.Getenv("QUASAR_MCP_CHECK_CONFIG"), "-" + action}
		if action == "query" {
			os.Args = append(os.Args, "спроси нейросеть: прочитай данные")
		}
		main()
		return
	}
	for _, apiName := range []string{"chat_completions", "responses"} {
		t.Run(apiName, func(t *testing.T) { testCustomMCPProcess(t, apiName, nil, false) })
		t.Run(apiName+"/airplay_disabled", func(t *testing.T) { testCustomMCPProcess(t, apiName, nil, true) })
	}
	t.Run("chat_parameters", func(t *testing.T) {
		testCustomMCPProcess(t, "chat_completions", map[string]any{"temperature": nil, "response_format": nil, "chat_template_kwargs": nil}, false)
	})
}

func testCustomMCPProcess(t *testing.T, apiName string, chatParameters map[string]any, airPlayDisabled bool) {
	t.Helper()
	var toolCalls, llmCalls atomic.Int64
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		toolCalls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "through MCP"}}}, nil
	})
	mcpAPI := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer mcpAPI.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiName == "responses" {
			if r.URL.Path != "/v1/responses" {
				t.Errorf("Responses endpoint = %q", r.URL.Path)
			}
			var request struct {
				Input []struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Output string `json:"output"`
				} `json:"input"`
				Tools []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"tools"`
			}
			if err := json.UnmarshalRead(r.Body, &request); err != nil {
				t.Error(err)
				return
			}
			var output []any
			if llmCalls.Add(1) == 1 {
				tools := make([]llm.Tool, 0, len(request.Tools))
				for _, tool := range request.Tools {
					tools = append(tools, llm.Tool{Type: tool.Type, Function: llm.Function{Name: tool.Name}})
				}
				name := mcpProcessTool(t, tools, airPlayDisabled)
				if name == "" {
					return
				}
				output = []any{map[string]any{"id": "fc-mcp-1", "type": "function_call", "status": "completed", "call_id": "mcp-1", "name": name, "arguments": `{}`}}
			} else {
				if len(request.Input) == 0 {
					t.Error("Responses input missing")
					return
				}
				last := request.Input[len(request.Input)-1]
				if last.Type != "function_call_output" || last.CallID != "mcp-1" || !strings.Contains(last.Output, "through MCP") {
					t.Errorf("MCP output did not return to Responses API: %+v", last)
				}
				output = []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Ответ получен"}}}}
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.MarshalWrite(w, map[string]any{"status": "completed", "output": output}); err != nil {
				t.Error(err)
			}
			return
		}
		var request struct {
			Messages           []llm.Message  `json:"messages"`
			Tools              []llm.Tool     `json:"tools"`
			Temperature        jsontext.Value `json:"temperature"`
			ResponseFormat     jsontext.Value `json:"response_format"`
			ChatTemplateKwargs jsontext.Value `json:"chat_template_kwargs"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			return
		}
		if chatParameters != nil && (len(request.Temperature) > 0 || len(request.ResponseFormat) > 0 || len(request.ChatTemplateKwargs) > 0) {
			t.Error("configured chat parameter removal did not reach the API")
			http.Error(w, "unsupported parameter", http.StatusBadRequest)
			return
		}
		var message llm.Message
		if llmCalls.Add(1) == 1 {
			name := mcpProcessTool(t, request.Tools, airPlayDisabled)
			if name == "" {
				return
			}
			message.ToolCalls = []llm.ToolCall{{ID: "mcp-1", Type: "function", Function: llm.FunctionCall{Name: name, Arguments: `{}`}}}
		} else {
			if !strings.Contains(request.Messages[len(request.Messages)-1].Content, "through MCP") {
				t.Error("MCP output did not return to LLM")
			}
			message.Content = "Ответ получен"
		}
		body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": "stop"}}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer api.Close()
	body, _ := json.Marshal(map[string]any{"mode": "hybrid", "airplay": map[string]any{"disabled": airPlayDisabled}, "llm_api": apiName, "llm_chat_parameters": chatParameters, "llm_base_url": api.URL + "/v1", "llm_model": "fixture", "assistant_tools": map[string]any{"enabled": true, "web": false, "history": false, "time": false, "updates": false, "mcp_servers": []any{map[string]any{"name": "fixture", "url": mcpAPI.URL}}}})
	path := configFile(t, string(body))
	for _, action := range []string{"check-mcp", "query"} {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCustomMCPProcess$")
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "QUASAR_") {
				child.Env = append(child.Env, entry)
			}
		}
		child.Env = append(child.Env, "QUASAR_HOME_MCP_URL=", "QUASAR_MCP_CHECK_CHILD="+action, "QUASAR_MCP_CHECK_CONFIG="+path)
		var stdout, stderr bytes.Buffer
		child.Stdout, child.Stderr = &stdout, &stderr
		err := child.Run()
		cancel()
		if err != nil {
			t.Fatalf("%s failed: %v %s", action, err, stderr.String())
		}
		if action == "check-mcp" {
			if !strings.Contains(stdout.String(), `"tool_names":["echo"]`) || toolCalls.Load() != 0 || llmCalls.Load() != 0 {
				t.Fatal("MCP discovery diagnostic executed an action", stdout.String())
			}
		} else if !strings.Contains(stdout.String(), "Ответ получен") || toolCalls.Load() != 1 || llmCalls.Load() != 2 {
			t.Fatal("CLI → LLM → MCP → LLM failed", stdout.String())
		}
	}
}

// Inspect both API catalogs by name, independently of built-in/MCP ordering.
func mcpProcessTool(t *testing.T, tools []llm.Tool, airPlayDisabled bool) string {
	t.Helper()
	expected := map[string]bool{}
	if !airPlayDisabled {
		for _, name := range []string{"get_airplay_status", "control_airplay", "control_airplay_playback", "set_airplay_volume"} {
			expected[name] = true
		}
	}
	if len(tools) != len(expected)+1 {
		t.Errorf("unexpected tool count with airplay.disabled=%v: %+v", airPlayDisabled, tools)
		return ""
	}
	var mcpName string
	for _, tool := range tools {
		name := tool.Function.Name
		if tool.Type != "function" {
			t.Errorf("non-function tool reached API: %+v", tool)
			return ""
		}
		if strings.HasPrefix(name, "mcp__fixture__") && mcpName == "" {
			mcpName = name
		} else if expected[name] {
			delete(expected, name)
		} else {
			t.Errorf("unexpected or duplicate tool reached API: %q", name)
			return ""
		}
	}
	if mcpName == "" || len(expected) != 0 {
		t.Errorf("missing MCP or AirPlay tools: MCP=%q missing=%v", mcpName, expected)
		return ""
	}
	return mcpName
}
