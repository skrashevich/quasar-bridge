package main

import (
	"os"
	"strings"
	"testing"

	"encoding/json/v2"
	"path/filepath"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/journal"
)

func TestReloadLLMSettingsRequiresRestart(t *testing.T) {
	path := configFile(t, `{"llm_api":"responses","routing":{"rules":[],"default_route":"yandex"}}`)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := new(bridge.Engine)
	if err := engine.SetRouting(initial.Routing); err != nil {
		t.Fatal(err)
	}
	revision := engine.RoutingStatus().Revision
	for _, settings := range []string{
		`"llm_api":"chat_completions"`,
		`"llm_api":"responses","llm_reasoning_effort":"low"`,
		`"llm_api":"responses","llm_max_output_tokens":8000`,
		`"llm_api":"responses","llm_hosted_tools":["web_search"]`,
	} {
		body := `{` + settings + `,"routing":{"rules":[],"default_route":"llm"}}`
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := reloadRouting(path, initial, engine); err == nil || !strings.Contains(err.Error(), "restart required") {
			t.Fatalf("changed LLM settings accepted: %s: %v", settings, err)
		}
		if engine.RoutingStatus().Revision != revision {
			t.Fatal("rejected LLM change altered routing")
		}
	}
}

func TestReloadListenAfterReplyRequiresRestart(t *testing.T) {
	path := configFile(t, `{}`)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := new(bridge.Engine)
	if err := engine.SetRouting(initial.Routing); err != nil {
		t.Fatal(err)
	}
	revision := engine.RoutingStatus().Revision
	if err := os.WriteFile(path, []byte(`{"listen_after_reply":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reloadRouting(path, initial, engine); err == nil || !strings.Contains(err.Error(), "restart required") {
		t.Fatalf("changed listen_after_reply accepted: %v", err)
	}
	if engine.RoutingStatus().Revision != revision {
		t.Fatal("rejected change altered routing")
	}
}

func TestReloadRoutingKeepsLastGoodSnapshot(t *testing.T) {
	t.Setenv("QUASAR_HOME_ASSISTANT_TOKEN", "token")
	path := configFile(t, `{"routing":{"rules":[],"default_route":"yandex"}}`)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := &bridge.Engine{}
	if err := engine.SetRouting(initial.Routing); err != nil {
		t.Fatal(err)
	}
	original := engine.RoutingStatus().Revision
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"routing":{"rules":[],"default_route":"llm"}}`)
	if err := reloadRouting(path, initial, engine); err != nil {
		t.Fatal(err)
	}
	good := engine.RoutingStatus().Revision
	if good == original || engine.Decide(t.Context(), "anything").Route != "llm" {
		t.Fatal("policy not installed")
	}
	for _, bad := range []string{
		`{"routing":{"rules":[],"default_route":"invalid"}}`,
		`{"routing":{"rules":[{"name":"bad","route":"home","match":{"regex":"["}}]}}`,
		`{"listen":"localhost:9999","routing":{"rules":[],"default_route":"home"}}`,
		`{"llm_system_prompt":"Комната «Зал».","routing":{"rules":[],"default_route":"home"}}`,
		`{"home_assistant":{"url":"http://ha.example.invalid"},"routing":{"rules":[],"default_route":"home"}}`,
		`{"routing":`,
	} {
		write(bad)
		if err := reloadRouting(path, initial, engine); err == nil {
			t.Fatal("bad reload accepted")
		}
		if engine.RoutingStatus().Revision != good {
			t.Fatal("bad reload changed policy")
		}
	}
}

func TestReloadJournalKeepsWorkingPolicyOnOpenFailure(t *testing.T) {
	path := configFile(t, `{"routing":{"rules":[],"default_route":"yandex"}}`)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := new(bridge.Engine)
	engine.SetRouting(initial.Routing)
	logs := new(journal.Writer)
	defer logs.Close()
	logs.Configure(initial.ConversationLog)
	next := initial
	next.ConversationLog.Enabled = true
	next.ConversationLog.Path = filepath.Join(t.TempDir(), "turns.jsonl")
	write := func() {
		t.Helper()
		raw, err := json.Marshal(next)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if err := reloadRouting(path, initial, engine, logs); err != nil {
		t.Fatal(err)
	}
	if !logs.Enabled() {
		t.Fatal("not enabled")
	}
	old := engine.RoutingStatus().Revision
	next.Routing.DefaultRoute = "llm"
	next.ConversationLog.Path = filepath.Dir(next.ConversationLog.Path)
	write()
	if reloadRouting(path, initial, engine, logs) == nil {
		t.Fatal("bad path accepted")
	}
	if !logs.Enabled() || engine.RoutingStatus().Revision != old {
		t.Fatal("failed reload changed good state")
	}
}
