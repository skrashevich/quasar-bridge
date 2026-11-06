package assistant

import (
	"context"
	"crypto/sha256"
	jsonv1 "encoding/json" // RawMessage interoperates with the MCP SDK's JSON encoder.
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"quasarbridge/internal/llm"
)

type MCPStatus struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Tools     int      `json:"tools"`
	ToolNames []string `json:"tool_names,omitempty"`
	Error     string   `json:"error,omitempty"`
}
type mcpFunction struct {
	definition llm.Tool
	original   string
	readOnly   bool
}
type mcpConnection struct {
	config    MCPServerConfig
	session   *mcp.ClientSession
	cancel    context.CancelFunc
	functions []mcpFunction
	status    MCPStatus
}
type mcpTool struct {
	connection *mcpConnection
	function   mcpFunction
}
type mcpCachedCall struct {
	result any
	err    error
}
type mcpRequestTools struct {
	*Tools
	connections []*mcpConnection
	definitions []llm.Tool
	tools       map[string]mcpTool
	mu          sync.Mutex
	calls       map[string]mcpCachedCall
}

var mcpAliasChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func mcpAlias(server, name string) string {
	prefix := "mcp__" + server + "__"
	slug := mcpAliasChars.ReplaceAllString(name, "_")
	hash := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s%s_%x", prefix, slug[:min(len(slug), 64-len(prefix)-9)], hash[:4])
}

// Connections belong to one LLM answer, isolating speakers and guaranteeing
// cleanup on cancellation. Discovery failures do not disable the built-in tools.
func (t *Tools) ForRequest(ctx context.Context) (Toolset, func()) {
	if len(t.Config.MCPServers) == 0 {
		return t, func() {}
	}
	r := &mcpRequestTools{Tools: t, tools: map[string]mcpTool{}, calls: map[string]mcpCachedCall{}, definitions: t.Definitions()}
	r.connections = make([]*mcpConnection, len(t.Config.MCPServers))
	var wg sync.WaitGroup
	for i, cfg := range t.Config.MCPServers {
		if cfg.Disabled {
			continue
		}
		wg.Go(func() { r.connections[i] = discoverMCP(ctx, cfg) })
	}
	wg.Wait()
	for _, connection := range r.connections {
		if connection == nil {
			continue
		}
		if len(r.definitions)+len(connection.functions) > 128 {
			connection.status.Error = "combined MCP tool catalog exceeds 128 tools; use allowed_tools"
			connection.functions = nil
		}
		aliases := map[string]bool{}
		for _, f := range connection.functions {
			alias := f.definition.Function.Name
			if _, exists := r.tools[alias]; exists || aliases[alias] {
				connection.status.Error = "MCP tool alias collision"
				connection.functions = nil
				break
			}
			aliases[alias] = true
		}
		connection.status.Tools = len(connection.functions)
		for _, f := range connection.functions {
			connection.status.ToolNames = append(connection.status.ToolNames, f.original)
			r.definitions = append(r.definitions, f.definition)
			r.tools[f.definition.Function.Name] = mcpTool{connection: connection, function: f}
		}
		slog.Info("mcp_server", "name", connection.config.Name, "tools", connection.status.Tools, "ready", connection.status.Error == "")
	}
	return r, func() {
		var wg sync.WaitGroup
		for _, c := range r.connections {
			if c != nil && c.session != nil {
				wg.Go(func() { c.cancel(); _ = c.session.Close() })
			}
		}
		wg.Wait()
	}
}

