package main

import (
	"context"
	"encoding/json/v2"
	"os"
	"reflect"
	"testing"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/llm"
)

func routingToolsFixture(t *testing.T) (*configurationControl, *assistant.Tools) {
	t.Helper()
	c := configurationFixture(t, `{"llm_model":"old","routing":{"rules":[{"name":"weather","route":"yandex","match":{"contains_any":["погода"]}},{"name":"code","route":"llm","match":{"contains_any":["код"]}}],"default_route":"yandex"}}`)
	return c, &assistant.Tools{Config: assistant.DefaultConfig(), Configuration: c}
}

func TestRoutingToolsManageAndPreviewWakewordRule(t *testing.T) {
	_, tools := routingToolsFixture(t)
	rule := map[string]any{"name": "wakeword", "route": "llm", "match": map[string]any{"wake_words": []string{"джарвис", "лёна"}}}
	routingToolCall(t, tools, "set_quasar_bridge_routing_rule", map[string]any{"rule": rule})
	for _, tc := range []struct{ args, route, rule string }{
		{`{"text":"погода завтра","wake_word":"лёна"}`, "llm", "wakeword"},
		{`{"text":"погода завтра","wake_word":"Лёна"}`, "yandex", "weather"},
		{`{"text":"погода завтра"}`, "yandex", "weather"},
	} {
		value, err := tools.Call(t.Context(), "test_quasar_bridge_routing", tc.args)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var preview struct {
			Active bridge.RouteDecision `json:"active"`
			Saved  bridge.RouteDecision `json:"saved"`
		}
		if err := json.Unmarshal(raw, &preview); err != nil {
			t.Fatal(err)
		}
		if preview.Active.Route != tc.route || preview.Active.Rule != tc.rule || !reflect.DeepEqual(preview.Saved, preview.Active) {
			t.Fatalf("%s: wrong active/saved preview: %s", tc.args, raw)
		}
	}
}

