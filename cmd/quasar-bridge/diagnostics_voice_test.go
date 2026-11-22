package main

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestDiagnosticsVoiceTogglePreservesPendingConfiguration(t *testing.T) {
	c := configurationFixture(t, `{"llm_model":"active","routing":{"rules":[],"default_route":"yandex"}}`)
	var bound bool
	c.applyDiagnostics = func(enabled bool) error { bound = enabled; return nil }
	if _, err := c.Set(t.Context(), map[string]jsontext.Value{"llm_model": jsontext.Value(`"pending"`), "routing.default_route": jsontext.Value(`"llm"`)}); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		answer, err := c.SetDiagnosticsNetworkAccess(t.Context(), enabled)
		want := "Сетевой доступ к веб-интерфейсу выключен."
		if enabled {
			want = "Сетевой доступ к веб-интерфейсу включён."
		}
		if err != nil || answer != want || bound != enabled || c.diagnosticsNetworkAccess() != enabled {
			t.Fatalf("toggle=%t: answer=%q bound=%t err=%v", enabled, answer, bound, err)
		}
		saved, err := readConfig(c.path)
		if err != nil || saved.LLMModel != "pending" || saved.Routing.DefaultRoute != "llm" || saved.Diagnostics.NetworkAccess != enabled {
			t.Fatalf("pending saved configuration lost: %+v %v", saved, err)
		}
		if c.active.LLMModel != "active" || c.engine.Decide(t.Context(), "test").Route != "yandex" || c.active.Listen != "localhost:8443" {
			t.Fatal("toggle applied other pending configuration or changed listener default")
		}
	}
	if _, err := os.Stat(c.path + ".voice-backup"); err != nil {
		t.Fatal("direct toggle did not use configuration backup", err)
	}
}

func TestDiagnosticsVoiceToggleDoesNotClaimSuccessWithoutListener(t *testing.T) {
	c := configurationFixture(t, `{}`)
	before, _ := os.ReadFile(c.path)
	for _, enabled := range []bool{false, true} {
		if answer, err := c.SetDiagnosticsNetworkAccess(t.Context(), enabled); err == nil || answer != "" {
			t.Fatalf("toggle=%t claimed live success: %q %v", enabled, answer, err)
		}
	}
	after, _ := os.ReadFile(c.path)
	if string(after) != string(before) || c.diagnosticsNetworkAccess() {
		t.Fatal("unavailable live toggle changed configuration")
	}
}

