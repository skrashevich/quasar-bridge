package main

import (
	"encoding/json/v2"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSoundCloudSetupCommand(t *testing.T) {
	if os.Getenv("QUASAR_SC_SETUP_CHILD") == "1" {
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", os.Getenv("QUASAR_SC_SETUP_CONFIG"), "-soundcloud-setup", "compare-soundcloud"}
		main()
		return
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:8443","mode":"observe","llm_model":"fixture","llm_base_url":"http://localhost/v1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSoundCloudSetupCommand$")
	cmd.Env = append(os.Environ(), "QUASAR_SC_SETUP_CHILD=1", "QUASAR_SC_SETUP_CONFIG="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("setup command: %v %s", err, output)
	}
	cfg, err := readConfig(path)
	if err != nil || !cfg.SoundCloud.Enabled || !cfg.SoundCloud.Default || !cfg.SoundCloud.CompareYandex || cfg.SoundCloud.API != "web" {
		t.Fatalf("CLI did not save SoundCloud selection: %+v %v", cfg.SoundCloud, err)
	}
}

func TestSoundCloudSetupPreservesOtherSettings(t *testing.T) {
	for _, tc := range []struct {
		mode                     string
		enabled, prefer, compare bool
	}{
		{"off", false, false, false}, {"explicit", true, false, false},
		{"default", true, true, false}, {"compare-yandex", true, false, true},
		{"compare-soundcloud", true, true, true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			raw := []byte(`{"listen":"127.0.0.1:8443","mode":"hybrid","llm_model":"fixture","llm_base_url":"http://localhost/v1","soundcloud":{"api":"official","client_id":"kept","client_secret":"secret","queue_limit":12,"base_url":"http://localhost:8443","proxy_url":"socks5h://proxy.example.invalid:1080"},"airplay":{"name":"kept"}}`)
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
			if err := configureSoundCloud(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			cfg, err := readConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			sc := cfg.SoundCloud
			if sc.Enabled != tc.enabled || sc.Default != tc.prefer || sc.CompareYandex != tc.compare || sc.API != "official" || sc.ClientID != "kept" || sc.ClientSecret != "secret" || sc.QueueLimit != 12 || sc.BaseURL != "http://localhost:8443" || sc.ProxyURL != "socks5h://proxy.example.invalid:1080" || cfg.AirPlay.Name != "kept" || cfg.LLMModel != "fixture" {
				t.Fatalf("settings changed: %+v", cfg)
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0600 {
				t.Fatal("configuration permissions are not private")
			}
		})
	}
}

func TestSoundCloudSetupNewPublicConfigAndInvalidNoWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"listen":"127.0.0.1:8443","mode":"observe","llm_model":"fixture","llm_base_url":"http://localhost/v1"}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := configureSoundCloud(path, "bogus"); err == nil {
		t.Fatal("accepted invalid mode")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("invalid mode changed configuration")
	}
	if err := configureSoundCloud(path, "compare-soundcloud"); err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(path)
	var cfg config
	if err := json.Unmarshal(after, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "hybrid" || cfg.SoundCloud.API != "web" || !cfg.SoundCloud.Enabled || !cfg.SoundCloud.Default || !cfg.SoundCloud.CompareYandex {
		t.Fatalf("wrong public config: %+v", cfg)
	}
}
