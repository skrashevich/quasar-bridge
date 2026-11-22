package bridge

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skrashevich/quasar-bridge/internal/llm"
)

func commandScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test command")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeRoutingCommandFromRuleAndClassifier(t *testing.T) {
	const answer = "Ответ программы."
	for _, source := range []string{"rule", "classifier"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("QUASAR_TEST_PRIVATE", "secret")
			input := filepath.Join(t.TempDir(), "requests")
			script := commandScript(t, `[ -z "${QUASAR_TEST_PRIVATE-}" ]
cat >> "$1"
printf 'diagnostic only\n' >&2
printf '%s\n' "$2"`)
			cfg := DefaultRoutingConfig()
			cfg.Commands = map[string]RoutingCommand{"beacon": {Path: script, Args: []string{input, answer}}}
			cfg.Rules = nil
			if source == "rule" {
				cfg.Rules = []RoutingRule{{Name: "beacon_rule", Route: "command", Command: "beacon", Match: RoutingMatch{ContainsAny: []string{"маяк"}}}}
			} else {
				cfg.Classifier.Commands = map[string]string{"beacon": "вопрос о маяке"}
			}
			e := &Engine{LLM: completionFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
				if source != "classifier" || !structured || !strings.Contains(messages[0].Content, `"beacon":"вопрос о маяке"`) || strings.Contains(messages[0].Content, script) {
					t.Error("unexpected classifier call or command catalog")
				}
				return `{"route":"command","command":"beacon"}`, nil
			})}
			if err := e.SetRouting(cfg); err != nil {
				t.Fatal(err)
			}
			n, up, down := soundSession(t, e)
			sendMilestoneVoice(t, n, "voice-command", "wake-tag", nil)
			if text := airPlayListeningResponse(t, down).GetVoiceResponse().GetOutputSpeech().GetText(); text != answer {
				t.Fatalf("speaker response = %q", text)
			}
			for {
				event := nextSoundFrame(t, up).Client.GetEvent()
				if event.GetContinueRequest() != nil {
					t.Fatal("command request also continued in Yandex")
				}
				if speech := event.GetTtsGenerate(); speech != nil {
					if speech.GetText() != answer {
						t.Fatalf("TtsGenerate = %q", speech.GetText())
					}
					break
				}
			}
			raw, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}
			var got commandInput
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("stdin must contain one JSON document: %v", err)
			}
			if got.Version != 1 || got.Text != "Расскажи про маяк" || got.Command != "beacon" || got.Source != source || got.RequestID != "voice-command" || got.InputType != "VoiceInput" || got.WakeWordTag != "wake-tag" || len(got.SpeakerKey) != 64 {
				t.Fatalf("stdin = %s", raw)
			}
			if (source == "rule" && got.Rule != "beacon_rule") || (source == "classifier" && got.Rule != "") {
				t.Fatalf("stdin rule = %q", got.Rule)
			}
		})
	}
}

func TestCommandDecisionUsesApprovedSnapshot(t *testing.T) {
	// /bin/cat echoes the JSON document. Neither text nor classifier output becomes argv.
	cfg := DefaultRoutingConfig()
	cfg.Commands = map[string]RoutingCommand{"echo": {Path: "/bin/cat"}, "rule_only": {Path: "/bin/false"}}
	cfg.Classifier.Commands = map[string]string{"echo": "echo request"}
	cfg.Rules = nil
	reply := `{"route":"command","command":"echo"}`
	e := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) { return reply, nil })}
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	phrase := "Текст \"в кавычках\"\n$HOME; (два слова)"
	selected := e.Decide(t.Context(), phrase)
	cfg.Commands["echo"] = RoutingCommand{Path: "/bin/false"}
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	result, err := e.ResolveDecision(t.Context(), phrase, selected)
	var input commandInput
	if err != nil || json.Unmarshal([]byte(result.Answer), &input) != nil || input.Text != phrase {
		t.Fatalf("lost command snapshot or transcript: %+v %v", result, err)
	}
	for _, invalid := range []string{
		`{"route":"command"}`, `{"route":"command","command":"missing"}`,
		`{"route":"command","command":"rule_only"}`, `{"route":"command","command":"echo","args":["extra"]}`,
	} {
		reply = invalid
		d := e.Decide(t.Context(), phrase)
		if d.Route != cfg.Classifier.OnError || d.Source != "classifier_error" || d.handler != nil {
			t.Errorf("accepted classifier choice %s", reply)
		}
	}
}

func TestCommandOutputAndDeadline(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"failed", "echo partial; echo private-detail >&2; exit 7"},
		{"empty", "printf '  \\n'"},
		{"invalid UTF-8", "printf '\\377'"},
		{"overflow", "head -c 16385 /dev/zero"},
		{"timeout", "sleep 30 & wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := RoutingCommand{Path: commandScript(t, tc.body), TimeoutMS: 3000}
			if tc.name == "timeout" {
				cmd.TimeoutMS = 100
			}
			start := time.Now()
			answer, err := cmd.run(t.Context(), "test", RouteDecision{Command: "test", Source: "rule"})
			if err == nil || answer != "" || strings.Contains(err.Error(), "private-detail") {
				t.Fatalf("unexpected command result: %q %v", answer, err)
			}
			if tc.name == "overflow" && !errors.Is(err, errCommandOutputLimit) {
				t.Fatalf("output limit: %v", err)
			}
			if tc.name == "timeout" && (!errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second) {
				t.Fatalf("deadline: %v, elapsed %s", err, time.Since(start))
			}
		})
	}
}

func TestCommandRouteRejectsInvalidConfiguration(t *testing.T) {
	for _, settings := range []string{
		`{"commands":{"bad id":{"path":"/bin/cat"}}}`,
		`{"commands":{"test":{"path":"relative"}}}`,
		`{"commands":{"test":{"path":"/bin/cat","args":["arg\u0000"]}}}`,
		`{"commands":{"test":{"path":"/bin/cat","timeout_ms":60001}}}`,
		`{"classifier":{"commands":{"missing":"test"}}}`,
		`{"commands":{"test":{"path":"/bin/cat"}},"classifier":{"commands":{"test":" "}}}`,
		`{"rules":[{"name":"test","route":"command","match":{"contains_any":["test"]}}]}`,
		`{"rules":[{"name":"test","route":"command","command":"missing","match":{"contains_any":["test"]}}]}`,
		`{"rules":[{"name":"test","route":"llm","command":"test","match":{"contains_any":["test"]}}]}`,
	} {
		cfg := DefaultRoutingConfig()
		if err := json.Unmarshal([]byte(settings), &cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := CompileRouting(cfg); err == nil {
			t.Errorf("accepted %s", settings)
		}
	}
}
