package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSoundCloudConfigDefaultsEnvironmentAndOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, body, base string
		enabled          bool
	}{
		{"disabled", `{}`, "", false},
		{"loopback", `{"soundcloud":{"enabled":true}}`, "http://localhost:8443", true},
		{"ipv6", `{"listen":"[::1]:8080","soundcloud":{"enabled":true}}`, "http://[::1]:8080", true},
		{"external", `{"listen":"0.0.0.0:8080","soundcloud":{"enabled":true,"base_url":"http://192.0.2.5:8080"}}`, "http://192.0.2.5:8080", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseConfig(soundCloudHybridJSON(t, tc.body), false)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.SoundCloud.Enabled != tc.enabled || cfg.SoundCloud.API != "auto" || cfg.SoundCloud.QueueLimit != 50 || cfg.SoundCloud.BaseURL != tc.base {
				t.Fatalf("defaults/origin incorrect: enabled=%v api=%q queue=%d base=%q", cfg.SoundCloud.Enabled, cfg.SoundCloud.API, cfg.SoundCloud.QueueLimit, cfg.SoundCloud.BaseURL)
			}
		})
	}
	for _, field := range []string{"CLIENT_ID", "CLIENT_SECRET", "ACCESS_TOKEN", "REFRESH_TOKEN", "TOKEN_FILE"} {
		t.Setenv("QUASAR_SOUNDCLOUD_"+field, "test-"+field)
	}
	cfg, err := parseConfig([]byte(`{}`), false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg.SoundCloud)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	for _, field := range []string{"CLIENT_ID", "CLIENT_SECRET", "ACCESS_TOKEN", "REFRESH_TOKEN", "TOKEN_FILE"} {
		if got[strings.ToLower(field)] != "test-"+field {
			t.Errorf("environment field %s not applied", field)
		}
	}
}
func TestSoundCloudConfigRejectsUnreachableOrInvalidOrigin(t *testing.T) {
	for _, body := range []string{
		`{"listen":"0.0.0.0:8080","soundcloud":{"enabled":true}}`,
		`{"listen":":8080","soundcloud":{"enabled":true}}`,
		`{"tls_cert":"cert","tls_key":"key","soundcloud":{"enabled":true}}`,
		`{"soundcloud":{"enabled":true,"base_url":"https://example.com/prefix"}}`,
		`{"soundcloud":{"enabled":true,"api":"bad"}}`,
		`{"soundcloud":{"enabled":true,"queue_limit":101}}`,
	} {
		if _, err := parseConfig(soundCloudHybridJSON(t, body), false); err == nil {
			t.Errorf("invalid configuration accepted: %s", body)
		}
	}
	// Disabled SoundCloud must not impose new listener constraints.
	if _, err := parseConfig([]byte(`{"listen":":8080"}`), false); err != nil {
		t.Fatal(err)
	}
}

func soundCloudHybridJSON(t *testing.T, body string) []byte {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	fields["mode"] = "hybrid"
	fields["llm_base_url"] = "https://llm.example.invalid/v1"
	fields["llm_model"] = "test"
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestSoundCloudRequiresHybrid(t *testing.T) {
	if _, err := parseConfig([]byte(`{"soundcloud":{"enabled":true}}`), false); err == nil {
		t.Fatal("enabled provider accepted observe mode")
	}
}
func TestSoundCloudStartsIndependentlyOfAssistantTools(t *testing.T) {
	cfg, err := parseConfig(soundCloudHybridJSON(t, `{"soundcloud":{"enabled":true},"assistant_tools":{"enabled":false}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	provider, relay, err := newSoundCloudProvider(cfg)
	if err != nil || provider == nil || relay == nil {
		t.Fatalf("provider=%v relay=%v err=%v", provider != nil, relay != nil, err)
	}
	// An unknown resource reaches the relay and returns an ordinary local 404.
	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	response, err := server.Client().Get(server.URL + "/soundcloud/absent")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("relay status=%d", response.StatusCode)
	}

	cfg.SoundCloud.ClientSecret = "private-secret"
	cfg.SoundCloud.AccessToken = "private-token"
	b, _ := json.Marshal(soundCloudHealth(cfg, true))
	if strings.Contains(string(b), "private-") {
		t.Fatal("health exposed credentials")
	}
	cfg.SoundCloud.Enabled = false
	if p, r, err := newSoundCloudProvider(cfg); p != nil || r != nil || err != nil {
		t.Fatal("disabled provider instantiated")
	}
}
