package main

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestSoundCloudComparisonConfigAndHealth(t *testing.T) {
	cfg, err := parseConfig(soundCloudHybridJSON(t, `{"soundcloud":{"enabled":true,"compare_yandex":true}}`), false)
	if err != nil || !cfg.SoundCloud.CompareYandex {
		t.Fatal(err)
	}
	if !soundCloudHealth(cfg, true)["compare_yandex"].(bool) {
		t.Fatal("comparison status absent")
	}
	if soundCloudHealth(cfg, false)["compare_yandex"].(bool) {
		t.Fatal("inactive provider reports active comparison")
	}
	cfg.SoundCloud.AccessToken = "private-token"
	cfg.SoundCloud.ClientSecret = "private-secret"
	b, _ := json.Marshal(soundCloudHealth(cfg, true))
	if strings.Contains(string(b), "private-") {
		t.Fatal("comparison health leaked credentials")
	}
	if _, err := parseConfig(soundCloudHybridJSON(t, `{"soundcloud":{"enabled":true,"compare_yandex":"yes"}}`), false); err == nil {
		t.Fatal("nonboolean option accepted")
	}
	plain, err := parseConfig(soundCloudHybridJSON(t, `{"soundcloud":{"enabled":true}}`), false)
	if err != nil || plain.SoundCloud.CompareYandex {
		t.Fatal("comparison must remain opt in")
	}
}
