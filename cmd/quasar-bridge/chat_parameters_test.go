package main

import (
	"encoding/json/v2"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/bridge"
)

func TestReadConfigChatParameters(t *testing.T) {
	for _, body := range []string{
		`{"llm_chat_parameters":{"temperature":null,"response_format":null,"chat_template_kwargs":null}}`,
		`{"llm_chat_parameters":{"temperature":0.7,"max_tokens":null,"max_completion_tokens":4096,"reasoning_effort":"low"}}`,
	} {
		cfg, err := readConfig(configFile(t, body))
		if err != nil {
			t.Fatalf("valid explicit chat parameters rejected: %v", err)
		}
		// Null must survive config parsing so the client can distinguish removal
		// from an omitted override, which retains the default.
		encoded, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var got, want map[string]any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(body), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got["llm_chat_parameters"], want["llm_chat_parameters"]) {
			t.Fatalf("chat parameters changed: %v", got["llm_chat_parameters"])
		}
	}
}

func TestReadConfigRejectsInvalidChatParameters(t *testing.T) {
	for _, body := range []string{
		`{"llm_chat_parameters":false}`,
		`{"llm_chat_parameters":{"tools":null}}`,
		`{"llm_chat_parameters":{"stream":true}}`,
		`{"llm_chat_parameters":{"messages":[]}}`,
		`{"llm_chat_parameters":{"model":"other"}}`,
		`{"llm_chat_parameters":{"functions":[]}}`,
		`{"llm_chat_parameters":{"unknown":null}}`,
		`{"llm_api":"responses","llm_chat_parameters":{"temperature":null}}`,
	} {
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Fatalf("invalid chat parameters accepted: %s", body)
		}
	}
	if _, err := readConfig(configFile(t, `{"llm_api":"responses","llm_chat_parameters":{}}`)); err != nil {
		t.Fatalf("empty parameters rejected for responses: %v", err)
	}
}

func TestReloadChatParametersRequiresRestart(t *testing.T) {
	path := configFile(t, `{}`)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := new(bridge.Engine)
	if err := os.WriteFile(path, []byte(`{"llm_chat_parameters":{"temperature":null}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reloadRouting(path, initial, engine); err == nil || !strings.Contains(err.Error(), "restart required") {
		t.Fatalf("changed chat parameters did not require restart: %v", err)
	}
}
