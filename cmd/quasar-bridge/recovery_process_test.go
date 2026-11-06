package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// A process-level check catches recovery registration accidentally moving into
// the assistant-tools block, and any LLM/classifier call preceding recovery.
func TestRecoveryProcessWithoutLLM(t *testing.T) {
	if os.Getenv("QUASAR_RECOVERY_CHILD") == "1" {
		os.Args = []string{"quasar-bridge", "-config", os.Getenv("QUASAR_RECOVERY_PATH"), "-query", os.Getenv("QUASAR_RECOVERY_QUERY")}
		main()
		return
	}
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "LLM offline", http.StatusServiceUnavailable)
	}))
	defer api.Close()
	for _, enabled := range []bool{false, true} {
		for _, query := range []string{"переведи quasar bridge в observe", "аварийный откат квазар бридж"} {
			t.Run(fmt.Sprintf("tools=%t/%s", enabled, query), func(t *testing.T) {
				path := configFile(t, fmt.Sprintf(`{"mode":"hybrid","airplay":{"disabled":true},"llm_base_url":%q,"llm_model":"offline","assistant_tools":{"enabled":%t}}`, api.URL, enabled))
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRecoveryProcessWithoutLLM$")
				for _, entry := range os.Environ() {
					if !strings.HasPrefix(entry, "QUASAR_") {
						child.Env = append(child.Env, entry)
					}
				}
				child.Env = append(child.Env, "QUASAR_RECOVERY_CHILD=1", "QUASAR_RECOVERY_PATH="+path, "QUASAR_RECOVERY_QUERY="+query)
				output, err := child.CombinedOutput()
				// This test process is deliberately not a managed speaker service.
				if err == nil || !strings.Contains(string(output), `"route":"recovery"`) || !strings.Contains(string(output), "standalone") {
					t.Fatalf("expected managed-service rejection: %v %s", err, output)
				}
				if calls.Load() != 0 {
					t.Fatalf("recovery contacted offline LLM %d times", calls.Load())
				}
				current, err := os.ReadFile(path)
				if err != nil || string(current) != string(original) {
					t.Fatalf("unmanaged recovery changed configuration: %v %s", err, current)
				}
			})
		}
	}
}