func discoverMCP(ctx context.Context, cfg MCPServerConfig) *mcpConnection {
	c := &mcpConnection{config: cfg, status: MCPStatus{Name: cfg.Name, Transport: cfg.transport()}}
	if validateMCP([]MCPServerConfig{cfg}) != nil {
		c.status.Error = "invalid MCP server configuration or missing environment"
		return c
	}
	// Connect uses the answer's context; a timer only bounds initialization/list.
	deadlineCtx, cancelDeadline := context.WithTimeout(ctx, cfg.timeout())
	defer cancelDeadline()
	// Cancel the session if discovery expires, but detach that deadline on success.
	sessionCtx, cancelSession := context.WithCancel(ctx)
	stopDeadline := context.AfterFunc(deadlineCtx, cancelSession)
	defer stopDeadline()
	var err error
	c.session, c.cancel, err = openMCPSession(sessionCtx, cfg)
	if err != nil {
		cancelSession()
		c.status.Error = "MCP connection or initialization failed"
		return c
	}
	innerCancel := c.cancel
	c.cancel = func() { cancelSession(); innerCancel() }
	defer func() {
		if c.status.Error != "" {
			c.cancel()
			_ = c.session.Close()
			c.session = nil
		}
	}()
	cursor := ""
	seen := map[string]bool{}
	found := map[string]bool{}
	for page := 0; page < 8; page++ {
		result, err := c.session.ListTools(deadlineCtx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil || result == nil {
			c.status.Error = "MCP tools/list failed"
			break
		}
		for _, tool := range result.Tools {
			if tool == nil || tool.Name == "" || len(tool.Name) > 128 || seen[tool.Name] || len(seen) >= 256 {
				c.status.Error = "invalid or oversized MCP catalog"
				break
			}
			seen[tool.Name] = true
			if len(cfg.AllowedTools) > 0 && !slices.Contains(cfg.AllowedTools, tool.Name) || slices.Contains(cfg.DeniedTools, tool.Name) {
				continue
			}
			var schema map[string]any
			raw, err := json.Marshal(tool.InputSchema)
			if err != nil || len(raw) > 32000 || json.Unmarshal(raw, &schema) != nil || schema == nil || len(c.functions) >= 128 {
				c.status.Error = "invalid or oversized MCP tool schema"
				break
			}
			if _, ok := schema["type"]; !ok {
				schema["type"] = "object"
			}
			if schema["type"] != "object" {
				c.status.Error = "MCP tool arguments must use an object schema"
				break
			}
			found[tool.Name] = true
			description := fmt.Sprintf("MCP %s: %s. %s", cfg.Name, tool.Name, tool.Description)
			if len(description) > 4000 {
				description = validPrefix(description, 4000)
			}
			c.functions = append(c.functions, mcpFunction{original: tool.Name, readOnly: tool.Annotations != nil && tool.Annotations.ReadOnlyHint, definition: llm.Tool{Type: "function", Function: llm.Function{Name: mcpAlias(cfg.Name, tool.Name), Description: description, Parameters: schema}}})
		}
		if c.status.Error != "" || result.NextCursor == "" {
			break
		}
		if result.NextCursor == cursor || page == 7 {
			c.status.Error = "invalid or oversized MCP pagination"
			break
		}
		cursor = result.NextCursor
	}
	for _, name := range cfg.AllowedTools {
		if !slices.Contains(cfg.DeniedTools, name) && !found[name] && c.status.Error == "" {
			c.status.Error = "an allowed MCP tool was not advertised"
		}
	}
	if c.status.Error == "" && !stopDeadline() {
		c.status.Error = "MCP discovery timed out"
	}
	if c.status.Error != "" {
		c.functions = nil
	}
	return c
}

func (t *mcpRequestTools) Definitions() []llm.Tool { return t.definitions }
func (t *mcpRequestTools) AllowedAfterHistory(name string) bool {
	if tool, ok := t.tools[name]; ok {
		return tool.connection.config.AllowPrivateHistory
	}
	return name != "check_quasar_bridge_update" && name != "install_quasar_bridge_update" && name != "read_webpage" && name != "play_sound" && name != "play_music" && !strings.HasPrefix(name, "mcp__")
}
func (t *mcpRequestTools) ToolStatus() string {
	var failures []string
	for _, c := range t.connections {
		if c != nil && c.status.Error != "" {
			failures = append(failures, c.config.Name)
		}
	}
	if len(failures) == 0 {
		return ""
	}
	return "Недоступны MCP-серверы: " + strings.Join(failures, ", ") + ". Не утверждай, что выполнил их инструменты."
}
func (t *mcpRequestTools) Call(ctx context.Context, name, args string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tool, ok := t.tools[name]
	if !ok {
		return t.Tools.Call(ctx, name, args)
	}
	if len(args) > 16000 {
		return nil, errors.New("MCP arguments exceed limit")
	}
	var arguments map[string]jsontext.Value
	if json.Unmarshal([]byte(args), &arguments) != nil || arguments == nil {
		return nil, errors.New("MCP arguments must be a JSON object")
	}
	canonical, _ := json.Marshal(arguments, json.Deterministic(true), jsontext.ReorderRawObjects(true))
	key := name + "\x00" + string(canonical)
	t.mu.Lock()
	defer t.mu.Unlock()
	if previous, ok := t.calls[key]; ok && !tool.function.readOnly {
		return previous.result, previous.err
	}
	ctx, cancel := context.WithTimeout(ctx, tool.connection.config.timeout())
	defer cancel()
	result, err := tool.connection.session.CallTool(ctx, &mcp.CallToolParams{Name: tool.function.original, Arguments: jsonv1.RawMessage(args)})
	var value any
	if err != nil {
		err = errors.New("MCP tool call failed or timed out; completion is unknown")
	} else if result == nil || result.NeedsInput() {
		err = errors.New("MCP tool needs unsupported interactive input")
	} else {
		var texts []string
		unsupported := 0
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok && text != nil {
				texts = append(texts, text.Text)
			} else {
				unsupported++
			}
		}
		value = map[string]any{"content": texts, "structured_content": result.StructuredContent, "is_error": result.IsError, "unsupported_content": unsupported}
		raw, encodeErr := json.Marshal(value)
		if encodeErr != nil {
			err = errors.New("invalid MCP result")
		} else {
			if utf8.RuneCount(raw) > tool.connection.config.resultLimit() {
				limit := tool.connection.config.resultLimit()
				text := string(raw)
				for i := range text {
					if limit == 0 {
						text = text[:i]
						break
					}
					limit--
				}
				value = map[string]any{"truncated": true, "text": text}
			}
			if result.IsError {
				bounded, _ := json.Marshal(value)
				err = fmt.Errorf("MCP tool reported an error: %s", bounded)
			}
		}
	}
	if !tool.function.readOnly {
		t.calls[key] = mcpCachedCall{result: value, err: err}
	}
	return value, err
}
func validPrefix(s string, limit int) string {
	s = s[:min(len(s), limit)]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// CheckMCP discovers tools, but never calls them or asks the LLM anything.
func CheckMCP(ctx context.Context, configs []MCPServerConfig) ([]MCPStatus, error) {
	if err := validateMCP(configs); err != nil {
		return nil, err
	}
	t := &Tools{Config: Config{MCPServers: configs}}
	tools, close := t.ForRequest(ctx)
	defer close()
	statuses := []MCPStatus{}
	failed := false
	if request, ok := tools.(*mcpRequestTools); ok {
		for _, c := range request.connections {
			if c != nil {
				statuses = append(statuses, c.status)
				failed = failed || c.status.Error != ""
			}
		}
	}
	if failed {
		return statuses, errors.New("one or more MCP servers are unavailable")
	}
	return statuses, nil
}
