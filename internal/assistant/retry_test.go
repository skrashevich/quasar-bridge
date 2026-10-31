package assistant

import (
	"context"
	"errors"
	"quasarbridge/internal/llm"
	"testing"
)

type failingTool struct{ calls int }

func (t *failingTool) Definitions() []llm.Tool {
	return []llm.Tool{definition("read_webpage", "Читает страницу", map[string]any{"url": map[string]string{"type": "string"}}, []string{"url"})}
}

func (t *failingTool) Call(context.Context, string, string) (any, error) {
	t.calls++
	return nil, errors.New("read timeout")
}

func TestAgentReusesFailedToolResult(t *testing.T) {
	tools := &failingTool{}
	rounds := 0
	agent := Agent{Tools: tools, MaxRounds: 4, MaxCalls: 4, Client: chatFunc(func(context.Context, []llm.Message, []llm.Tool) (llm.Message, error) {
		rounds++
		if rounds < 3 {
			return llm.Message{ToolCalls: []llm.ToolCall{{ID: string(rune('a' + rounds)), Type: "function", Function: llm.FunctionCall{Name: "read_webpage", Arguments: `{"url":"https://example.com/page"}`}}}}, nil
		}
		return llm.Message{Content: "Страница недоступна."}, nil
	})}
	answer, err := agent.Complete(t.Context(), []llm.Message{{Role: "user", Content: "Прочитай https://example.com/page"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Страница недоступна." {
		t.Fatalf("unexpected answer %q", answer)
	}
	if tools.calls != 1 {
		t.Fatalf("failed tool executed %d times, want 1", tools.calls)
	}
}
