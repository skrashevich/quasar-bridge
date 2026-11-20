package assistant

import (
	"context"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

type promptStatusTools struct{ tools Tools }

func (t *promptStatusTools) Definitions() []llm.Tool { return t.tools.Definitions() }
func (t *promptStatusTools) Call(ctx context.Context, name, args string) (any, error) {
	return t.tools.Call(ctx, name, args)
}

func (t *promptStatusTools) ToolStatus() string { return "Unavailable MCP: fixture" }

func TestToolPromptPreservesRuntimeStatusAndBudget(t *testing.T) {
	for _, prompt := range []string{"", " \n\t", "Custom tool instructions."} {
		t.Run(prompt, func(t *testing.T) {
			calls := 0
			a := Agent{SystemPrompt: prompt, Tools: &promptStatusTools{tools: Tools{Config: DefaultConfig()}}, MaxRounds: 1, MaxCalls: 1,
				Client: chatFunc(func(_ context.Context, messages []llm.Message, _ []llm.Tool) (llm.Message, error) {
					calls++
					system := messages[0].Content
					if strings.TrimSpace(prompt) == "" {
						if !strings.Contains(system, "Используй доступные инструменты") {
							t.Error("blank tool prompt suppressed default instructions")
						}
					} else if !strings.HasPrefix(system, prompt) || strings.Contains(system, "Используй доступные инструменты") {
						t.Errorf("custom tool instructions did not replace defaults: %q", system)
					}
					if !strings.Contains(system, "Unavailable MCP: fixture") || !strings.Contains(system, "Answer in English.") {
						t.Errorf("lost runtime status or general instructions: %q", system)
					}
					for i, message := range messages {
						if i > 0 && message.Role == "system" {
							t.Error("multiple system messages")
						}
					}
					if calls == 1 {
						return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "clock", Type: "function", Function: llm.FunctionCall{Name: "current_time", Arguments: "{}"}}}}, nil
					}
					if !strings.Contains(system, "Лимит вызовов инструментов исчерпан") {
						t.Error("custom prompt suppressed budget instruction")
					}
					return llm.Message{Role: "assistant", Content: "Answer"}, nil
				})}
			answer, err := a.Complete(t.Context(), []llm.Message{{Role: "system", Content: "Answer in English."}, {Role: "user", Content: "What time is it?"}}, false)
			if err != nil || answer != "Answer" || calls != 2 {
				t.Fatalf("tool dialogue failed: answer=%q, calls=%d, err=%v", answer, calls, err)
			}
		})
	}
}