func TestDiagnosticsVoiceToggleValidatesBeforeBinding(t *testing.T) {
	for _, reason := range []string{"invalid_config", "scheduled_restart", "canceled"} {
		t.Run(reason, func(t *testing.T) {
			c := configurationFixture(t, `{}`)
			c.applyDiagnostics = func(bool) error { t.Error("invalid toggle reached listener"); return nil }
			ctx := t.Context()
			switch reason {
			case "invalid_config":
				if err := os.WriteFile(c.path, []byte(`{"unsupported":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "scheduled_restart":
				c.restartDone = make(chan struct{})
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			before, _ := os.ReadFile(c.path)
			if answer, err := c.SetDiagnosticsNetworkAccess(ctx, true); err == nil || answer != "" {
				t.Fatalf("invalid operation succeeded: %q %v", answer, err)
			}
			after, _ := os.ReadFile(c.path)
			if string(after) != string(before) || c.diagnosticsNetworkAccess() {
				t.Fatal("invalid operation changed configuration")
			}
		})
	}
}

func TestDiagnosticsVoiceToggleBindFailureKeepsSavedAndActiveState(t *testing.T) {
	c := configurationFixture(t, `{}`)
	bindError := errors.New("bind failed")
	c.applyDiagnostics = func(bool) error { return bindError }
	if answer, err := c.SetDiagnosticsNetworkAccess(t.Context(), true); !errors.Is(err, bindError) || answer != "" {
		t.Fatalf("bind failure lost: %q %v", answer, err)
	}
	if raw, _ := os.ReadFile(c.path); string(raw) != `{}` || c.diagnosticsNetworkAccess() {
		t.Fatal("bind failure saved or activated network access")
	}
	if _, err := os.Stat(c.path + ".voice-backup"); !os.IsNotExist(err) {
		t.Fatal("bind failure wrote configuration backup")
	}
}

func TestDiagnosticsVoiceToggleSaveFailureRestoresBinding(t *testing.T) {
	for _, rollbackFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "restored", true: "rollback_error"}[rollbackFails], func(t *testing.T) {
			c := configurationFixture(t, `{}`)
			// A directory cannot be replaced by the atomic backup-file rename.
			if err := os.Mkdir(c.path+".voice-backup", 0700); err != nil {
				t.Fatal(err)
			}
			var bound bool
			var bindings []bool
			rollbackError := errors.New("restore failed")
			c.applyDiagnostics = func(enabled bool) error {
				bindings = append(bindings, enabled)
				if !enabled && rollbackFails {
					return rollbackError
				}
				bound = enabled
				return nil
			}
			answer, err := c.SetDiagnosticsNetworkAccess(t.Context(), true)
			if err == nil || answer != "" || !strings.Contains(err.Error(), "backup") || (rollbackFails && !errors.Is(err, rollbackError)) {
				t.Fatalf("save/rollback failure lost: %q %v", answer, err)
			}
			if !reflect.DeepEqual(bindings, []bool{true, false}) || (!rollbackFails && bound) {
				t.Fatalf("listener not restored: bindings=%v bound=%t", bindings, bound)
			}
			if raw, _ := os.ReadFile(c.path); string(raw) != `{}` || c.diagnosticsNetworkAccess() {
				t.Fatal("save failure changed saved or active access")
			}
		})
	}
}

func TestDiagnosticsVoiceToggleIdempotentDoesNotRewriteConfiguration(t *testing.T) {
	c := configurationFixture(t, `{}`)
	c.applyDiagnostics = func(bool) error { return nil }
	if _, err := c.SetDiagnosticsNetworkAccess(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(c.path); string(raw) != `{}` {
		t.Fatalf("default localhost command rewrote configuration: %s", raw)
	}
	if _, err := os.Stat(c.path + ".voice-backup"); !os.IsNotExist(err) {
		t.Fatal("idempotent command replaced backup")
	}
	if _, err := c.SetDiagnosticsNetworkAccess(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(c.path)
	backup, _ := os.ReadFile(c.path + ".voice-backup")
	if _, err := c.SetDiagnosticsNetworkAccess(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(c.path)
	afterBackup, _ := os.ReadFile(c.path + ".voice-backup")
	if string(after) != string(before) || string(afterBackup) != string(backup) || !c.diagnosticsNetworkAccess() {
		t.Fatal("repeated enable rewrote configuration or backup")
	}
}

func TestDiagnosticsVoiceToggleDetectsExternalEditWithoutSaving(t *testing.T) {
	c := configurationFixture(t, `{}`)
	if _, err := c.Set(t.Context(), map[string]jsontext.Value{"diagnostics.network_access": jsontext.Value("true")}); err != nil {
		t.Fatal(err)
	}
	const external = `{"diagnostics":{"network_access":false},"llm_model":"external"}`
	var bound bool
	c.applyDiagnostics = func(enabled bool) error {
		bound = enabled
		if enabled {
			return os.WriteFile(c.path, []byte(external), 0600)
		}
		return nil
	}
	if answer, err := c.SetDiagnosticsNetworkAccess(t.Context(), true); err == nil || answer != "" || !strings.Contains(err.Error(), "changed externally") {
		t.Fatalf("external edit falsely reported as applied: %q %v", answer, err)
	}
	if bound || c.diagnosticsNetworkAccess() {
		t.Fatal("external edit did not restore previous listener and active access")
	}
	if raw, _ := os.ReadFile(c.path); string(raw) != external {
		t.Fatalf("external configuration overwritten: %s", raw)
	}
	if backup, _ := os.ReadFile(c.path + ".voice-backup"); string(backup) != `{}` {
		t.Fatalf("conflict replaced previous backup: %s", backup)
	}
}
