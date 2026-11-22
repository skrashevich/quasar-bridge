package assistant

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skrashevich/quasar-bridge/internal/journal"
	"github.com/skrashevich/quasar-bridge/internal/llm"
)

func TestNaturalHistoryHasLocalDateAndRecordsBeforeFirstModelCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	stamp := time.Date(2026, 10, 25, 22, 12, 0, 0, time.UTC)
	var rows []string
	for _, e := range []journal.Event{{Time: stamp, Session: "s", Request: "r", Event: "request", Text: "сколько будет три плюс четыре"}, {Time: stamp, Session: "s", Request: "r", Event: "response", Text: "7", Delivered: true}} {
		raw, _ := json.Marshal(e)
		rows = append(rows, string(raw))
	}
	os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0600)
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Timezone = "Europe/Moscow"
	source := journal.DefaultConfig()
	source.Path = path
	tools := &Tools{Config: cfg, JournalConfig: func() journal.Config { return source }, Now: func() time.Time { return stamp.Add(time.Hour) }}
	calls := 0
	agent := Agent{Tools: tools, MaxRounds: 3, MaxCalls: 4, Client: chatFunc(func(ctx context.Context, m []llm.Message, defs []llm.Tool) (llm.Message, error) {
		calls++
		clock, history := false, false
		for _, message := range m {
			if message.Role == "tool" {
				if strings.Contains(message.Content, `2026-10-26T02:12:00+03:00`) {
					clock = true
				}
				if strings.Contains(message.Content, `2026-10-26T01:12:00+03:00`) && strings.Contains(message.Content, `"answer":"7"`) {
					history = true
				}
			}
		}
		if !clock || !history {
			t.Errorf("prefetched local context missing: clock=%v history=%v", clock, history)
		}
		for _, def := range defs {
			if def.Function.Name == "read_webpage" {
				t.Error("web available after private prefetch")
			}
		}
		return llm.Message{Content: "Сегодня считали три плюс четыре"}, nil
	})}
	answer, err := agent.Complete(t.Context(), []llm.Message{{Role: "user", Content: "о чём мы сегодня общались"}}, false)
	if err != nil || answer == "" || calls != 1 {
		t.Fatalf("answer %q calls %d err %v", answer, calls, err)
	}
}

func TestExplicitWebURLIsReadBeforeFirstModelCall(t *testing.T) {
	cfg := DefaultConfig()
	reader := NewWebReader(cfg)
	fetches := 0
	reader.http = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		fetches++
		if r.URL.Path != "/MixedCase" {
			t.Error("URL case changed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(`<title>Example Domain</title><p>Example text</p>`))}, nil
	})}
	tools := &Tools{Config: cfg, WebReader: reader}
	modelCalls := 0
	a := Agent{Tools: tools, MaxCalls: 4, MaxRounds: 3, Client: chatFunc(func(ctx context.Context, m []llm.Message, defs []llm.Tool) (llm.Message, error) {
		modelCalls++
		found := false
		for _, msg := range m {
			if msg.Role == "tool" && strings.Contains(msg.Content, "Example Domain") {
				found = true
			}
		}
		if !found {
			t.Error("web page missing before generation")
		}
		return llm.Message{Content: "Example Domain"}, nil
	})}
	if _, err := a.Complete(t.Context(), []llm.Message{{Role: "user", Content: "прочитай страницу https://example.org/MixedCase"}}, false); err != nil || modelCalls != 1 || fetches != 1 {
		t.Fatalf("model=%d fetch=%d err=%v", modelCalls, fetches, err)
	}
}

func TestYesterdayFollowupPrefetchesFreshHistory(t *testing.T) {
	stamp := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "history.jsonl")
	var rows []string
	for _, tc := range []struct {
		when time.Time
		text string
	}{{stamp.Add(-24 * time.Hour), "yesterday-marker"}, {stamp, "today-marker"}} {
		raw, _ := json.Marshal(journal.Event{Time: tc.when, Session: "s", Request: tc.text, Event: "request", Text: tc.text})
		rows = append(rows, string(raw))
	}
	os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0600)
	cfg := DefaultConfig()
	cfg.Timezone = "UTC"
	source := journal.DefaultConfig()
	source.Path = path
	tools := &Tools{Config: cfg, JournalConfig: func() journal.Config { return source }, Now: func() time.Time { return stamp }}
	agent := Agent{Tools: tools, MaxRounds: 2, MaxCalls: 3, Client: chatFunc(func(_ context.Context, m []llm.Message, _ []llm.Tool) (llm.Message, error) {
		found := false
		for _, msg := range m {
			if msg.Role == "tool" && strings.Contains(msg.Content, "yesterday-marker") {
				found = true
				if strings.Contains(msg.Content, "today-marker") {
					t.Error("today was included in yesterday selection")
				}
			}
		}
		if !found {
			t.Error("yesterday history was not prefetched")
		}
		return llm.Message{Content: "Вчера обсуждали вчерашнюю тему"}, nil
	})}
	_, err := agent.Complete(t.Context(), []llm.Message{{Role: "user", Content: "о чём мы сегодня общались?"}, {Role: "assistant", Content: "Сегодня говорили о музыке"}, {Role: "user", Content: "а вчера?"}}, false)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLatestDateWinsOverEarlierFollowup(t *testing.T) {
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	tools := &Tools{Config: cfg, Now: func() time.Time { return now }}
	messages := []llm.Message{{Role: "user", Content: "о чём мы сегодня общались?"}, {Role: "assistant", Content: "Сегодня обсуждали музыку"}, {Role: "user", Content: "а вчера?"}, {Role: "assistant", Content: "Вчера обсуждали погоду"}, {Role: "user", Content: "а сегодня?"}}
	found := false
	for _, call := range tools.InitialCallsForConversation(messages) {
		if call.Name == "search_history" {
			found = true
			var q HistoryQuery
			if err := json.Unmarshal([]byte(call.Arguments), &q); err != nil {
				t.Fatal(err)
			}
			if q.Since != "2026-10-26T00:00:00Z" || q.Until != "" {
				t.Fatal("previous yesterday overrode current today", q)
			}
		}
	}
	if !found {
		t.Fatal("history topic lost across several followups")
	}
}
