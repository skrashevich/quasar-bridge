package assistant

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"quasarbridge/internal/journal"
	"strings"
	"testing"
	"time"
)

func TestHistorySearchJoinsRotatedVoiceRequestsAndDeliveredAnswers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.jsonl")
	cfg := journal.DefaultConfig()
	cfg.Path = path
	cfg.MaxBackups = 2
	stamp := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	write := func(path string, events []journal.Event) {
		t.Helper()
		var rows []string
		for _, e := range events {
			raw, _ := json.Marshal(e)
			rows = append(rows, string(raw))
		}
		if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(path+".1", []journal.Event{{Time: stamp, Session: "s", Request: "r", Event: "request"}, {Time: stamp, Session: "s", Request: "r", Event: "transcript", Text: "Какая погода в Москве?"}})
	write(path, []journal.Event{{Time: stamp.Add(time.Second), Session: "s", Request: "r", Event: "cloud_response", Speech: "cloud duplicate"}, {Time: stamp.Add(time.Second), Session: "s", Request: "r", Event: "response", Speech: "Солнечно", Route: "yandex", Delivered: true}, {Time: stamp.Add(time.Minute), Session: "s", Request: "r2", Event: "request", Text: "Сколько будет два плюс два"}, {Time: stamp.Add(time.Minute), Session: "s", Request: "r2", Event: "response", Text: "Четыре", Delivered: true}})
	got, err := SearchHistory(t.Context(), cfg, HistoryQuery{Query: "москве солнечно", Limit: 5}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Answer != "Солнечно" || got.Items[0].Query != "Какая погода в Москве?" {
		t.Fatalf("bad history: %+v", got)
	}
	got, err = SearchHistory(t.Context(), cfg, HistoryQuery{Limit: 1}, 1)
	if err != nil || len(got.Items) != 1 || got.Items[0].Query != "Сколько будет два плюс два" {
		t.Fatalf("bad latest: %+v %v", got, err)
	}
	if _, err = SearchHistory(t.Context(), cfg, HistoryQuery{Since: "not-a-date"}, 1); err == nil {
		t.Fatal("invalid date accepted")
	}
}

func TestHistorySearchKeepsStreamedTurnsAndSkipsServiceResponses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.jsonl")
	cfg := journal.DefaultConfig()
	cfg.Path = path
	stamp := time.Now().Add(-time.Hour)
	events := []journal.Event{
		{Event: "request", Request: "stream", Text: "Расскажи про Луну"},
		{Event: "route", Request: "stream", Route: "yandex"},
		{Event: "progress", Request: "stream", Speech: "Луна", Delivered: true},
		{Event: "progress", Request: "stream", Text: "Луна — спутник Земли.", Delivered: true},
		{Event: "response", Request: "stream", Delivered: true},
		{Event: "response", Request: "stream", Delivered: true},
		{Event: "request", Request: "canceled", Text: "Включи музыку"},
		{Event: "progress", Request: "canceled", Text: "Включаю", Delivered: true},
		{Event: "cancel", Request: "canceled"},
		{Event: "response", Request: "service", Text: "Принято.", Delivered: true},
		{Event: "request", Request: "late"},
		{Event: "response", Request: "late", Text: "Четыре", Delivered: true},
		{Event: "transcript", Request: "late", Text: "Два плюс два"},
		{Event: "request", Request: "final", Text: "Проверка полного ответа"},
		{Event: "progress", Request: "final", Speech: "Сейчас объясню.", Delivered: true},
		{Event: "response", Request: "final", Kind: "DeferredAliceResponse", Text: "Полный ответ.", Speech: "ответ.", Delivered: true},
		{Event: "request", Request: "suppressed", Text: "Неотправленный ответ"},
		{Event: "progress", Request: "suppressed", Text: "Скрытый текст", Delivered: false},
	}
	var rows []string
	for i, e := range events {
		e.Time = stamp.Add(time.Duration(i) * time.Second)
		e.Session = "session"
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, string(raw))
	}
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := SearchHistory(t.Context(), cfg, HistoryQuery{Limit: 20}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 5 {
		t.Fatalf("history must contain only user turns: %+v", got.Items)
	}
	byQuery := map[string]HistoryItem{}
	for _, item := range got.Items {
		byQuery[item.Query] = item
	}
	if item := byQuery["Расскажи про Луну"]; item.Answer != "Луна — спутник Земли." || item.Route != "yandex" {
		t.Errorf("lost streaming answer or route: %+v", item)
	}
	if item := byQuery["Включи музыку"]; item.Answer != "Включаю" || item.Status != "interrupted" {
		t.Errorf("lost partial delivered answer: %+v", item)
	}
	if item := byQuery["Два плюс два"]; item.Answer != "Четыре" {
		t.Errorf("lost late transcript: %+v", item)
	}
	if item := byQuery["Проверка полного ответа"]; item.Answer != "Полный ответ." || item.Status != "" {
		t.Errorf("lost full final answer: %+v", item)
	}
	if item := byQuery["Неотправленный ответ"]; item.Answer != "" || item.Status != "incomplete" {
		t.Errorf("suppressed answer leaked: %+v", item)
	}
}
