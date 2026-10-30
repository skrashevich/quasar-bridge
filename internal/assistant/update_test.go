package assistant

import (
	"context"
	"errors"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
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

func TestShortUpdateCommandsUsePrefetchedCheck(t *testing.T) {
	tools := &Tools{Config: DefaultConfig(), Updater: new(testUpdater)}
	for _, text := range []string{"Проверь обновления", "Есть ли обновления?", "Алиса, проверь обновления, пожалуйста!", "Спроси нейросеть: проверь обновления"} {
		t.Run(text, func(t *testing.T) {
			modelCalls := 0
			agent := &Agent{Tools: tools, MaxCalls: 4, MaxRounds: 3, Client: chatFunc(func(context.Context, []llm.Message, []llm.Tool) (llm.Message, error) {
				modelCalls++
				return llm.Message{}, errors.New("simple update check must not call the model")
			})}
			if reply, err := agent.Complete(t.Context(), []llm.Message{{Role: "user", Content: text}}, false); err != nil || modelCalls != 0 || !strings.Contains(reply, "1.2.4") {
				t.Fatalf("calls=%d error=%v", modelCalls, err)
			}
		})
	}
	for _, text := range []string{"Не проверяй обновления", "В истории найди проверь обновления", "Проверь обновления датчика", "Проверь обновления прошивки", "Проверь обновления и включи свет", "Установи обновления"} {
		for _, call := range tools.InitialCalls(text) {
			if call.Name == "check_quasar_bridge_update" || call.Name == "install_quasar_bridge_update" {
				t.Errorf("unexpected update prefetch for %q: %s", text, call.Name)
			}
		}
	}
	tools.Config.Updates = false
	for _, call := range tools.InitialCalls("Проверь обновления") {
		if call.Name == "check_quasar_bridge_update" {
			t.Fatal("disabled update prefetched")
		}
	}
}

type updateReplyFixture struct {
	testUpdater
	status update.Status
	err    error
}

func (u *updateReplyFixture) Check(context.Context) (update.Status, error) { return u.status, u.err }

func TestSimpleUpdateCheckRepliesWithoutModelAndDistinguishesUnknownVersion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status update.Status
		err    error
		want   string
	}{
		{name: "available", status: update.Status{CurrentVersion: "1.2.3", CurrentVersionKnown: true, LatestVersion: "1.2.4", UpdateAvailable: true}, want: "Есть новая версия"},
		{name: "current", status: update.Status{CurrentVersion: "1.2.4", CurrentVersionKnown: true, LatestVersion: "1.2.4"}, want: "Более новой стабильной версии нет"},
		{name: "development", status: update.Status{CurrentVersion: "dev", LatestVersion: "1.2.4"}, want: "сравнить версии автоматически нельзя"},
		{name: "unreachable", err: errors.New("offline"), want: "Не удалось проверить обновления"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := &Tools{Config: DefaultConfig(), Updater: &updateReplyFixture{status: tc.status, err: tc.err}}
			agent := &Agent{Tools: tools, MaxCalls: 4, MaxRounds: 3, Client: chatFunc(func(context.Context, []llm.Message, []llm.Tool) (llm.Message, error) {
				t.Error("simple update check reached model")
				return llm.Message{Content: "Обновлений нет"}, nil
			})}
			reply, err := agent.Complete(t.Context(), []llm.Message{{Role: "user", Content: "Проверь обновления"}}, false)
			if err != nil || !strings.Contains(reply, tc.want) {
				t.Fatalf("reply=%q, error=%v", reply, err)
			}
			if tc.name == "development" && strings.Contains(strings.ToLower(reply), "обновлений нет") {
				t.Fatal("unknown build reported up to date")
			}
		})
	}
}
