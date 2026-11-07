package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"quasarbridge/internal/bridge"
)

func TestReadConfigWakewordRouteMarker(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{"wakeword_route_marker":"/tmp/wakeword-route"}`))
	if err != nil || cfg.WakewordRouteMarker != "/tmp/wakeword-route" {
		t.Fatalf("marker=%q err=%v", cfg.WakewordRouteMarker, err)
	}
	if cfg, err = readConfig(configFile(t, `{}`)); err != nil || cfg.WakewordRouteMarker != "" {
		t.Fatalf("default marker=%q err=%v", cfg.WakewordRouteMarker, err)
	}
	if _, err = readConfig(configFile(t, `{"wakeword_route_marker":"relative"}`)); err == nil {
		t.Fatal("relative marker accepted")
	}
}

func TestReadConfigLLMAPISettings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		api    string
		effort string
		tokens int
		tools  int
	}{
		{"defaults", `{}`, "chat_completions", "", 4096, 0},
		{"responses", `{"llm_api":"responses","llm_reasoning_effort":"low","llm_max_output_tokens":8000,"llm_hosted_tools":["web_search","code_interpreter"]}`, "responses", "low", 8000, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := readConfig(configFile(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LLMAPI != tc.api || cfg.LLMReasoningEffort != tc.effort || cfg.LLMMaxOutputTokens != tc.tokens || len(cfg.LLMHostedTools) != tc.tools {
				t.Fatalf("LLM settings: %+v", cfg)
			}
		})
	}
}

func TestReadConfigLLMAPIEnvironmentOverrides(t *testing.T) {
	t.Setenv("QUASAR_LLM_API", "responses")
	t.Setenv("QUASAR_LLM_REASONING_EFFORT", "low")
	cfg, err := readConfig(configFile(t, `{"llm_api":"chat_completions"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMAPI != "responses" || cfg.LLMReasoningEffort != "low" {
		t.Fatalf("env overrides: %+v", cfg)
	}
	t.Setenv("QUASAR_LLM_API", "")
	t.Setenv("QUASAR_LLM_REASONING_EFFORT", "")
	if _, err := readConfig(configFile(t, `{}`)); err != nil {
		t.Fatalf("empty API override should use default: %v", err)
	}
}

func TestReadConfigRejectsInvalidLLMAPISettings(t *testing.T) {
	for _, body := range []string{
		`{"llm_api":"completions"}`,
		`{"llm_api":"responses","llm_reasoning_effort":"fast"}`,
		`{"llm_reasoning_effort":"low"}`,
		`{"llm_api":"responses","llm_max_output_tokens":0}`,
		`{"llm_api":"responses","llm_max_output_tokens":-1}`,
		`{"llm_api":"responses","llm_hosted_tools":["unknown"]}`,
		`{"llm_api":"responses","llm_hosted_tools":["web_search","web_search"]}`,
		`{"llm_hosted_tools":["web_search"]}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := readConfig(configFile(t, body)); err == nil {
				t.Fatal("invalid LLM settings accepted")
			}
		})
	}
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if _, err := readConfig(configFile(t, `{"llm_api":"responses","llm_reasoning_effort":"`+effort+`"}`)); err != nil {
			t.Fatalf("effort %q rejected: %v", effort, err)
		}
	}
}

func configFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadConfigAcceptsListenAfterReply(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{}`, true},
		{`{"listen_after_reply":true}`, true},
		{`{"listen_after_reply":false}`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			cfg, err := readConfig(configFile(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ListenAfterReply != tc.want {
				t.Fatalf("listen_after_reply=%v, want %v", cfg.ListenAfterReply, tc.want)
			}
		})
	}
}
func TestReadConfigEnvironmentOverridesDeploymentSettings(t *testing.T) {
	t.Setenv("QUASAR_MODE", "hybrid")
	t.Setenv("QUASAR_LISTEN", "localhost:9443")
	t.Setenv("QUASAR_UPSTREAM", "wss://upstream.example.invalid/uni.ws")
	t.Setenv("QUASAR_TLS_CERT", "runtime/public.pem")
	t.Setenv("QUASAR_TLS_KEY", "runtime/private.key")
	t.Setenv("QUASAR_ALLOWED_IPS", "192.0.2.10, 2001:db8::10")
	t.Setenv("QUASAR_LLM_BASE_URL", "https://llm.example.invalid/v1")
	t.Setenv("QUASAR_LLM_MODEL", "configured-model")
	t.Setenv("QUASAR_HOME_MCP_URL", "https://home.example.invalid/mcp")
	t.Setenv("QUASAR_HOME_WARM_AREA", "Office")
	cfg, err := readConfig(configFile(t, `{"mode":"observe","llm_model":"previous"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "localhost:9443" || cfg.Upstream != "wss://upstream.example.invalid/uni.ws" || cfg.TLSCert != "runtime/public.pem" || cfg.TLSKey != "runtime/private.key" || cfg.Mode != "hybrid" || cfg.LLMModel != "configured-model" || cfg.HomeWarmArea != "Office" || len(cfg.AllowedIPs) != 2 {
		t.Fatalf("configuration override failed: %+v", cfg)
	}
}
func TestReadConfigRejectsInvalidAndUnknownSettings(t *testing.T) {
	for _, body := range []string{
		`{"unexpected":"value"}`, `{"mode":"unknown"}`, `{"tls_cert":"cert.pem"}`,
		`{"allowed_ips":["not-an-address"]}`, `{"upstream":"file:///tmp/socket"}`,
		`{"llm_base_url":"https://name:password@example.invalid/v1"}`,
		`{"mode":"hybrid"}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := readConfig(configFile(t, body)); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
func TestReadConfigHasGenericDefaults(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "observe" || !strings.HasPrefix(cfg.Listen, "localhost:") {
		t.Fatalf("defaults=%+v", cfg)
	}
}

func TestHybridHomeAssistantIsOptional(t *testing.T) {
	t.Setenv("QUASAR_HOME_MCP_URL", "")
	body := `{"mode":"hybrid","llm_base_url":"https://llm.example.invalid/v1","llm_model":"test"}`
	if _, err := readConfig(configFile(t, body)); err != nil {
		t.Fatalf("LLM-only hybrid rejected: %v", err)
	}
	for _, endpoint := range []string{"not-a-url", "file:///tmp/mcp", "https://user:secret@example.invalid/mcp"} {
		t.Setenv("QUASAR_HOME_MCP_URL", endpoint)
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Fatal("invalid configured HA endpoint accepted")
		}
	}
}

func TestRoutingDefaultsAndPartialOverlay(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{"routing":{"classifier":{"on_error":"llm"},"refusal":{"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routing.Rules) != len(bridge.DefaultRoutingConfig().Rules) || cfg.Routing.Classifier.TimeoutMS != 8000 || cfg.Routing.Classifier.OnError != "llm" || cfg.Routing.Refusal.Enabled {
		t.Fatalf("partial defaults lost: %+v", cfg.Routing)
	}
	empty, err := readConfig(configFile(t, `{"routing":{"rules":[],"default_route":"yandex"}}`))
	if err != nil || len(empty.Routing.Rules) != 0 {
		t.Fatalf("empty rules: %v", err)
	}
	for _, body := range []string{
		`{"routing":{"default_route":"invalid"}}`,
		`{"routing":{"rules":[{"name":"bad","route":"llm","match":{"regex":"["}}]}}`,
		`{"routing":{"rules":[{"name":"bad","route":"llm","match":{"typo":["x"]}}]}}`,
	} {
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Fatal("invalid routing accepted")
		}
	}
}

func TestRuleArrayReplacesDefaultElements(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{"routing":{"rules":[{"name":"custom","route":"llm","match":{"contains_any":["ping"]}}],"default_route":"yandex"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routing.Rules) != 1 || len(cfg.Routing.Rules[0].Match.ContainsAllGroups) != 0 {
		t.Fatal("custom rule inherited built-in conditions")
	}
	e := &bridge.Engine{}
	if err := e.SetRouting(cfg.Routing); err != nil {
		t.Fatal(err)
	}
	if got := e.Decide(t.Context(), "PING"); got.Route != "llm" || got.Rule != "custom" {
		t.Fatal(got)
	}
}

func TestExampleRoutingEqualsBackwardCompatibleDefaults(t *testing.T) {
	t.Setenv("QUASAR_HOME_MCP_URL", "https://home.example.invalid/mcp")
	cfg, err := readConfig("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Routing, bridge.DefaultRoutingConfig()) {
		t.Fatal("example defaults drifted from legacy policy")
	}
}

func TestLocalProgressConfiguration(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{"mode":"observe"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalProgress.Enabled || cfg.LocalProgress.AfterMS != 2000 || cfg.LocalProgress.IntervalMS != 4000 {
		t.Fatal("unexpected progress defaults")
	}
	for _, body := range []string{`{"mode":"observe","local_progress":{"enabled":true,"after_ms":99}}`, `{"mode":"observe","local_progress":{"enabled":true,"interval_ms":999}}`, `{"mode":"observe","local_progress":{"enabled":true,"after_ms":10001}}`} {
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Fatal("invalid progress timing accepted")
		}
	}
}

func TestDialogueContextDefaultsAndValidation(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{"mode":"observe"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DialogueContext.Enabled || cfg.DialogueContext.TTLSeconds != 300 || cfg.DialogueContext.MaxTurns != 6 {
		t.Fatal("wrong dialogue defaults")
	}
	if _, err := readConfig(configFile(t, `{"mode":"observe","dialogue_context":{"max_turns":0}}`)); err == nil {
		t.Fatal("invalid dialogue limit accepted")
	}
}

func TestMCPExampleAndUnknownConfiguration(t *testing.T) {
	t.Setenv("QUASAR_HOME_MCP_URL", "")
	cfg, err := readConfig("../../config.mcp.example.json")
	if err != nil || len(cfg.AssistantTools.MCPServers) != 1 || cfg.AssistantTools.MCPServers[0].Name != "parallel" {
		t.Fatal("invalid MCP example", err)
	}
	if got := (&bridge.Engine{}); got.SetRouting(cfg.Routing) != nil || got.Decide(t.Context(), "найди в интернете документацию").Route != "llm" {
		t.Fatal("MCP search phrase did not reach LLM")
	}
	if _, err := readConfig(configFile(t, `{"assistant_tools":{"enabled":true,"mcp_servers":[{"name":"x","url":"https://mcp.example.invalid","typo":true}]}}`)); err == nil {
		t.Fatal("unknown MCP option accepted")
	}
}
