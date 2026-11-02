package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/reply"
	"quasarbridge/internal/update"
)

func configurationFixture(t *testing.T, body string) *configurationControl {
	t.Helper()
	path := configFile(t, body)
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := &bridge.Engine{Configuration: true}
	if err := engine.SetRouting(cfg.Routing); err != nil {
		t.Fatal(err)
	}
	logs := new(journal.Writer)
	if err := logs.Configure(cfg.ConversationLog); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	return &configurationControl{path: path, active: cfg, engine: engine, logs: logs}
}

func TestConfigurationRejectsInvalidBatchWithoutWriting(t *testing.T) {
	for _, changes := range []map[string]jsontext.Value{
		{"listen_after_reply": jsontext.Value("false"), "dialogue_context.ttl_seconds": jsontext.Value("0")},
		{"listen_after_reply": jsontext.Value(`"false"`)},
		{"listen_after_reply": jsontext.Value("null")},
		{"llm_max_output_tokens": jsontext.Value("1.5")},
		{"llm_reasoning_effort": jsontext.Value(`"high"`)},
		{"assistant_tools": jsontext.Value(`{"mcp_servers":[{"command":"sh"}]}`)},
		{"assistant_tools.mcp_servers": jsontext.Value("[]")},
		{"airplay.aplay": jsontext.Value(`"sh"`)},
		{"tls_key": jsontext.Value(`"/secret"`)},
		{"routing.rules": jsontext.Value(`[{"name":"invalid","route":"llm","match":{"regex":"["}}]`)},
	} {
		c := configurationFixture(t, `{}`)
		before, _ := os.ReadFile(c.path)
		if _, err := c.Set(t.Context(), changes); err == nil {
			t.Errorf("invalid changes accepted: %s", changes)
		}
		after, _ := os.ReadFile(c.path)
		if !bytes.Equal(before, after) {
			t.Fatal("failed validation changed configuration")
		}
		if _, err := os.Stat(c.path + ".voice-backup"); !os.IsNotExist(err) {
			t.Fatal("failed validation replaced the backup")
		}
	}
}

func TestConfigurationPreservesUnrelatedFieldsAndBacksUp(t *testing.T) {
	const original = `{"llm_model":"old","airplay":{"name":"Kitchen","device":"hw:1"},"routing":{"rules":[],"default_route":"yandex"}}`
	c := configurationFixture(t, original)
	if _, err := c.Set(t.Context(), map[string]jsontext.Value{"airplay.name": jsontext.Value(`"Bedroom"`), "llm_model": jsontext.Value(`"new"`)}); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(c.path)
	if err != nil || cfg.AirPlay.Name != "Bedroom" || cfg.AirPlay.Device != "hw:1" || cfg.LLMModel != "new" || len(cfg.Routing.Rules) != 0 {
		t.Fatalf("lost unrelated configuration: %+v %v", cfg, err)
	}
	backup, err := os.ReadFile(c.path + ".voice-backup")
	if err != nil || string(backup) != original {
		t.Fatalf("wrong backup: %s %v", backup, err)
	}
	info, _ := os.Stat(c.path)
	if info.Mode().Perm()&0077 != 0 {
		t.Fatal("configuration permissions expose private settings")
	}
}

