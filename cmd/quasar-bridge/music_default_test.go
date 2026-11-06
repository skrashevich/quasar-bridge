package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"strings"
	"testing"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/soundcloud"
)

func musicDefaultFixture(t *testing.T) (*configurationControl, *soundcloud.Provider) {
	t.Helper()
	c := configurationFixture(t, `{"mode":"hybrid","llm_base_url":"https://model.example.invalid/v1","llm_model":"test","soundcloud":{"enabled":true,"api":"web","client_secret":"private-secret"},"routing":{"default_route":"yandex"}}`)
	p := soundcloud.NewProvider(c.active.SoundCloud, nil, nil)
	c.engine.SoundCloud = p
	c.engine.MusicPreferences = c
	return c, p
}

func TestVoiceMusicDefaultPersistsAndAppliesWithoutRestart(t *testing.T) {
	c, p := musicDefaultFixture(t)
	ctx := dialogue.WithSpeaker(t.Context(), "test-speaker")
	if p.CanHandle(ctx, "включи песню Гусары") {
		t.Fatal("initial default is SoundCloud")
	}
	if got := c.engine.Decide(ctx, "включи SoundCloud по умолчанию"); got.Route != "music_preference" {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"включи SoundCloud по умолчанию", true},
		{"верни Яндекс Музыку по умолчанию", false},
	} {
		answer, err := c.engine.Resolve(ctx, tc.text)
		if err != nil || answer.Route != "music_preference" || !strings.Contains(answer.Answer, "по умолчанию") {
			t.Fatal(answer, err)
		}
		saved, err := readConfig(c.path)
		if err != nil || saved.SoundCloud.Default != tc.want || p.IsDefault() != tc.want || p.CanHandle(ctx, "включи песню Гусары") != tc.want {
			t.Fatalf("saved/live mismatch: %v %v", saved.SoundCloud.Default, err)
		}
		fresh := soundcloud.NewProvider(saved.SoundCloud, nil, nil)
		if fresh.IsDefault() != tc.want {
			t.Fatal("restart lost preference")
		}
		if c.result("ok", saved).RestartRequired {
			t.Fatal("default required restart")
		}
		status, err := c.engine.Resolve(ctx, "какой музыкальный сервис по умолчанию")
		if err != nil || !strings.Contains(status.Answer, map[bool]string{true: "SoundCloud", false: "Яндекс"}[tc.want]) {
			t.Fatal(status, err)
		}
		health, _ := json.Marshal(soundCloudHealth(saved, true, p))
		if !bytes.Contains(health, []byte(`"default":`)) || bytes.Contains(health, []byte("private-secret")) {
			t.Fatal(string(health))
		}
	}
	backup, err := os.ReadFile(c.path + ".voice-backup")
	if err != nil || !bytes.Contains(backup, []byte(`"default": true`)) {
		t.Fatal("missing backup", err)
	}
}

func TestVoiceMusicDefaultFailedSaveDoesNotApply(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		c, p := musicDefaultFixture(t)
		ctx := t.Context()
		if cancel {
			var stop context.CancelFunc
			ctx, stop = context.WithCancel(ctx)
			stop()
		} else {
			if err := os.Mkdir(c.path+".voice-backup", 0700); err != nil {
				t.Fatal(err)
			}
		}
		before, _ := os.ReadFile(c.path)
		answer, _ := c.MusicDefault(ctx, "soundcloud")
		after, _ := os.ReadFile(c.path)
		if p.IsDefault() || !bytes.Equal(before, after) || strings.Contains(answer, "Теперь") {
			t.Fatal("failed save applied preference", answer)
		}
	}
}

func TestVoiceMusicDefaultUnavailableAndUnsupported(t *testing.T) {
	c, _ := musicDefaultFixture(t)
	c.engine.SoundCloud = nil
	before, _ := os.ReadFile(c.path)
	for _, target := range []string{"soundcloud", "unsupported"} {
		answer, err := c.MusicDefault(t.Context(), target)
		if err != nil || answer == "" || strings.Contains(answer, "Теперь") {
			t.Fatal(answer, err)
		}
	}
	after, _ := os.ReadFile(c.path)
	if !bytes.Equal(before, after) {
		t.Fatal("unavailable selection changed config")
	}
}

