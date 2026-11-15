package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpeakerEndpointPreservesOtherConfigAndRollback(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "quasar.cfg")
	backup := filepath.Join(dir, "backup.json")
	original := `{"aliced":{"uniProxyUrl":"wss://previous.example.invalid/uni.ws","uniProxyQuicUrl":"quic://previous.example.invalid","other":3},"unknown":{"keep":true}}`
	if err := os.WriteFile(cfg, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	local := "ws://127.0.0.1:8443/uni.ws"
	if err := setSpeakerEndpoint(cfg, backup, local, "apply"); err != nil {
		t.Fatal(err)
	}
	if err := setSpeakerEndpoint(cfg, backup, local, "apply"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfg)
	if !strings.Contains(string(data), local) || !strings.Contains(string(data), `"keep":true`) {
		t.Fatal("bad patch")
	}
	// An unrelated firmware/config update must survive rollback.
	data = []byte(strings.Replace(string(data), `"other":3`, `"other":4`, 1))
	os.WriteFile(cfg, data, 0644)
	if err := setSpeakerEndpoint(cfg, backup, local, "rollback"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(cfg)
	if !strings.Contains(string(data), "previous.example.invalid") || !strings.Contains(string(data), `"other":4`) {
		t.Fatal("bad rollback")
	}
}

func TestSpeakerEndpointRejectsExternalQUICChange(t *testing.T) {
	for _, action := range []string{"apply", "rollback"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			cfg := filepath.Join(dir, "quasar.cfg")
			backup := filepath.Join(dir, "backup.json")
			local := "ws://127.0.0.1:8443/uni.ws"
			original := `{"aliced":{"uniProxyUrl":"wss://previous.example.invalid/uni.ws","uniProxyQuicUrl":"quic://previous.example.invalid"}}`
			if err := os.WriteFile(cfg, []byte(original), 0644); err != nil {
				t.Fatal(err)
			}
			if err := setSpeakerEndpoint(cfg, backup, local, "apply"); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(cfg)
			raw = []byte(strings.Replace(string(raw), `"uniProxyQuicUrl":""`, `"uniProxyQuicUrl":"quic://changed.example.invalid"`, 1))
			os.WriteFile(cfg, raw, 0644)
			if setSpeakerEndpoint(cfg, backup, local, action) == nil {
				t.Fatal("external QUIC change overwritten")
			}
			after, _ := os.ReadFile(cfg)
			if string(raw) != string(after) {
				t.Fatal("config mutated on rejection")
			}
		})
	}
}

func TestAndroidSpeakerEndpointPreservesBackendAndOtherConfig(t *testing.T) {
	dir := t.TempDir()
	cfg, backup := filepath.Join(dir, "quasar.cfg"), filepath.Join(dir, "backup.json")
	original := `{"common":{"backendUrl":"https://quasar.yandex.net/","keep":{"field":true}},"aliced":{"uniProxyUrl":"wss://uniproxy.alice.yandex.net/uni.ws","uniProxyQuicUrl":"quic://original","other":3},"unknown":{"keep":true}}`
	if err := os.WriteFile(cfg, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	local := "ws://127.0.0.1:8443/uni.ws"
	if err := setSpeakerEndpointForPlatform(cfg, backup, local, "apply", true); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), `"backend":"https://quasar.yandex.net/"`) || !strings.Contains(string(saved), `"backend_local":"http://127.0.0.1:8443/android-backend"`) {
		t.Fatalf("backend not recorded: %s", saved)
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("backup permissions", err)
	}
	if err := setSpeakerEndpointForPlatform(cfg, backup, local, "apply", true); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(backup); string(after) != string(saved) {
		t.Fatal("idempotent apply replaced original backup")
	}
	data, _ := os.ReadFile(cfg)
	if !strings.Contains(string(data), `"backendUrl":"http://127.0.0.1:8443/android-backend"`) || !strings.Contains(string(data), `"keep":{"field":true}`) || !strings.Contains(string(data), `"unknown":{"keep":true}`) {
		t.Fatalf("bad Android patch: %s", data)
	}
	data = []byte(strings.Replace(string(data), `"other":3`, `"other":4`, 1))
	if err := os.WriteFile(cfg, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := setSpeakerEndpointForPlatform(cfg, backup, local, "rollback", true); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(cfg)
	if !strings.Contains(string(data), `"backendUrl":"https://quasar.yandex.net/"`) || !strings.Contains(string(data), `"uniProxyQuicUrl":"quic://original"`) || !strings.Contains(string(data), `"other":4`) {
		t.Fatalf("bad Android rollback: %s", data)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatal("backup remains after rollback", err)
	}
}

