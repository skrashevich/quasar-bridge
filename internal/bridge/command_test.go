package bridge

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
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

func TestCommandDiagnostics(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	ctx := context.WithValue(t.Context(), commandOriginKey{}, commandOrigin{RequestID: "private-native-id", logTag: "tag12345"})
	for _, tc := range []struct {
		name, program, script, stage string
		exitCode                     *int
	}{
		{"success", "/bin/sh", "echo response; echo success-diagnostic >&2", "", new(0)},
		{"shell syntax", "/bin/sh", "echo '", "wait", new(2)},
		{"program error", "/bin/sh", "echo problem >&2; echo 'Authorization: Bearer fixture-credential' >&2; echo private-transcript >&2; exit 7", "wait", new(7)},
		{"missing executable", filepath.Join(t.TempDir(), "missing"), "", "start", nil},
		{"no speech", "/bin/sh", "exit 0", "stdout", new(0)},
		{"signal", "/bin/sh", "kill -TERM $$", "wait", nil},
		{"deadline", "/bin/sh", "exec sleep 20", "wait", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			cmd := RoutingCommand{Path: tc.program, TimeoutMS: 3000}
			if tc.script != "" {
				cmd.Args = []string{"-c", tc.script}
			}
			if tc.name == "deadline" {
				cmd.TimeoutMS = 150
			}
			_, err := cmd.run(ctx, "private-transcript", RouteDecision{Command: "example", Rule: "test rule", Source: "rule"})
			if (err != nil) != (tc.stage != "") {
				t.Fatalf("unexpected command result %v", err)
			}
			for _, hidden := range []string{"private-transcript", "fixture-credential", "private-native-id", "success-diagnostic"} {
				if strings.Contains(logs.String(), hidden) {
					t.Fatal("unexpected private field or successful stderr in the command log")
				}
			}
			var events []map[string]any
			dec := jsontext.NewDecoder(&logs)
			for {
				var event map[string]any
				if err := json.UnmarshalDecode(dec, &event); err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					t.Fatal(err)
				}
				if event["command"] != "example" || event["source"] != "rule" || event["rule"] != "test rule" || event["program"] != tc.program || event["request"] != "tag12345" {
					t.Fatalf("missing invocation metadata: %v", event)
				}
				events = append(events, event)
			}
			if tc.stage == "start" {
				if len(events) != 1 || !strings.Contains(events[0]["error"].(string), "no such file") {
					t.Fatal(events)
				}
			} else if len(events) != 2 || events[0]["msg"] != "routing_command_started" || events[0]["pid"] != events[1]["pid"] {
				t.Fatal(events)
			}
			end := events[len(events)-1]
			if end["elapsed_ms"] == nil || tc.exitCode != nil && end["exit_code"] != float64(*tc.exitCode) {
				t.Fatal(end)
			}
			if tc.stage != "" {
				if end["level"] != "ERROR" || end["stage"] != tc.stage || end["msg"] != "routing_command_failed" {
					t.Fatal(end)
				}
			} else if end["msg"] != "routing_command_finished" || end["stderr"] != nil || end["stdout_bytes"] != float64(len("response\n")) {
				t.Fatal(end)
			}
			switch tc.name {
			case "program error":
				if !strings.Contains(end["stderr"].(string), "problem") {
					t.Fatal(end)
				}
			case "shell syntax":
				if end["stderr"] == nil || end["stderr"] == "" {
					t.Fatal(end)
				}
			case "signal":
				if end["signal"] == nil || end["exit_code"] != nil {
					t.Fatal(end)
				}
			case "deadline":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCommandStderrDrainsAndBoundsDiagnostics(t *testing.T) {
	var s commandStderr
	const first = "ошибка ввода\n"
	data := []byte(first + strings.Repeat("x", 10000))
	if n, err := s.Write(data); n != len(data) || err != nil {
		t.Fatal(n, err)
	}
	if n, err := s.Write(data); n != len(data) || err != nil {
		t.Fatal(n, err)
	}
	if len(s.data) > maxCommandStderr || s.total != int64(2*len(data)) || s.excerpt("", nil) != strings.TrimSpace(first) {
		t.Fatal("incorrect bounded stderr capture")
	}
	var echo commandStderr
	_, _ = echo.Write([]byte("problem with private-arg, private-request and private-text\nnext line"))
	if got := echo.excerpt("private-text", []string{"private-arg"}, "private-request"); got != "problem with *, * and * next line" {
		t.Fatal(got)
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