func TestMusicDefaultGenericSettingReloadAndFailure(t *testing.T) {
	c, p := musicDefaultFixture(t)
	result, err := c.Set(t.Context(), map[string]jsontext.Value{"soundcloud.default": jsontext.Value("true")})
	if err != nil || result.(configurationResult).RestartRequired || p.IsDefault() {
		t.Fatal(result, err)
	}
	if _, err = c.Apply(t.Context(), "reload"); err != nil || !p.IsDefault() {
		t.Fatal(err)
	}
	value, err := c.Get(t.Context(), "soundcloud")
	encoded, _ := json.Marshal(value)
	if err != nil || bytes.Contains(encoded, []byte("private-secret")) || bytes.Contains(encoded, []byte("client_secret")) || !bytes.Contains(encoded, []byte(`"active":true`)) {
		t.Fatal(string(encoded), err)
	}
	c.engine.SoundCloud = nil
	if _, err = c.Apply(t.Context(), "reload"); err == nil {
		t.Fatal("reload pretended missing provider was active")
	}
}

func TestMusicDefaultDoesNotApplyOtherSavedSettings(t *testing.T) {
	c, p := musicDefaultFixture(t)
	if _, err := c.Set(t.Context(), map[string]jsontext.Value{"llm_model": jsontext.Value(`"pending"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MusicDefault(t.Context(), "soundcloud"); err != nil || !p.IsDefault() {
		t.Fatal(err)
	}
	saved, err := readConfig(c.path)
	if err != nil || saved.LLMModel != "pending" || c.active.LLMModel != "test" {
		t.Fatal("unrelated setting applied", err)
	}
	if _, err = c.Apply(t.Context(), "reload"); err == nil {
		t.Fatal("unrelated setting reload accepted")
	}
}

func TestMusicDefaultLateDecisionCannotOverwriteNewer(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "dialogue_enabled", false: "dialogue_disabled"}[enabled], func(t *testing.T) {
			c, p := musicDefaultFixture(t)
			dialogueConfig := dialogue.DefaultConfig()
			dialogueConfig.Enabled = enabled
			c.engine.Dialogue = dialogue.New(dialogueConfig)
			ctx := dialogue.WithSpeaker(t.Context(), "speaker")
			old := c.engine.Prepare(ctx)
			decision := c.engine.Decide(old, "верни Яндекс Музыку по умолчанию")
			if _, err := c.engine.Resolve(ctx, "включи SoundCloud по умолчанию"); err != nil {
				t.Fatal(err)
			}
			if _, err := c.engine.ResolveDecision(old, "верни Яндекс Музыку по умолчанию", decision); err == nil || !p.IsDefault() {
				t.Fatal("late default overwritten", err)
			}
		})
	}
}

func TestMusicDefaultOlderArrivalRejectedWithDialogueDisabled(t *testing.T) {
	c, p := musicDefaultFixture(t)
	// Direct controller calls model A paused after Engine.Current and before
	// c.mu, then B committing before A enters the controller.
	newer := sound.WithRequestSequence(t.Context(), 20)
	older := sound.WithRequestSequence(t.Context(), 10)
	if _, err := c.MusicDefault(newer, "soundcloud"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(c.path)
	if _, err := c.MusicDefault(older, "yandex"); err == nil {
		t.Fatal("older controller mutation accepted")
	}
	after, _ := os.ReadFile(c.path)
	if !p.IsDefault() || !bytes.Equal(before, after) {
		t.Fatal("old controller mutation changed default")
	}
	if _, err := c.MusicDefault(sound.WithRequestSequence(t.Context(), 30), "status"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MusicDefault(sound.WithRequestSequence(t.Context(), 25), "yandex"); err != nil || p.IsDefault() {
		t.Fatal("status incorrectly advanced mutation sequence", err)
	}
}

func TestMusicDefaultGetReportsUnavailableRuntimeFalse(t *testing.T) {
	c := configurationFixture(t, `{"soundcloud":{"enabled":false,"default":true}}`)
	value, err := c.Get(t.Context(), "soundcloud")
	encoded, _ := json.Marshal(value)
	if err != nil || !bytes.Contains(encoded, []byte(`"active":false`)) || !bytes.Contains(encoded, []byte(`"pending":true`)) {
		t.Fatal(string(encoded), err)
	}
}
