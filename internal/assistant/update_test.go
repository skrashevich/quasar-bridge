package assistant

import (
	"context"
	"testing"

	"quasarbridge/internal/update"
)

type testUpdater struct{ installs int }

func (u *testUpdater) Check(context.Context) (update.Status, error) {
	return update.Status{LatestVersion: "1.2.4", UpdateAvailable: true}, nil
}
func (u *testUpdater) Install(context.Context) (update.InstallResult, error) {
	u.installs++
	return update.InstallResult{Status: "scheduled", Version: "1.2.4"}, nil
}

func TestUpdateToolsAreAvailableAndRejectUnexpectedArguments(t *testing.T) {
	updater := new(testUpdater)
	tools := &Tools{Config: DefaultConfig(), Updater: updater}
	names := map[string]bool{}
	for _, def := range tools.Definitions() {
		names[def.Function.Name] = true
	}
	for _, name := range []string{"check_quasar_bridge_update", "install_quasar_bridge_update"} {
		if !names[name] {
			t.Fatalf("missing %s", name)
		}
		for _, args := range []string{`{"url":"https://example.org/evil"}`, `null`, `[]`} {
			if _, err := tools.Call(t.Context(), name, args); err == nil {
				t.Fatalf("%s accepted %s", name, args)
			}
		}
	}
	if updater.installs != 0 {
		t.Fatal("invalid input started an installation")
	}
	result, err := tools.Call(t.Context(), "check_quasar_bridge_update", `{}`)
	if err != nil || !result.(update.Status).UpdateAvailable {
		t.Fatalf("check = %v, %v", result, err)
	}
	result, err = tools.Call(t.Context(), "install_quasar_bridge_update", `{}`)
	if err != nil || result.(update.InstallResult).Status != "scheduled" || updater.installs != 1 {
		t.Fatalf("install = %v, %v", result, err)
	}
	tools.Config.Updates = false
	for _, name := range []string{"check_quasar_bridge_update", "install_quasar_bridge_update"} {
		if _, err := tools.Call(t.Context(), name, `{}`); err == nil {
			t.Fatal("disabled tool executed")
		}
		for _, def := range tools.Definitions() {
			if def.Function.Name == name {
				t.Fatal("disabled tool advertised")
			}
		}
	}
}

func TestUpdateToolsRespectHistoryPrivacy(t *testing.T) {
	tools := &Tools{Config: DefaultConfig(), Updater: new(testUpdater)}
	agent := &Agent{Tools: tools}
	mcpTools := &mcpRequestTools{Tools: tools}
	for _, name := range []string{"check_quasar_bridge_update", "install_quasar_bridge_update"} {
		if agent.allowedAfterHistory(name) || mcpTools.AllowedAfterHistory(name) {
			t.Fatalf("network tool %s allowed after private history", name)
		}
	}
}
