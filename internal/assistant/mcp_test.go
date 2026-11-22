package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/skrashevich/quasar-bridge/internal/llm"
)

func fixtureMCP(calls *atomic.Int64) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	for _, name := range []string{"echo", "hidden", "failure", "needs_input"} {
		s.AddTool(&mcp.Tool{Name: name, Description: "Read a fixture value", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls.Add(1)
			if req.Params.Name == "needs_input" {
				return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{}}, nil
			}
			if req.Params.Name == "failure" {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "fixture failure"}}}, nil
			}
			var args map[string]any
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{StructuredContent: args, Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
		})
	}
	return s
}

func TestMCPInputRequestNeverAutomaticallyRetries(t *testing.T) {
	var calls atomic.Int64
	s := fixtureMCP(&calls)
	server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer server.Close()
	cfg := DefaultConfig()
	cfg.Web, cfg.History, cfg.Time = false, false, false
	cfg.MCPServers = []MCPServerConfig{{Name: "input", URL: server.URL, AllowedTools: []string{"needs_input"}}}
	tools, close := (&Tools{Config: cfg}).ForRequest(t.Context())
	defer close()
	defs := tools.Definitions()
	if len(defs) != 1 {
		t.Fatal("fixture unavailable")
	}
	for range 2 {
		_, err := tools.Call(t.Context(), defs[0].Function.Name, `{}`)
		if err == nil || !strings.Contains(err.Error(), "interactive input") {
			t.Fatalf("input request outcome lost: calls=%d err=%v", calls.Load(), err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("SDK replayed tools/call: %d handler invocations", calls.Load())
	}
}

func TestMCPTimeoutDoesNotReplayUnknownAction(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "slow", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &mcp.CallToolResult{}, nil
		}
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	defer httpServer.Close()
	defer close(release)
	cfg := DefaultConfig()
	cfg.MCPServers = []MCPServerConfig{{Name: "slow", URL: httpServer.URL, TimeoutMS: 1000}}
	tools, closeTools := (&Tools{Config: cfg}).ForRequest(t.Context())
	defer closeTools()
	var name string
	for _, def := range tools.Definitions() {
		if strings.HasPrefix(def.Function.Name, "mcp__") {
			name = def.Function.Name
		}
	}
	if name == "" {
		t.Fatal("slow fixture discovery failed")
	}
	for range 2 {
		start := time.Now()
		_, err := tools.Call(t.Context(), name, `{}`)
		if err == nil || time.Since(start) > 2*time.Second {
			t.Fatal("timeout lost", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unknown action retried")
	}
}

func TestMCPCredentialsNeverFollowOtherOrigins(t *testing.T) {
	var foreignCalls atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls.Add(1) }))
	defer foreign.Close()
	t.Setenv("FIXTURE_MCP_TOKEN", "private-fixture")
	for _, transport := range []string{"streamable_http", "sse"} {
		t.Run(transport, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if transport == "sse" {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", foreign.URL)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				} else {
					http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
				}
			}))
			defer server.Close()
			_, err := CheckMCP(t.Context(), []MCPServerConfig{{Name: "untrusted", URL: server.URL, Transport: transport, BearerTokenEnv: "FIXTURE_MCP_TOKEN", TimeoutMS: 1000}})
			if err == nil || foreignCalls.Load() != 0 {
				t.Fatal("credentials escaped configured origin", err)
			}
		})
	}
}

func TestMCPDiscoveryAndCallsOverHTTPAndSSE(t *testing.T) {
	for _, transport := range []string{"streamable_http", "sse"} {
		t.Run(transport, func(t *testing.T) {
			var calls atomic.Int64
			s := fixtureMCP(&calls)
			var handler http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
			if transport == "sse" {
				handler = mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return s }, nil)
			}
			t.Setenv("FIXTURE_MCP_TOKEN", "private-fixture")
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer private-fixture" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer httpServer.Close()
			cfg := DefaultConfig()
			cfg.Web, cfg.Time, cfg.History = false, false, false
			cfg.MCPServers = []MCPServerConfig{{Name: "one", Transport: transport, URL: httpServer.URL, BearerTokenEnv: "FIXTURE_MCP_TOKEN", AllowedTools: []string{"echo", "failure"}}, {Name: "two", Transport: transport, URL: httpServer.URL, BearerTokenEnv: "FIXTURE_MCP_TOKEN", AllowedTools: []string{"echo"}}}
			tools, close := (&Tools{Config: cfg}).ForRequest(t.Context())
			defer close()
			defs := tools.Definitions()
			if len(defs) != 3 || defs[0].Function.Name == defs[2].Function.Name {
				t.Fatalf("namespace/filter failure: %+v", defs)
			}
			for _, def := range defs {
				if !strings.HasPrefix(def.Function.Name, "mcp__") || len(def.Function.Name) > 64 || def.Function.Parameters["type"] != "object" {
					t.Fatal("invalid LLM tool schema", def)
				}
			}
			result, err := tools.Call(t.Context(), defs[0].Function.Name, `{"value":"hello","id":9007199254740993,"nested":{"x":1,"y":2}}`)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(result)
			if !strings.Contains(string(raw), "hello") || !strings.Contains(string(raw), "9007199254740993") {
				t.Fatal("lost MCP result", string(raw))
			}
			// Identical non-read-only calls, including failures, are never replayed.
			tools.Call(t.Context(), defs[0].Function.Name, `{"nested":{"y":2,"x":1},"id":9007199254740993,"value":"hello"}`)
			if calls.Load() != 1 {
				t.Fatal("duplicate side effect")
			}
			for range 2 {
				if _, err := tools.Call(t.Context(), defs[1].Function.Name, `{}`); err == nil || !strings.Contains(err.Error(), "fixture failure") {
					t.Fatal("MCP isError lost", err)
				}
			}
			if calls.Load() != 2 {
				t.Fatal("failed side effect retried")
			}
			if _, err := tools.Call(t.Context(), "mcp__one__hidden", `{}`); err == nil {
				t.Fatal("unadvertised tool called")
			}
		})
	}
}

