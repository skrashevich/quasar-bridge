package llm

import (
	"encoding/json/v2"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const streamMessageItem = `{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Связь"}]}`

func outputItemDone(index, item string) string {
	return `data: {"type":"response.output_item.done","output_index":` + index + `,"item":` + item + "}\n\n"
}

func TestChatGPTCompletesOutputItemsWithSparseTerminal(t *testing.T) {
	for _, terminal := range []string{
		`{"status":"completed","output":[]}`,
		`{"status":"completed"}`,
		`{"status":"completed","output":null}`,
	} {
		t.Run(terminal, func(t *testing.T) {
			c := chatGPTFixtureClient(t, func(*http.Request) string {
				return outputItemDone("0", streamMessageItem) + completedEvent(terminal)
			})
			answer, err := c.Complete(t.Context(), []Message{{Role: "user", Content: "проверка"}}, false)
			if err != nil || answer != "Связь" {
				t.Fatalf("answer=%q error=%v", answer, err)
			}
		})
	}
}

func TestResponseStreamOutputItemsKeepTerminalAuthoritative(t *testing.T) {
	c := chatGPTFixtureClient(t, func(*http.Request) string {
		return outputItemDone("0", streamMessageItem) + completedEvent(responseAnswer)
	})
	answer, err := c.Complete(t.Context(), nil, false)
	if err != nil || answer != "Ответ" {
		t.Fatalf("answer=%q error=%v", answer, err)
	}
}

func TestChatGPTOutputItemsPreserveReasoningAndNamespacedToolCycle(t *testing.T) {
	const reasoning = `{"type":"reasoning","encrypted_content":"private-reasoning","summary":[]}`
	const call = `{"type":"function_call","namespace":"quasar","name":"current_time","call_id":"call-1","arguments":"{}","status":"completed"}`
	calls := 0
	c := chatGPTFixtureClient(t, func(r *http.Request) string {
		calls++
		if calls == 1 {
			// Frames may arrive out of order; output_index defines replay order.
			return outputItemDone("1", call) + outputItemDone("0", reasoning) + completedEvent(`{"status":"completed","output":[]}`)
		}
		var body struct {
			Input []any `json:"input"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		var want []any
		if err := json.Unmarshal([]byte(`[{"role":"user","content":"time"},`+reasoning+`,`+call+`,{"type":"function_call_output","call_id":"call-1","output":"12:00"}]`), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(body.Input, want) {
			t.Errorf("tool replay lost output item fields or ordering: %v", body.Input)
		}
		return outputItemDone("0", streamMessageItem) + completedEvent(`{"status":"completed"}`)
	})
	messages := []Message{{Role: "user", Content: "time"}}
	tools := []Tool{{Type: "function", Function: Function{Name: "current_time", Parameters: map[string]any{"type": "object"}}}}
	message, err := c.Chat(t.Context(), messages, tools)
	if err != nil || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "current_time" {
		t.Fatalf("message=%+v error=%v", message, err)
	}
	messages = append(messages, message, Message{Role: "tool", ToolCallID: "call-1", Content: "12:00"})
	answer, err := c.Chat(t.Context(), messages, tools)
	if err != nil || answer.Content != "Связь" {
		t.Fatalf("message=%+v error=%v", answer, err)
	}
}

func TestResponseStreamOutputItemsRequireSuccessfulTerminal(t *testing.T) {
	for _, ending := range []string{
		"",
		"data: [DONE]\n\n",
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"subscription_sharing_usage_limit_exceeded\"}}}\n\n",
		"data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n",
	} {
		t.Run(ending, func(t *testing.T) {
			data, err := readResponseStream(strings.NewReader(outputItemDone("0", streamMessageItem) + ending))
			if err == nil || data != nil {
				t.Fatalf("unfinished output escaped: data=%s error=%v", data, err)
			}
		})
	}
}

func TestResponseStreamRejectsMalformedOutputItems(t *testing.T) {
	for _, tc := range []struct{ name, frames string }{
		{"missing index", "data: {\"type\":\"response.output_item.done\",\"item\":" + streamMessageItem + "}\n\n"},
		{"null index", outputItemDone("null", streamMessageItem)},
		{"negative index", outputItemDone("-1", streamMessageItem)},
		{"fractional index", outputItemDone("0.5", streamMessageItem)},
		{"huge index", outputItemDone("2147483647", streamMessageItem)},
		{"missing first index", outputItemDone("1", streamMessageItem)},
		{"index gap", outputItemDone("0", streamMessageItem) + outputItemDone("2", streamMessageItem)},
		{"duplicate index", outputItemDone("0", streamMessageItem) + outputItemDone("0", streamMessageItem)},
		{"missing item", "data: {\"type\":\"response.output_item.done\",\"output_index\":0}\n\n"},
		{"null item", outputItemDone("0", "null")},
		{"array item", outputItemDone("0", "[]")},
		{"untyped item", outputItemDone("0", "{}")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := readResponseStream(strings.NewReader(tc.frames + completedEvent(`{"status":"completed","output":[]}`)))
			if err == nil || data != nil {
				t.Fatalf("invalid output accepted: data=%s error=%v", data, err)
			}
		})
	}
}
