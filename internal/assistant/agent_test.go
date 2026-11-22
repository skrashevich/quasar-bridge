package assistant

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/llm"
)

func TestAgentExecutesToolAndReturnsModelAnswer(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Messages []llm.Message `json:"messages"`
			Tools    []llm.Tool    `json:"tools"`
		}
		json.UnmarshalRead(r.Body, &body)
		systems := 0
		for _, m := range body.Messages {
			if m.Role == "system" {
				systems++
			}
		}
		if systems != 1 {
			t.Errorf("Qwen requires one initial system message, got %d", systems)
		}
		if calls == 1 {
			if len(body.Tools) != 1 || body.Tools[0].Function.Name != "current_time" {
				t.Error("wrong tools")
			}
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"clock","type":"function","function":{"name":"current_time","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		last := body.Messages[len(body.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "clock" {
			t.Error("lost tool output")
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Время получено"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	config := DefaultConfig()
	config.Web = false
	config.History = false
	agent := Agent{Client: &llm.Client{BaseURL: server.URL}, Tools: &Tools{Config: config}, MaxRounds: 2, MaxCalls: 2}
	got, err := agent.Complete(t.Context(), []llm.Message{{Role: "system", Content: "Ты голосовой помощник"}, {Role: "user", Content: "время"}}, false)
	if err != nil || got != "Время получено" || calls != 2 {
		t.Fatalf("answer %q calls %d error %v", got, calls, err)
	}
}

type chatFunc func(context.Context, []llm.Message, []llm.Tool) (llm.Message, error)

func (f chatFunc) Chat(ctx context.Context, m []llm.Message, t []llm.Tool) (llm.Message, error) {
	return f(ctx, m, t)
}
func TestAgentRejectsCallsAfterBudgetAndDisabledTool(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Web = false
	cfg.History = false
	tools := &Tools{Config: cfg}
	if _, err := tools.Call(t.Context(), "read_webpage", `{"url":"https://example.org"}`); err == nil {
		t.Fatal("disabled tool executed")
	}
	n := 0
	agent := Agent{Client: chatFunc(func(ctx context.Context, m []llm.Message, tools []llm.Tool) (llm.Message, error) {
		n++
		systems := 0
		for _, message := range m {
			if message.Role == "system" {
				systems++
			}
		}
		if systems != 1 {
			t.Errorf("duplicate system prompt at budget boundary: %d", systems)
		}
		id := "first"
		if n > 1 {
			id = "second"
		}
		return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: id, Type: "function", Function: llm.FunctionCall{Name: "current_time", Arguments: "{}"}}}}, nil
	}), Tools: tools, MaxCalls: 1, MaxRounds: 1}
	if _, err := agent.Complete(t.Context(), nil, false); err == nil || n != 2 {
		t.Fatal("tool budget was ignored")
	}
}

type privacyTools struct{ calls []string }

func (t *privacyTools) Definitions() []llm.Tool {
	return []llm.Tool{{Type: "function", Function: llm.Function{Name: "search_history"}}, {Type: "function", Function: llm.Function{Name: "read_webpage"}}, {Type: "function", Function: llm.Function{Name: "current_time"}}}
}
func (t *privacyTools) Call(ctx context.Context, name, args string) (any, error) {
	t.calls = append(t.calls, name)
	return map[string]string{"text": "private history"}, nil
}
func TestHistoryPreventsWebExfiltrationInSameAndLaterBatch(t *testing.T) {
	for _, sameBatch := range []bool{false, true} {
		t.Run(fmt.Sprint(sameBatch), func(t *testing.T) {
			tools := new(privacyTools)
			round := 0
			history := llm.ToolCall{ID: "h", Type: "function", Function: llm.FunctionCall{Name: "search_history", Arguments: `{"query":""}`}}
			web := llm.ToolCall{ID: "w", Type: "function", Function: llm.FunctionCall{Name: "read_webpage", Arguments: `{"url":"https://example.org/?data=private"}`}}
			a := Agent{Tools: tools, MaxRounds: 3, MaxCalls: 3, Client: chatFunc(func(ctx context.Context, m []llm.Message, defs []llm.Tool) (llm.Message, error) {
				round++
				if round == 1 {
					calls := []llm.ToolCall{history}
					if sameBatch {
						calls = append(calls, web)
					}
					return llm.Message{ToolCalls: calls}, nil
				}
				if !sameBatch && round == 2 {
					return llm.Message{ToolCalls: []llm.ToolCall{web}}, nil
				}
				return llm.Message{Content: "answer"}, nil
			})}
			if _, err := a.Complete(t.Context(), nil, false); err != nil {
				t.Fatal(err)
			}
			if len(tools.calls) != 1 || tools.calls[0] != "search_history" {
				t.Fatalf("history leaked over network: %v", tools.calls)
			}
		})
	}
}

func TestPrivateHistoryBarrierSurvivesNextDialogueTurn(t *testing.T) {
	tools := &privacyTools{}
	calls := 0
	a := Agent{Tools: tools, MaxRounds: 3, MaxCalls: 3, Client: chatFunc(func(_ context.Context, m []llm.Message, defs []llm.Tool) (llm.Message, error) {
		calls++
		if calls == 1 {
			return llm.Message{ToolCalls: []llm.ToolCall{{ID: "h", Type: "function", Function: llm.FunctionCall{Name: "search_history", Arguments: `{"query":""}`}}}}, nil
		}
		if calls == 2 {
			return llm.Message{Content: "private answer"}, nil
		}
		for _, d := range defs {
			if d.Function.Name == "read_webpage" {
				t.Error("network tool offered with private history in context")
			}
		}
		if calls == 3 {
			return llm.Message{ToolCalls: []llm.ToolCall{{ID: "w", Type: "function", Function: llm.FunctionCall{Name: "read_webpage", Arguments: `{"url":"https://example.org"}`}}}}, nil
		}
		return llm.Message{Content: "Cannot send history"}, nil
	})}
	_, private, err := a.CompleteDialogue(t.Context(), []llm.Message{{Role: "user", Content: "history"}}, false)
	if err != nil || !private {
		t.Fatal("private access not reported", err)
	}
	_, private, err = a.CompleteDialogue(t.Context(), []llm.Message{{Role: "user", Content: "history"}, {Role: "assistant", Content: "private answer"}, {Role: "user", Content: "открой сайт"}}, private)
	if err != nil || !private || len(tools.calls) != 1 || tools.calls[0] != "search_history" {
		t.Fatalf("private data could reach webpage: calls=%v err=%v", tools.calls, err)
	}
}
