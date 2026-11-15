package main

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		ok         bool
	}{
		{"bridge", `{"status":"ok","mode":"observe"}`, 200, true},
		{"unrelated service", `hello`, 200, false},
		{"failed bridge", `{"status":"failed"}`, 200, false},
		{"HTTP failure", `{"status":"ok"}`, 503, false},
		{"redirect", ``, 302, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("wrong path: %s", r.URL.Path)
				}
				w.Header().Set("Location", "http://192.0.2.1/healthz")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			if err := checkHealth(t.Context(), s.URL+"/healthz"); (err == nil) != tc.ok {
				t.Fatalf("checkHealth error=%v, want success %v", err, tc.ok)
			}
		})
	}
}

func TestHealthcheckRejectsNonLoopback(t *testing.T) {
	for _, address := range []string{"http://example.com/healthz", "http://192.0.2.1/healthz", "https://127.0.0.1/healthz", "http://user:secret@127.0.0.1/healthz", "http://127.0.0.1/other", "http://127.0.0.1/healthz?key=secret"} {
		if err := checkHealth(t.Context(), address); err == nil {
			t.Fatalf("accepted invalid address %q", address)
		}
	}
}

// Android service scripts need this mode before installation/configuration and
// without relying on DNS, proxy variables or external HTTP clients.
func TestHealthcheckProcess(t *testing.T) {
	if address := os.Getenv("QUASAR_TEST_HEALTH_URL"); address != "" {
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", "/missing/config.json", "-healthcheck", address}
		main()
		os.Exit(0)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) }))
	defer s.Close()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHealthcheckProcess$")
	cmd.Env = append(os.Environ(), "QUASAR_TEST_HEALTH_URL="+s.URL+"/healthz", "QUASAR_DNS_SERVER=invalid-secret", "HTTP_PROXY=http://127.0.0.1:1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Bridge healthy") {
		t.Fatalf("health CLI failed without configuration: %v %s", err, out)
	}
}
