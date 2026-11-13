package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type deltaTransport struct{}

func (deltaTransport) RoundTrip(*http.Request) (*http.Response, error) {
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"private\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"private\"}]}]}}\n\n"
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestStructuredCompletionNeverEmitsSpeechDeltas(t *testing.T) {
	c := &Client{API: "chatgpt", Model: "test", HTTP: &http.Client{Transport: deltaTransport{}},
		AccessToken: func(context.Context) (string, error) { return "fixture", nil }}
	called := false
	ctx := WithTextDeltas(t.Context(), func(string) error { called = true; return nil })
	_, err := c.Complete(ctx, []Message{{Role: "user", Content: "route"}}, true)
	if err != nil || called {
		t.Fatalf("structured completion: callback=%v err=%v", called, err)
	}
}

func TestStreamDeltasBeforeCompletion(t *testing.T) {
	input := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"one\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\" two\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
	var deltas []string
	_, err := readResponseStreamWithDeltas(strings.NewReader(input), func(s string) error {
		deltas = append(deltas, s)
		return nil
	})
	if err != nil || strings.Join(deltas, "") != "one two" {
		t.Fatalf("deltas %v, err %v", deltas, err)
	}
}

func TestStreamDeltasStopOnConsumerError(t *testing.T) {
	want := errors.New("consumer stopped")
	input := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"one\"}\n\n"
	_, err := readResponseStreamWithDeltas(strings.NewReader(input), func(string) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
}

func TestStreamDeltaDeliveredBeforeCompletion(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	received := make(chan string, 1)
	finished := make(chan error, 1)
	go func() {
		_, err := readResponseStreamWithDeltas(reader, func(s string) error {
			received <- s
			return nil
		})
		finished <- err
	}()
	if _, err := io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-received:
		if value != "hello" {
			t.Fatalf("delta = %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("delta was buffered until completion")
	}
	select {
	case err := <-finished:
		t.Fatalf("stream ended prematurely: %v", err)
	default:
	}
	_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
	_ = writer.Close()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
