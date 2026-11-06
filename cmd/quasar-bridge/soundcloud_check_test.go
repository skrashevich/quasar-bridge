package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSoundCloudProxyInput(t *testing.T) {
	for _, input := range []string{"socks5://proxy.example.invalid:1080\n", "socks5h://user:password@proxy.example.invalid:1080\r\n", "socks5://proxy.example.invalid:1080"} {
		got, err := readSoundCloudProxy(strings.NewReader(input))
		if err != nil || got != strings.TrimRight(input, "\r\n") {
			t.Fatalf("valid proxy input rejected: %v", err)
		}
	}
	for _, input := range []string{"", "\n", "http://secret-user:secret-password@proxy.example.invalid:1080\n", "socks5://" + strings.Repeat("x", 4096) + ":1080\n"} {
		if _, err := readSoundCloudProxy(strings.NewReader(input)); err == nil || strings.Contains(err.Error(), "secret-") {
			t.Fatal("invalid proxy input accepted or credentials exposed")
		}
	}
}

func TestSoundCloudCheckCommand(t *testing.T) {
	if os.Getenv("QUASAR_SC_CHECK_CHILD") == "1" {
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", os.Getenv("QUASAR_SC_CHECK_PATH"), os.Getenv("QUASAR_SC_CHECK_FLAG")}
		main()
		return
	}
	for _, candidate := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "candidate override"}[candidate], func(t *testing.T) {
			proxyURL, targets := rejectingSoundCloudProxy(t)
			path := filepath.Join(t.TempDir(), "config.json")
			original := []byte(`{"mode":"observe","soundcloud":{"api":"web","client_id":"fixture","proxy_url":"socks5://old.example.invalid:1080"}}`)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			kind := "-check-soundcloud"
			if candidate {
				kind = "-check-soundcloud-proxy"
			}
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSoundCloudCheckCommand$")
			cmd.Env = append(os.Environ(), "QUASAR_SC_CHECK_CHILD=1", "QUASAR_SC_CHECK_PATH="+path, "QUASAR_SC_CHECK_FLAG="+kind, "QUASAR_SOUNDCLOUD_PROXY_URL=socks5://old-env.example.invalid:1080")
			if candidate {
				cmd.Stdin = strings.NewReader(proxyURL + "\n")
			}
			output, err := cmd.CombinedOutput()
			if candidate {
				if err == nil || !strings.Contains(string(output), "SoundCloud") {
					t.Fatalf("proxy refusal was not reported: %v, %s", err, output)
				}
				select {
				case got := <-targets:
					if got != "api-v2.soundcloud.com" {
						t.Fatalf("candidate probe target = %q", got)
					}
				case <-time.After(time.Second):
					t.Fatal("check ignored the proxy supplied on stdin")
				}
			} else if err != nil || !strings.Contains(string(output), "выключен") {
				t.Fatalf("disabled SoundCloud should skip network: %v, %s", err, output)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != string(original) {
				t.Fatal("diagnostic check changed the saved configuration")
			}
		})
	}
}