func routingRevisionFromTool(t *testing.T, tools *assistant.Tools) string {
	t.Helper()
	value, err := tools.Call(t.Context(), "get_quasar_bridge_routing", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		SavedRevision string `json:"saved_revision"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.SavedRevision == "" {
		t.Fatalf("missing saved routing revision: %s %v", raw, err)
	}
	return result.SavedRevision
}

func routingToolCall(t *testing.T, tools *assistant.Tools, name string, args map[string]any) any {
	t.Helper()
	args["expected_revision"] = routingRevisionFromTool(t, tools)
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tools.Call(t.Context(), name, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(encoded, &status); err != nil || status.Status != "applied" {
		t.Fatalf("routing not applied: %s %v", encoded, err)
	}
	return result
}

func TestRoutingToolsManageOneRuleAndApplyWithoutRestart(t *testing.T) {
	c, tools := routingToolsFixture(t)
	original, err := readConfig(c.path)
	if err != nil {
		t.Fatal(err)
	}
	rule := map[string]any{"name": "voice_weather", "route": "llm", "match": map[string]any{"contains_any": []string{"погода"}}}
	routingToolCall(t, tools, "set_quasar_bridge_routing_rule", map[string]any{"rule": rule})
	if got := c.engine.Decide(t.Context(), "погода завтра"); got.Route != "llm" || got.Rule != "voice_weather" {
		t.Fatalf("new rule did not precede broad weather rule: %+v", got)
	}
	cfg, err := readConfig(c.path)
	if err != nil || len(cfg.Routing.Rules) != 3 || cfg.Routing.Rules[1].Name != "weather" || cfg.LLMModel != "old" {
		t.Fatalf("unrelated configuration lost: %+v %v", cfg, err)
	}
	routingToolCall(t, tools, "control_quasar_bridge_routing_rule", map[string]any{"action": "disable", "name": "voice_weather"})
	if got := c.engine.Decide(t.Context(), "погода завтра"); got.Route != "yandex" {
		t.Fatalf("disabled rule still active: %+v", got)
	}
	routingToolCall(t, tools, "control_quasar_bridge_routing_rule", map[string]any{"action": "enable", "name": "voice_weather"})
	if got := c.engine.Decide(t.Context(), "погода завтра"); got.Route != "llm" {
		t.Fatalf("enabled rule not active: %+v", got)
	}
	routingToolCall(t, tools, "control_quasar_bridge_routing_rule", map[string]any{"action": "move", "name": "voice_weather", "before": ""})
	if got := c.engine.Decide(t.Context(), "погода завтра"); got.Rule != "weather" {
		t.Fatalf("priority change ignored: %+v", got)
	}
	rule["route"] = "home"
	routingToolCall(t, tools, "set_quasar_bridge_routing_rule", map[string]any{"rule": rule, "before": "weather"})
	cfg, _ = readConfig(c.path)
	if cfg.Routing.Rules[0].Route != "home" || cfg.Routing.Rules[1].Name != "weather" {
		t.Fatal("updating one rule lost ordering or neighbors")
	}
	routingToolCall(t, tools, "control_quasar_bridge_routing_rule", map[string]any{"action": "delete", "name": "voice_weather"})
	cfg, err = readConfig(c.path)
	if err != nil || !reflect.DeepEqual(cfg, original) {
		t.Fatalf("delete did not restore original semantics: %+v %v", cfg, err)
	}
}

func TestRoutingToolsRejectStaleRevisionAndInvalidChanges(t *testing.T) {
	c, tools := routingToolsFixture(t)
	old := routingRevisionFromTool(t, tools)
	routingToolCall(t, tools, "set_quasar_bridge_routing_rule", map[string]any{"rule": map[string]any{"name": "custom", "route": "llm", "match": map[string]any{"contains_any": []string{"апельсин"}}}})
	before, _ := os.ReadFile(c.path)
	backup, _ := os.ReadFile(c.path + ".voice-backup")
	for _, args := range []string{
		`{"expected_revision":"` + old + `","action":"delete","name":"weather"}`,
		`{"expected_revision":"` + routingRevisionFromTool(t, tools) + `","action":"delete","name":"not-found"}`,
		`{"expected_revision":"` + routingRevisionFromTool(t, tools) + `","action":"move","name":"custom","before":"not-found"}`,
	} {
		if _, err := tools.Call(t.Context(), "control_quasar_bridge_routing_rule", args); err == nil {
			t.Fatalf("invalid/stale routing mutation accepted: %s", args)
		}
	}
	args, _ := json.Marshal(map[string]any{"expected_revision": routingRevisionFromTool(t, tools), "rule": map[string]any{"name": "bad", "route": "llm", "match": map[string]any{"regex": "["}}})
	if _, err := tools.Call(t.Context(), "set_quasar_bridge_routing_rule", string(args)); err == nil {
		t.Fatal("invalid regex accepted")
	}
	after, _ := os.ReadFile(c.path)
	afterBackup, _ := os.ReadFile(c.path + ".voice-backup")
	if string(before) != string(after) || string(backup) != string(afterBackup) {
		t.Fatal("rejected mutation changed configuration or backup")
	}
	if got := c.engine.Decide(t.Context(), "апельсин"); got.Rule != "custom" {
		t.Fatal("rejected mutation changed active policy")
	}
}

func TestRoutingChangePreservesOtherPendingSettings(t *testing.T) {
	c, tools := routingToolsFixture(t)
	_, err := tools.Call(t.Context(), "set_quasar_bridge_config", `{"changes":{"llm_model":"new"}}`)
	if err != nil {
		t.Fatal(err)
	}
	value := routingToolCall(t, tools, "set_quasar_bridge_routing_rule", map[string]any{"rule": map[string]any{"name": "custom", "route": "llm", "match": map[string]any{"contains_any": []string{"апельсин"}}}})
	if got := c.engine.Decide(t.Context(), "апельсин"); got.Rule != "custom" {
		t.Fatalf("routing blocked by unrelated pending settings: %+v", got)
	}
	cfg, _ := readConfig(c.path)
	if cfg.LLMModel != "new" || c.active.LLMModel != "old" {
		t.Fatal("pending backend setting lost or falsely applied")
	}
	raw, _ := json.Marshal(value)
	var result struct {
		RestartRequired bool     `json:"restart_required"`
		Pending         []string `json:"pending"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.RestartRequired || len(result.Pending) != 1 || result.Pending[0] != "llm_model" {
		t.Fatalf("pending backend settings not reported: %s", raw)
	}
}

func TestRoutingPreviewDoesNotExecuteClassifierOrChangeSettings(t *testing.T) {
	c, tools := routingToolsFixture(t)
	before, _ := os.ReadFile(c.path)
	value, err := tools.Call(t.Context(), "test_quasar_bridge_routing", `{"text":"погода завтра"}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	var result struct {
		Active struct {
			Route string `json:"route"`
			Rule  string `json:"rule"`
		} `json:"active"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Active.Route != "yandex" || result.Active.Rule != "weather" {
		t.Fatalf("wrong static preview: %s", raw)
	}
	after, _ := os.ReadFile(c.path)
	if string(before) != string(after) {
		t.Fatal("preview changed configuration")
	}
	_, err = tools.Call(t.Context(), "set_quasar_bridge_config", `{"changes":{"routing.default_route":"classify"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(t.Context(), "reload"); err != nil {
		t.Fatal(err)
	}
	c.engine.LLM = forbiddenRoutingClassifier{}
	value, err = tools.Call(t.Context(), "test_quasar_bridge_routing", `{"text":"совершенно другой текст"}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(value)
	if json.Unmarshal(raw, &result) != nil || result.Active.Route != "classify" {
		t.Fatalf("preview ran/fabricated classifier decision: %s", raw)
	}
}

type forbiddenRoutingClassifier struct{}

func (forbiddenRoutingClassifier) Complete(context.Context, []llm.Message, bool) (string, error) {
	panic("static preview invoked a classifier")
}

func TestRoutingMutationDoesNotApplyOtherPendingRoutingChanges(t *testing.T) {
	c, tools := routingToolsFixture(t)
	if _, err := tools.Call(t.Context(), "set_quasar_bridge_config", `{"changes":{"routing.default_route":"llm"}}`); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(c.path)
	args, _ := json.Marshal(map[string]any{"expected_revision": routingRevisionFromTool(t, tools), "action": "disable", "name": "weather"})
	if _, err := tools.Call(t.Context(), "control_quasar_bridge_routing_rule", string(args)); err == nil {
		t.Fatal("an unrelated saved routing change was implicitly applied")
	}
	after, _ := os.ReadFile(c.path)
	if string(before) != string(after) || c.active.Routing.DefaultRoute != "yandex" {
		t.Fatal("pending routing configuration was modified or applied")
	}
}

func TestRoutingOptionsSwitchAndDefaultApplyAtomically(t *testing.T) {
	c, tools := routingToolsFixture(t)
	// A pending backend change must stay pending while routing is applied.
	if _, err := tools.Call(t.Context(), "set_quasar_bridge_config", `{"changes":{"llm_model":"new"}}`); err != nil {
		t.Fatal(err)
	}
	configure := func(options map[string]any) {
		t.Helper()
		routingToolCall(t, tools, "configure_quasar_bridge_routing", options)
	}
	configure(map[string]any{"classifier_enabled": false, "default_route": "llm"})
	if got := c.engine.Decide(t.Context(), "совершенно новый вопрос"); got.Route != "llm" || got.Source != "default" {
		t.Fatalf("default not applied: %+v", got)
	}
	if got := c.engine.Decide(t.Context(), "погода завтра"); got.Rule != "weather" || got.Route != "yandex" {
		t.Fatalf("default replaced explicit rule: %+v", got)
	}
	configure(map[string]any{"default_route": "yandex"})
	if got := c.engine.Decide(t.Context(), "другой новый вопрос"); got.Route != "yandex" || got.Source != "default" {
		t.Fatalf("default not reassigned: %+v", got)
	}
	configure(map[string]any{"classifier_enabled": true})
	value, err := tools.Call(t.Context(), "test_quasar_bridge_routing", `{"text":"другой новый вопрос"}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	var result struct {
		Active struct {
			Route string `json:"route"`
		} `json:"active"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Active.Route != "classify" {
		t.Fatalf("classifier not restored: %s", raw)
	}
	configure(map[string]any{"classifier_enabled": false})
	if got := c.engine.Decide(t.Context(), "ещё один вопрос"); got.Route != "yandex" {
		t.Fatalf("switch lost chosen default: %+v", got)
	}
	cfg, err := readConfig(c.path)
	if err != nil || cfg.LLMModel != "new" || c.active.LLMModel != "old" || len(cfg.Routing.Rules) != 2 {
		t.Fatalf("other settings or rules changed: %+v %v", cfg, err)
	}
}

func TestRoutingOptionsRejectInvalidInputWithoutWriting(t *testing.T) {
	c, tools := routingToolsFixture(t)
	before, _ := os.ReadFile(c.path)
	revision := routingRevisionFromTool(t, tools)
	for _, args := range []string{
		`{"expected_revision":"` + revision + `"}`,
		`{"expected_revision":"` + revision + `","default_route":"classify"}`,
		`{"expected_revision":"` + revision + `","default_route":"invalid","classifier_enabled":false}`,
		`{"expected_revision":"` + revision + `","classifier_enabled":"false"}`,
		`{"expected_revision":"outdated","classifier_enabled":false}`,
	} {
		if _, err := tools.Call(t.Context(), "configure_quasar_bridge_routing", args); err == nil {
			t.Fatalf("invalid routing options accepted: %s", args)
		}
	}
	after, _ := os.ReadFile(c.path)
	if string(before) != string(after) {
		t.Fatal("invalid options changed the configuration")
	}
}

func TestRoutingOptionsMaterializeLegacyModeWithoutTogglingIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options map[string]any
		enabled bool
		route   string
	}{
		{"new default preserves classifier", map[string]any{"default_route": "home"}, true, "home"},
		{"disable preserves legacy fallback", map[string]any{"classifier_enabled": false}, false, "llm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := configurationFixture(t, `{"routing":{"default_route":"classify","classifier":{"on_error":"llm"}}}`)
			tools := &assistant.Tools{Config: assistant.DefaultConfig(), Configuration: c}
			routingToolCall(t, tools, "configure_quasar_bridge_routing", tc.options)
			cfg, err := readConfig(c.path)
			if err != nil || cfg.Routing.Classifier.Enabled == nil || cfg.Routing.ClassifierEnabled() != tc.enabled || cfg.Routing.DefaultRoute != tc.route {
				t.Fatalf("legacy mode changed unexpectedly: %+v %v", cfg.Routing, err)
			}
		})
	}
}