func TestMCPPrivateHistoryRequiresExplicitPermission(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked", true: "trusted"}[allow], func(t *testing.T) {
			var calls atomic.Int64
			s := fixtureMCP(&calls)
			server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
			defer server.Close()
			cfg := DefaultConfig()
			cfg.Web, cfg.History = false, false
			cfg.MCPServers = []MCPServerConfig{{Name: "external", URL: server.URL, AllowedTools: []string{"echo"}, AllowPrivateHistory: allow}}
			alias := mcpAlias("external", "echo")
			round := 0
			a := Agent{Tools: &Tools{Config: cfg}, MaxCalls: 2, MaxRounds: 2, Client: chatFunc(func(ctx context.Context, messages []llm.Message, defs []llm.Tool) (llm.Message, error) {
				round++
				if round == 1 {
					advertised := false
					for _, def := range defs {
						advertised = advertised || def.Function.Name == alias
					}
					if advertised != allow {
						t.Fatal("history policy not applied to definitions")
					}
					// Attempt a forbidden call even when the definition was removed.
					return llm.Message{ToolCalls: []llm.ToolCall{{ID: "external", Type: "function", Function: llm.FunctionCall{Name: alias, Arguments: `{"value":"private history"}`}}}}, nil
				}
				return llm.Message{Content: "Ответ"}, nil
			})}
			_, private, err := a.CompleteDialogue(t.Context(), []llm.Message{{Role: "user", Content: "проверь данные"}}, true)
			if err != nil || !private || (calls.Load() == 1) != allow {
				t.Fatalf("history leaked or permission ignored: calls=%d private=%v err=%v", calls.Load(), private, err)
			}
		})
	}
}

func TestMCPStdio(t *testing.T) {
	if os.Getenv("QUASAR_MCP_STDIO_FIXTURE") == "1" {
		var calls atomic.Int64
		if err := fixtureMCP(&calls).Run(t.Context(), &mcp.StdioTransport{}); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	cfg := DefaultConfig()
	cfg.Web, cfg.Time, cfg.History = false, false, false
	cfg.MCPServers = []MCPServerConfig{{Name: "local", Transport: "stdio", Command: os.Args[0], Args: []string{"-test.run=^TestMCPStdio$"}, Env: map[string]string{"QUASAR_MCP_STDIO_FIXTURE": "1"}, AllowedTools: []string{"echo"}}}
	tools, close := (&Tools{Config: cfg}).ForRequest(t.Context())
	defer close()
	defs := tools.Definitions()
	if len(defs) != 1 {
		t.Fatal("stdio discovery failed", defs)
	}
	if _, err := tools.Call(t.Context(), defs[0].Function.Name, `{"value":"stdio"}`); err != nil {
		t.Fatal(err)
	}
}

func TestMCPFailureKeepsBuiltinToolsAndRedactsSecrets(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret-capability private-token", 503) }))
	defer httpServer.Close()
	cfg := DefaultConfig()
	cfg.Web, cfg.History = false, false
	cfg.MCPServers = []MCPServerConfig{{Name: "offline", URL: httpServer.URL + "/secret-capability"}}
	tools, close := (&Tools{Config: cfg}).ForRequest(t.Context())
	defer close()
	if defs := tools.Definitions(); len(defs) != 1 || defs[0].Function.Name != "current_time" {
		t.Fatal("failed MCP removed builtin", defs)
	}
	status, err := CheckMCP(t.Context(), cfg.MCPServers)
	if err == nil {
		t.Fatal("unavailable MCP diagnostic succeeded")
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw)+err.Error(), "secret-capability") || strings.Contains(string(raw)+err.Error(), "private-token") {
		t.Fatal("MCP error leaked credentials")
	}
}

func TestMCPAgentRoutesToolResultToModel(t *testing.T) {
	var calls atomic.Int64
	s := fixtureMCP(&calls)
	server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	defer server.Close()
	cfg := DefaultConfig()
	cfg.Web, cfg.Time, cfg.History = false, false, false
	cfg.MCPServers = []MCPServerConfig{{Name: "test", URL: server.URL, AllowedTools: []string{"echo"}}}
	round := 0
	a := Agent{Tools: &Tools{Config: cfg}, MaxCalls: 2, MaxRounds: 2, Client: chatFunc(func(ctx context.Context, messages []llm.Message, defs []llm.Tool) (llm.Message, error) {
		round++
		if round == 1 {
			if len(defs) != 1 {
				return llm.Message{}, errors.New("missing MCP definition")
			}
			return llm.Message{ToolCalls: []llm.ToolCall{{ID: "mcp-call", Type: "function", Function: llm.FunctionCall{Name: defs[0].Function.Name, Arguments: `{"value":"answer data"}`}}}}, nil
		}
		if !strings.Contains(messages[len(messages)-1].Content, "answer data") {
			t.Fatal("model did not receive MCP result")
		}
		return llm.Message{Content: "Готовый голосовой ответ"}, nil
	})}
	answer, err := a.Complete(t.Context(), []llm.Message{{Role: "user", Content: "спроси нейросеть"}}, false)
	if err != nil || answer != "Готовый голосовой ответ" || calls.Load() != 1 {
		t.Fatal(answer, err, calls.Load())
	}
}
