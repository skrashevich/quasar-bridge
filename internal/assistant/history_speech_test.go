package assistant

import (
	"path/filepath"
	"testing"

	"quasarbridge/internal/journal"
)

func TestHistorySearchPreservesLocalLLMDisplayText(t *testing.T) {
	const original = "Матч начнётся в пятницу. ([example.org](https://example.org/match))"
	const spoken = "Матч начнётся в пятницу."
	for _, tc := range []struct {
		name, route, kind, text, speech, want string
	}{
		{"local immediate", "llm", "AliceResponse", original, spoken, original},
		{"local URL only", "llm", "AliceResponse", "https://example.org/match", "Ссылка доступна в текстовом ответе.", "https://example.org/match"},
		{"local speech only", "llm", "AliceResponse", "", spoken, spoken},
		{"cloud immediate", "yandex", "AliceResponse", original, spoken, spoken},
		{"unknown immediate", "", "AliceResponse", original, spoken, spoken},
		{"local deferred", "llm", "DeferredAliceResponse", original, spoken, original},
		{"cloud deferred", "yandex", "DeferredAliceResponse", original, spoken, original},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := journal.DefaultConfig()
			cfg.Enabled = true
			cfg.Path = filepath.Join(t.TempDir(), "conversation.jsonl")
			writer := new(journal.Writer)
			if err := writer.Configure(cfg); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = writer.Close() })
			writer.Record(journal.Event{Session: "speaker", Request: "question", Event: "request", Kind: "TextInput", Text: "Когда матч?"})
			writer.Record(journal.Event{Session: "speaker", Request: "question", Event: "route", Route: tc.route})
			writer.Record(journal.Event{Session: "speaker", Request: "question", Event: "response", Kind: tc.kind, Route: tc.route, Text: tc.text, Speech: tc.speech, Delivered: true})
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}

			result, err := SearchHistory(t.Context(), cfg, HistoryQuery{Limit: 20}, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Items) != 1 || result.Items[0].Query != "Когда матч?" || result.Items[0].Answer != tc.want || result.Items[0].Route != tc.route {
				t.Fatalf("history lost the correct answer: %+v, want %q", result.Items, tc.want)
			}
		})
	}
}
