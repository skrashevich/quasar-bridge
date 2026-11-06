package update

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quasarbridge/internal/reply"
)

func TestSaveObservePreservesConfigurationAndBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := []byte(`{"mode":"hybrid","llm_model":"","unknown":{"secret":"keep","number":9007199254740993},"assistant_tools":{"enabled":false}}`)
	if err := os.WriteFile(path, original, 0640); err != nil {
		t.Fatal(err)
	}
	restore, err := saveObserve(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]jsontext.Value
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["mode"]) != `"observe"` || string(doc["unknown"]) != `{"secret":"keep","number":9007199254740993}` || string(doc["assistant_tools"]) != `{"enabled":false}` || string(doc["llm_model"]) != `""` {
		t.Fatalf("configuration fields changed: %s", body)
	}
	backup, err := os.ReadFile(path + ".recovery-backup")
	if err != nil || string(backup) != string(original) {
		t.Fatalf("original backup not preserved: %s, %v", backup, err)
	}
	info, err := os.Stat(path + ".recovery-backup")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions: %v, %v", info, err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(path)
	if string(body) != string(original) {
		t.Fatalf("failed scheduling did not restore original: %s", body)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "runtime.env")); !os.IsNotExist(err) {
		t.Fatalf("runtime environment created by failed recovery remains: %v", err)
	}
}

func TestSaveObserveRejectsUnsafeConfiguration(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "oversize", "array", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			body := `{"mode":"hybrid"}`
			switch kind {
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				body = strings.Repeat(" ", 1<<20+1)
			case "array":
				body = `[]`
			case "invalid":
				body = `{`
			}
			if kind != "symlink" && kind != "directory" {
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := saveObserve(path); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			if _, err := os.Stat(path + ".recovery-backup"); !os.IsNotExist(err) {
				t.Fatalf("backup created for rejected recovery: %v", err)
			}
		})
	}
}

func TestRecoveryRejectsUnmanagedInstallationAndInvalidAction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const original = `{"mode":"hybrid"}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	u := New(path)
	for _, action := range []string{"observe", "rollback", "restart", "rollback; reboot", ""} {
		if _, err := u.Recover(t.Context(), action); err == nil {
			t.Fatalf("unmanaged or invalid recovery %q accepted", action)
		}
	}
	body, _ := os.ReadFile(path)
	if string(body) != original {
		t.Fatalf("rejected recovery changed config: %s", body)
	}
}

func TestSaveObserveOverridesRuntimeModeAndPreservesSecrets(t *testing.T) {
	for _, assignment := range []string{`QUASAR_MODE="hybrid"`, `export QUASAR_MODE='hybrid'`, `QUASAR_MODE=$(printf hybrid)`} {
		t.Run(assignment, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			const original = `{"mode":"hybrid"}`
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("QUASAR_MODE", "hybrid")
			envPath := filepath.Join(filepath.Dir(path), "runtime.env")
			originalEnv := "SECRET='dummy-secret'\n" + assignment + "\n"
			if err := os.WriteFile(envPath, []byte(originalEnv), 0600); err != nil {
				t.Fatal(err)
			}
			restore, err := saveObserve(path)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := os.ReadFile(envPath)
			if !strings.HasPrefix(string(body), originalEnv) || !strings.HasSuffix(string(body), "QUASAR_MODE='observe'\n") {
				t.Fatalf("recovery lost environment or did not override mode: %s", body)
			}
			backup, err := os.ReadFile(envPath + ".recovery-backup")
			if err != nil || string(backup) != originalEnv {
				t.Fatalf("environment backup: %s, %v", backup, err)
			}
			command := exec.CommandContext(t.Context(), "sh", "-c", `. "$1"; printf '%s|%s' "$QUASAR_MODE" "$SECRET"`, "recovery-env-test", envPath)
			if output, err := command.Output(); err != nil || string(output) != "observe|dummy-secret" {
				t.Fatalf("saved runtime environment did not enable observe: %s, %v", output, err)
			}
			if err := restore(); err != nil {
				t.Fatal(err)
			}
			body, _ = os.ReadFile(envPath)
			if string(body) != originalEnv {
				t.Fatalf("original environment not restored: %s", body)
			}
		})
	}
}

func TestSaveObserveRejectsUnsafeRuntimeEnvironmentBeforeSaving(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			const original = `{"mode":"hybrid"}`
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			envPath := filepath.Join(filepath.Dir(path), "runtime.env")
			switch kind {
			case "symlink":
				if err := os.Symlink(path, envPath); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(envPath, 0700); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(envPath, []byte(strings.Repeat(" ", 1<<20+1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := saveObserve(path); err == nil {
				t.Fatal("unsafe runtime environment accepted")
			}
			body, _ := os.ReadFile(path)
			if string(body) != original {
				t.Fatalf("rejected recovery changed configuration: %s", body)
			}
		})
	}
}

func TestSaveObserveDoesNotOverwriteConcurrentConfigurationOnRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mode":"hybrid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	restore, err := saveObserve(path)
	if err != nil {
		t.Fatal(err)
	}
	const external = `{"mode":"observe","llm_model":"external"}`
	if err := os.WriteFile(path, []byte(external), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restore(); err == nil {
		t.Fatal("restore overwrote concurrent edits")
	}
	body, _ := os.ReadFile(path)
	if string(body) != external {
		t.Fatalf("concurrent edits lost: %s", body)
	}
}

func TestRollbackWaitsForDeliveryAndRebootsOnlyOnSuccess(t *testing.T) {
	for _, succeed := range []bool{true, false} {
		t.Run(map[bool]string{true: "success", false: "failure"}[succeed], func(t *testing.T) {
			state := t.TempDir()
			app := filepath.Join(state, "app")
			if err := os.Mkdir(app, 0700); err != nil {
				t.Fatal(err)
			}
			code := "exit 1\n"
			if succeed {
				code = "exit 0\n"
			}
			control := "#!/bin/sh\ntest \"$1\" = rollback || exit 3\nprintf rolledback > \"$QUASAR_DATA_DIR/result\"\n" + code
			if err := os.WriteFile(filepath.Join(app, "control.sh"), []byte(control), 0700); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "reboot"), []byte("#!/bin/sh\nprintf rebooted > \"$QUASAR_DATA_DIR/rebooted\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			lock := filepath.Join(state, "lock")
			if err := os.Mkdir(lock, 0700); err != nil {
				t.Fatal(err)
			}
			delivery := reply.WithPolicy(t.Context())
			ctx, cancel := context.WithCancel(delivery)
			defer cancel()
			done, err := launchServiceAction(ctx, state, lock, "rollback")
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			time.Sleep(100 * time.Millisecond)
			if _, err := os.Stat(filepath.Join(state, "result")); !os.IsNotExist(err) {
				t.Fatal("rollback started before answer delivery")
			}
			reply.Delivered(delivery)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("rollback helper did not finish")
			}
			if body, err := os.ReadFile(filepath.Join(state, "result")); err != nil || string(body) != "rolledback" {
				t.Fatalf("rollback not invoked: %s, %v", body, err)
			}
			_, err = os.Stat(filepath.Join(state, "rebooted"))
			if succeed && err != nil || !succeed && !os.IsNotExist(err) {
				t.Fatalf("unexpected reboot after rollback (success=%v): %v", succeed, err)
			}
			if _, err := os.Stat(lock); !os.IsNotExist(err) {
				t.Fatalf("operation lock retained: %v", err)
			}
		})
	}
}
