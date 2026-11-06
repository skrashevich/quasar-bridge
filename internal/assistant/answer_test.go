package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

type unavailableWebTools struct{ calls int }

func (*unavailableWebTools) Definitions() []llm.Tool {
	return []llm.Tool{definition("read_webpage", "Read a page", map[string]any{"url": map[string]string{"type": "string"}}, []string{"url"})}
}
func (t *unavailableWebTools) Call(context.Context, string, string) (any, error) {
	t.calls++
	return nil, errors.New("fixture webpage unavailable")
}

func TestAgentRejectsToolMarkupAfterFailedToolBudget(t *testing.T) {
	const raw = "<tool_call>\n<function=read_webpage>\n<parameter=url>\nhttps://example.org\n</parameter>\n</function>\n</tool_call>"
	rounds := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Tools []llm.Tool `json:"tools"`
		}
		if err := json.UnmarshalRead(r.Body, &input); err != nil {
			t.Error(err)
			http.Error(w, "invalid fixture input", 400)
			return
		}
		rounds++
		message := llm.Message{Role: "assistant", Content: raw}
		if rounds <= 3 {
			if rounds == 1 && len(input.Tools) != 1 {
				t.Error("tool unavailable before any attempt")
			}
			if rounds > 1 && len(input.Tools) != 0 {
				t.Error("failed tool kept offered")
			}
			message = llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: string(rune('a' + rounds)), Type: "function", Function: llm.FunctionCall{Name: "read_webpage", Arguments: `{"url":"https://example.org"}`}}}}
		} else if len(input.Tools) != 0 {
			t.Error("tool budget not exhausted")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	tools := new(unavailableWebTools)
	agent := Agent{Client: &llm.Client{BaseURL: server.URL}, Tools: tools, MaxRounds: 3, MaxCalls: 4}
	answer, err := agent.Complete(t.Context(), []llm.Message{{Role: "user", Content: "Открой страницу https://example.org"}}, false)
	if err == nil || answer != "" {
		t.Fatalf("raw tool call accepted as final speech: answer=%q error=%v", answer, err)
	}
	if rounds != 4 || tools.calls != 1 {
		t.Fatalf("unexpected retry: model rounds=%d tool calls=%d", rounds, tools.calls)
	}
	if strings.Contains(err.Error(), "example.org") {
		t.Fatal("rejected payload leaked into error")
	}
}

func TestAgentFinalAnswerRejectsReservedToolTags(t *testing.T) {
	for _, text := range []string{
		`<tool_call>{"name":"read_webpage","arguments":{"url":"https://example.org"}}</tool_call>`,
		"Сейчас открою.\n<tool_call>\n<function=read_webpage>",
		"```xml\n<TOOL_CALL>\n</TOOL_CALL>\n```",
		`<function=read_webpage><parameter=url>https://example.org`,
		`<tool_calls><tool_call`,
		`<tool_call`,
	} {
		t.Run(text[:min(len(text), 40)], func(t *testing.T) {
			agent := Agent{Tools: &Tools{}, MaxRounds: 1, MaxCalls: 1, Client: chatFunc(func(context.Context, []llm.Message, []llm.Tool) (llm.Message, error) {
				return llm.Message{Content: text}, nil
			})}
			if answer, err := agent.Complete(t.Context(), nil, false); err == nil || answer != "" {
				t.Fatalf("tool markup escaped final answer validation: answer=%q error=%v", answer, err)
			}
		})
	}
}

func TestAgentFinalAnswerPreservesOrdinaryText(t *testing.T) {
	for _, text := range []string{"Не удалось прочитать страницу.", "Инструмент read_webpage недоступен.", "<p>Пример HTML</p>", "Идентификатор tool_call хранится в JSON.", "<tool_callback>обычный тег</tool_callback>"} {
		agent := Agent{Tools: &Tools{}, MaxRounds: 1, MaxCalls: 1, Client: chatFunc(func(context.Context, []llm.Message, []llm.Tool) (llm.Message, error) {
			return llm.Message{Content: text}, nil
		})}
		if answer, err := agent.Complete(t.Context(), nil, false); err != nil || answer != text {
			t.Fatalf("ordinary answer changed: answer=%q error=%v", answer, err)
		}
	}
}