func TestConfigurationEnvironmentOverrideIsReadOnly(t *testing.T) {
	t.Setenv("QUASAR_LLM_MODEL", "environment-model")
	t.Setenv("QUASAR_LLM_API_KEY", "do-not-disclose")
	c := configurationFixture(t, `{"llm_model":"file-model","llm_base_url":"https://private.example/v1?key=private-key","assistant_tools":{"mcp_servers":[{"name":"private","url":"https://private.example/mcp?token=private-token","disabled":true}]}}`)
	if _, err := c.Set(t.Context(), map[string]jsontext.Value{"llm_model": jsontext.Value(`"ineffective"`)}); err == nil || !strings.Contains(err.Error(), "QUASAR_LLM_MODEL") {
		t.Fatalf("environment override accepted: %v", err)
	}
	value, err := c.Get(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	for _, secret := range []string{"do-not-disclose", "private-key", "private-token", "private.example"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("configuration exposed %s", secret)
		}
	}
	if !bytes.Contains(raw, []byte(`"saved":"file-model"`)) || !bytes.Contains(raw, []byte(`"effective":"environment-model"`)) {
		t.Fatalf("stored value lost under environment override: %s", raw)
	}
	var result struct {
		Settings []struct {
			Path     string `json:"path"`
			Writable bool   `json:"writable"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	for _, setting := range result.Settings {
		if setting.Path == "llm_model" && setting.Writable {
			t.Fatal("environment-controlled setting advertised as writable")
		}
	}
}

func TestConfigurationReportsPendingUntilRestart(t *testing.T) {
	c := configurationFixture(t, `{"llm_model":"old"}`)
	value, err := c.Set(t.Context(), map[string]jsontext.Value{"llm_model": jsontext.Value(`"new"`)})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(configurationResult)
	if !result.RestartRequired || len(result.Pending) != 1 || result.Pending[0] != "llm_model" {
		t.Fatalf("wrong pending state: %+v", result)
	}
	if _, err := c.Apply(t.Context(), "reload"); err == nil {
		t.Fatal("backend change was reported as hot-reloaded")
	}
	value, err = c.Get(t.Context(), "llm_model")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	if !bytes.Contains(raw, []byte(`"active":"old"`)) || !bytes.Contains(raw, []byte(`"saved":"new"`)) {
		t.Fatalf("saved change presented as active: %s", raw)
	}
	if _, err := c.Apply(t.Context(), "restart"); err == nil {
		t.Fatal("unavailable restart accepted")
	}
}

func TestConfigurationReloadFailureKeepsActiveState(t *testing.T) {
	logDir := t.TempDir()
	// An existing directory cannot be opened as the JSONL output file.
	c := configurationFixture(t, `{"conversation_log":{"path":`+strconv.Quote(logDir)+`},"routing":{"rules":[],"default_route":"yandex"}}`)
	if _, err := c.Set(t.Context(), map[string]jsontext.Value{"conversation_log.enabled": jsontext.Value("true"), "routing.default_route": jsontext.Value(`"llm"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(t.Context(), "reload"); err == nil {
		t.Fatal("invalid journal target applied")
	}
	if c.logs.Enabled() || c.engine.Decide(t.Context(), "test").Route != "yandex" {
		t.Fatal("partial reload changed active state")
	}
}

func TestConfigurationReloadAndRestartScheduling(t *testing.T) {
	c := configurationFixture(t, `{"routing":{"rules":[],"default_route":"yandex"}}`)
	if _, err := c.Set(t.Context(), map[string]jsontext.Value{"routing.default_route": jsontext.Value(`"llm"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(t.Context(), "reload"); err != nil {
		t.Fatal(err)
	}
	if c.engine.Decide(t.Context(), "test").Route != "llm" {
		t.Fatal("routing not installed")
	}
	if got := c.result("ok", c.active); got.RestartRequired || len(got.Pending) != 0 {
		t.Fatalf("applied changes still pending: %+v", got)
	}
	var restarts int
	done := make(chan struct{})
	c.restart = func(context.Context) (any, error) {
		restarts++
		return update.RestartResult{Status: "scheduled", Done: done}, nil
	}
	if _, err := c.Apply(t.Context(), "restart"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(t.Context(), "restart"); err == nil {
		t.Fatal("duplicate restart accepted")
	}
	if _, err := c.Set(t.Context(), map[string]jsontext.Value{"llm_model": jsontext.Value(`"too-late"`)}); err == nil {
		t.Fatal("configuration changed after restart scheduled")
	}
	if restarts != 1 {
		t.Fatal("restarted twice")
	}
	// The detached controller failed without stopping this process.
	close(done)
	if _, err := c.Set(t.Context(), map[string]jsontext.Value{"llm_model": jsontext.Value(`"retry"`)}); err != nil {
		t.Fatalf("failed restart permanently disabled settings: %v", err)
	}
}

func TestConfigurationRefusesSymlinkAndCancelledWrite(t *testing.T) {
	c := configurationFixture(t, `{}`)
	original := c.path
	c.path = filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(original, c.path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Set(t.Context(), map[string]jsontext.Value{"llm_model": jsontext.Value(`"new"`)}); err == nil {
		t.Fatal("symlink replaced")
	}
	c.path = original
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Set(ctx, map[string]jsontext.Value{"llm_model": jsontext.Value(`"new"`)}); err == nil {
		t.Fatal("cancelled write accepted")
	}
	if body, _ := os.ReadFile(original); string(body) != `{}` {
		t.Fatal("target modified")
	}
}

func TestConfigurationToolsValidateArgumentsAndDisable(t *testing.T) {
	c := configurationFixture(t, `{}`)
	tools := &assistant.Tools{Config: assistant.DefaultConfig(), Configuration: c}
	for _, call := range []struct{ name, args string }{
		{"get_quasar_bridge_config", `null`},
		{"get_quasar_bridge_config", `[]`},
		{"get_quasar_bridge_config", `{"section":10}`},
		{"get_quasar_bridge_config", `{"path":"/etc/passwd"}`},
		{"set_quasar_bridge_config", `{}`},
		{"set_quasar_bridge_config", `{"changes":{}}`},
		{"set_quasar_bridge_config", `{"changes":{"llm_model":"new"},"extra":true}`},
		{"apply_quasar_bridge_config", `{"action":"stop"}`},
		{"apply_quasar_bridge_config", `{"action":"restart","command":"sh"}`},
	} {
		if _, err := tools.Call(t.Context(), call.name, call.args); err == nil {
			t.Errorf("%s accepted %s", call.name, call.args)
		}
	}
	if body, _ := os.ReadFile(c.path); string(body) != `{}` {
		t.Fatal("malformed arguments modified configuration")
	}
	tools.Config.Configuration = false
	for _, name := range []string{"get_quasar_bridge_config", "set_quasar_bridge_config", "apply_quasar_bridge_config"} {
		if _, err := tools.Call(t.Context(), name, `{}`); err == nil {
			t.Fatal("disabled configuration tool executed")
		}
		for _, def := range tools.Definitions() {
			if def.Function.Name == name {
				t.Fatal("disabled configuration tool advertised")
			}
		}
	}
}

func TestConfigurationRestartSuppressesListeningOnlyWhenScheduled(t *testing.T) {
	c := configurationFixture(t, `{}`)
	tools := &assistant.Tools{Config: assistant.DefaultConfig(), Configuration: c}
	failed := reply.WithPolicy(t.Context())
	if _, err := tools.Call(failed, "apply_quasar_bridge_config", `{"action":"restart"}`); err == nil {
		t.Fatal("unavailable restart succeeded")
	}
	if !reply.ListeningAllowed(failed) {
		t.Fatal("failed restart disabled listening")
	}
	done := make(chan struct{})
	c.restart = func(context.Context) (any, error) { return update.RestartResult{Status: "scheduled", Done: done}, nil }
	success := reply.WithPolicy(t.Context())
	if _, err := tools.Call(success, "apply_quasar_bridge_config", `{"action":"restart"}`); err != nil {
		t.Fatal(err)
	}
	if reply.ListeningAllowed(success) {
		t.Fatal("restart reopened listening")
	}
	close(done)
}
