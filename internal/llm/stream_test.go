package llm

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
)

func streamItem(index int, item string) string {
	data, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": index, "item": jsontext.Value(item)})
	return "data: " + string(data) + "\n\n"
}

const streamedAnswer = `{"id":"msg-1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Привет"}]}`

func TestChatGPTRecoversCompletedStreamItems(t *testing.T) {
	for _, output := range []string{`"output":[]`, `"output":null`, `"id":"response-1"`} {
		t.Run(output, func(t *testing.T) {
			c := chatGPTFixtureClient(t, func(*http.Request) string {
				return "data: {\"type\":\"response.output_text.delta\",\"delta\":\"При\"}\n\n" +
					"data: {\"type\":\"response.output_text.delta\",\"delta\":\"вет\"}\n\n" +
					streamItem(0, streamedAnswer) + completedEvent(`{"status":"completed",`+output+`}`)
			})
			answer, err := c.Complete(t.Context(), nil, false)
			if err != nil || answer != "Привет" {
				t.Fatalf("answer=%q error=%v", answer, err)
			}
		})
	}
}

func TestChatGPTPrefersTerminalOutput(t *testing.T) {
	c := chatGPTFixtureClient(t, func(*http.Request) string {
		return streamItem(0, streamedAnswer) + completedEvent(responseAnswer)
	})
	answer, err := c.Complete(t.Context(), nil, false)
	if err != nil || answer != "Ответ" {
		t.Fatalf("answer=%q error=%v", answer, err)
	}
}

func TestChatGPTRecoveredItemsKeepToolContext(t *testing.T) {
	const reasoning = `{"type":"reasoning","encrypted_content":"fixture-reasoning","summary":[]}`
	const call = `{"type":"function_call","namespace":"quasar","name":"current_time","call_id":"call-1","arguments":"{}","status":"completed"}`
	requests := 0
	c := chatGPTFixtureClient(t, func(r *http.Request) string {
		requests++
		if requests == 1 {
			// Item order is defined by output_index, not arrival order.
			return streamItem(1, call) + streamItem(0, reasoning) + completedEvent(`{"status":"completed","output":[]}`)
		}
		var body struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Input) != 4 || body.Input[1]["encrypted_content"] != "fixture-reasoning" || body.Input[2]["namespace"] != "quasar" || body.Input[3]["call_id"] != "call-1" {
			t.Errorf("recovered context not preserved: %v", body.Input)
		}
		return streamItem(0, streamedAnswer) + completedEvent(`{"status":"completed","output":[]}`)
	})
	tools := []Tool{{Type: "function", Function: Function{Name: "current_time", Parameters: map[string]any{"type": "object"}}}}
	messages := []Message{{Role: "user", Content: "time"}}
	m, err := c.Chat(t.Context(), messages, tools)
	if err != nil || len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "current_time" {
		t.Fatalf("message=%+v error=%v", m, err)
	}
	messages = append(messages, m, Message{Role: "tool", ToolCallID: "call-1", Content: "12:00"})
	answer, err := c.Chat(t.Context(), messages, tools)
	if err != nil || answer.Content != "Привет" {
		t.Fatalf("message=%+v error=%v", answer, err)
	}
}

func TestChatGPTDoesNotReleaseUnfinishedStreamItems(t *testing.T) {
	const partialCall = `{"type":"function_call","name":"current_time","call_id":"call-1","arguments":"{}","status":"in_progress"}`
	cases := []struct{ name, stream, want string }{
		{"no terminal", streamItem(0, streamedAnswer), "not completed"},
		{"failed after item", streamItem(0, streamedAnswer) + "data: {\"type\":\"response.failed\"}\n\n", "failed"},
		{"incomplete after item", streamItem(0, streamedAnswer) + "data: {\"type\":\"response.incomplete\"}\n\n", "incomplete"},
		{"wrong terminal status", streamItem(0, streamedAnswer) + completedEvent(`{"status":"in_progress","output":[]}`), "not completed"},
		{"unfinished tool", streamItem(0, partialCall) + completedEvent(`{"status":"completed","output":[]}`), "incomplete"},
		{"delta without done item", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" + completedEvent(`{"status":"completed","output":[]}`), "no answer"},
		{"missing item index", "data: {\"type\":\"response.output_item.done\",\"item\":{}}\n\n", "invalid"},
		{"negative item index", streamItem(-1, streamedAnswer), "invalid"},
		{"null item", streamItem(0, `null`), "invalid"},
		{"missing earlier item", streamItem(1, streamedAnswer) + completedEvent(`{"status":"completed","output":[]}`), "missing items"},
		{"duplicate item", streamItem(0, streamedAnswer) + streamItem(0, streamedAnswer) + completedEvent(`{"status":"completed","output":[]}`), "duplicate"},
		{"refusal", streamItem(0, `{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}`) + completedEvent(`{"status":"completed","output":[]}`), "refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := chatGPTFixtureClient(t, func(*http.Request) string { return tc.stream })
			m, err := c.Chat(t.Context(), nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || m.Content != "" || len(m.ToolCalls) != 0 {
				t.Fatalf("message=%+v error=%v want=%q", m, err, tc.want)
			}
		})
	}
}
