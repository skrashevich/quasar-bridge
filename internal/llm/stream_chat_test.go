package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompleteStreamYieldsTextBeforeResponseIsDone(t *testing.T) {
	release := make(chan struct{})
	first := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "\"stream\":true") {
			t.Errorf("request is not streaming: %s", string(body))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Первое предложение. \"}}]}")
		w.(http.Flusher).Flush()
		close(first)
		<-release
		fmt.Fprintln(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Второе предложение.\"}}]}")
		fmt.Fprintln(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}")
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Model: "test", API: "chat_completions"}
	gotFirst := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		answer, err := client.CompleteStream(context.Background(), []Message{{Role: "user", Content: "Тест"}}, false, func(delta string) error {
			select {
			case gotFirst <- delta:
			default:
			}
			return nil
		})
		if err == nil && answer != "Первое предложение. Второе предложение." {
			err = fmt.Errorf("lost streamed content: %q", answer)
		}
		done <- err
	}()
	<-first
	select {
	case got := <-gotFirst:
		if got != "Первое предложение. " {
			t.Fatalf("bad first delta %q", got)
		}
	case <-done:
		t.Fatal("LLM finished before the first chunk was consumed")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSentenceStreamingRejectsNonCompletionsAPI(t *testing.T) {
	for _, api := range []string{"responses", "chatgpt"} {
		if (&Client{API: api}).SupportsSentenceStreaming() {
			t.Fatalf("unexpected streaming capability for %s", api)
		}
	}
}
