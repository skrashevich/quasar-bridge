package bridge

import (
	"context"
	"encoding/json/v2"
	"testing"

	"quasarbridge/internal/llm"
)

// Decode JSON to test both explicit switches and legacy configs without a flag.
func TestClassifierSwitchAndIndependentDefault(t *testing.T) {
	for _, tc := range []struct {
		settings, route, source string
		calls                   int
	}{
		{`{"default_route":"classify"}`, "home", "classifier", 1},
		{`{"default_route":"llm"}`, "llm", "default", 0},
		{`{"default_route":"llm","classifier":{"enabled":true}}`, "home", "classifier", 1},
		{`{"default_route":"llm","classifier":{"enabled":false}}`, "llm", "default", 0},
		{`{"default_route":"yandex","classifier":{"enabled":false}}`, "yandex", "default", 0},
		{`{"default_route":"classify","classifier":{"enabled":false,"on_error":"llm"}}`, "llm", "default", 0},
	} {
		t.Run(tc.settings, func(t *testing.T) {
			cfg := DefaultRoutingConfig()
			if err := json.Unmarshal([]byte(tc.settings), &cfg); err != nil {
				t.Fatal(err)
			}
			cfg.Rules = nil
			calls := 0
			e := &Engine{Home: homeFunc(func(context.Context, string) (string, error) {
				t.Fatal("deciding a route executed Home Assistant")
				return "", nil
			}), LLM: completionFunc(func(_ context.Context, _ []llm.Message, structured bool) (string, error) {
				if !structured {
					t.Fatal("route decision generated an answer")
				}
				calls++
				return `{"route":"home"}`, nil
			})}
			if err := e.SetRouting(cfg); err != nil {
				t.Fatal(err)
			}
			got := e.Decide(t.Context(), "совершенно новый вопрос")
			if got.Route != tc.route || got.Source != tc.source || calls != tc.calls {
				t.Fatalf("decision=%+v calls=%d want=%s/%s/%d", got, calls, tc.route, tc.source, tc.calls)
			}
			policy, err := CompileRouting(cfg)
			if err != nil {
				t.Fatal(err)
			}
			preview := policy.Preview("совершенно новый вопрос", "")
			wantPreview := tc.route
			if tc.calls > 0 {
				wantPreview = "classify"
			}
			if preview.Route != wantPreview || calls != tc.calls {
				t.Fatalf("preview=%+v calls=%d", preview, calls)
			}
		})
	}
}
