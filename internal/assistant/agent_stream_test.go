package assistant

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

type streamTestTransport struct{ calls int }

func (t *streamTestTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	var event string
	if t.calls == 1 {
		event = `{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","status":"completed","call_id":"clock","name":"current_time","arguments":"{}"}]}}`
	} else {
		event = `{"type":"response.output_text.delta","delta":"Сейчас полдень."}` + "\n\ndata: " +
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Сейчас полдень."}]}]}}`
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + event + "\n\n"))}, nil
}

func TestStreamDeltaAfterToolRound(t *testing.T) {
	transport := new(streamTestTransport)
	model := &llm.Client{API: "chatgpt", Model: "test", HTTP: &http.Client{Transport: transport},
		AccessToken: func(context.Context) (string, error) { return "fixture", nil }}
	cfg := DefaultConfig()
	cfg.Web, cfg.History = false, false
	agent := Agent{Client: model, Tools: &Tools{Config: cfg}, MaxRounds: 2, MaxCalls: 2}
	var deltas []string
	ctx := llm.WithTextDeltas(t.Context(), func(s string) error {
		deltas = append(deltas, s)
		return nil
	})
	answer, err := agent.Complete(ctx, []llm.Message{{Role: "user", Content: "который час"}}, false)
	if err != nil || answer != "Сейчас полдень." || strings.Join(deltas, "") != answer || transport.calls != 2 {
		t.Fatalf("answer=%q deltas=%v calls=%d err=%v", answer, deltas, transport.calls, err)
	}
}