func TestAndroidSpeakerEndpointRejectsExternalBackendChange(t *testing.T) {
	for _, action := range []string{"apply", "rollback"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			cfg, backup := filepath.Join(dir, "quasar.cfg"), filepath.Join(dir, "backup.json")
			original := `{"common":{"backendUrl":"https://quasar.yandex.net"},"aliced":{"uniProxyUrl":"wss://original/uni.ws"}}`
			if err := os.WriteFile(cfg, []byte(original), 0644); err != nil {
				t.Fatal(err)
			}
			local := "ws://127.0.0.1:8443/uni.ws"
			if err := setSpeakerEndpointForPlatform(cfg, backup, local, "apply", true); err != nil {
				t.Fatal(err)
			}
			saved, _ := os.ReadFile(backup)
			data, _ := os.ReadFile(cfg)
			data = []byte(strings.Replace(string(data), "http://127.0.0.1:8443/android-backend", "https://changed.invalid", 1))
			if err := os.WriteFile(cfg, data, 0644); err != nil {
				t.Fatal(err)
			}
			if err := setSpeakerEndpointForPlatform(cfg, backup, local, action, true); err == nil {
				t.Fatal("external backend change overwritten")
			}
			if after, _ := os.ReadFile(cfg); string(after) != string(data) {
				t.Fatal("configuration mutated on rejection")
			}
			if after, _ := os.ReadFile(backup); string(after) != string(saved) {
				t.Fatal("backup mutated on rejection")
			}
		})
	}
}

func TestAndroidSpeakerEndpointRejectsUnexpectedBackendBeforeWriting(t *testing.T) {
	for _, common := range []string{``, `,"common":null`, `,"common":{}`, `,"common":{"backendUrl":""}`, `,"common":{"backendUrl":"https://other.invalid"}`, `,"common":{"backendUrl":"http://quasar.yandex.net"}`, `,"common":{"backendUrl":"https://user@quasar.yandex.net"}`, `,"common":{"backendUrl":"https://quasar.yandex.net/path"}`, `,"common":{"backendUrl":"https://quasar.yandex.net?secret=marker"}`, `,"common":{"backendUrl":"https://quasar.yandex.net:443"}`} {
		t.Run(common, func(t *testing.T) {
			dir := t.TempDir()
			cfg, backup := filepath.Join(dir, "quasar.cfg"), filepath.Join(dir, "backup.json")
			original := `{"aliced":{"uniProxyUrl":"wss://original/uni.ws"}` + common + `}`
			if err := os.WriteFile(cfg, []byte(original), 0644); err != nil {
				t.Fatal(err)
			}
			err := setSpeakerEndpointForPlatform(cfg, backup, "ws://127.0.0.1:8443/uni.ws", "apply", true)
			if err == nil {
				t.Fatal("unexpected backend accepted")
			}
			if strings.Contains(err.Error(), "marker") || strings.Contains(err.Error(), "other.invalid") {
				t.Fatal("raw endpoint included in error")
			}
			if after, _ := os.ReadFile(cfg); string(after) != original {
				t.Fatal("configuration mutated on rejection")
			}
			if _, err := os.Stat(backup); !os.IsNotExist(err) {
				t.Fatal("backup created on rejection", err)
			}
		})
	}
}

func TestSpeakerEndpointBackupCannotCrossPlatforms(t *testing.T) {
	for _, android := range []bool{false, true} {
		t.Run(fmt.Sprint(android), func(t *testing.T) {
			dir := t.TempDir()
			cfg, backup := filepath.Join(dir, "quasar.cfg"), filepath.Join(dir, "backup.json")
			original := `{"common":{"backendUrl":"https://quasar.yandex.net"},"aliced":{"uniProxyUrl":"wss://original/uni.ws"}}`
			if err := os.WriteFile(cfg, []byte(original), 0644); err != nil {
				t.Fatal(err)
			}
			local := "ws://127.0.0.1:8443/uni.ws"
			if err := setSpeakerEndpointForPlatform(cfg, backup, local, "apply", android); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(cfg)
			for _, action := range []string{"apply", "rollback"} {
				if err := setSpeakerEndpointForPlatform(cfg, backup, local, action, !android); err == nil {
					t.Fatal("platform mismatch accepted")
				}
				if after, _ := os.ReadFile(cfg); string(after) != string(data) {
					t.Fatal("configuration mutated on platform mismatch")
				}
			}
		})
	}
}

func TestAndroidSpeakerEndpointDerivesIPv6TLSBackend(t *testing.T) {
	dir := t.TempDir()
	cfg, backup := filepath.Join(dir, "quasar.cfg"), filepath.Join(dir, "backup.json")
	original := `{"common":{"backendUrl":"https://quasar.yandex.net"},"aliced":{"uniProxyUrl":"wss://original/uni.ws"}}`
	if err := os.WriteFile(cfg, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	if err := setSpeakerEndpointForPlatform(cfg, backup, "wss://[::1]:8443/uni.ws", "apply", true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfg)
	if !strings.Contains(string(data), `"backendUrl":"https://[::1]:8443/android-backend"`) {
		t.Fatal("bad TLS/IPv6 backend derivation")
	}
}
