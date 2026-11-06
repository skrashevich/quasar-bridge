package main

import (
	"context"
	"encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCheckBackendsReportsSanitizedChatGPTFailure(t *testing.T) {
	if path := os.Getenv("QUASAR_CHECK_FAILURE_CONFIG"); path != "" {
		status, err := strconv.Atoi(os.Getenv("QUASAR_CHECK_FAILURE_STATUS"))
		if err != nil {
			t.Fatal(err)
		}
		http.DefaultClient.Transport = chatGPTTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodPost || r.URL.String() != "https://api.openai.com/v1/responses" {
				t.Fatalf("unexpected inference request: %s %s", r.Method, r.URL)
			}
			body := `{"error":{"message":"private upstream body","code":"subscription_sharing_usage_limit_exceeded"}}`
			if status == http.StatusUnauthorized {
				body = `{"error":{"message":"private upstream body"}}`
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", path, "-check-backends"}
		main()
		return
	}
	for _, tc := range []struct {
		status int
		cause  string
	}{
		{http.StatusUnauthorized, "ChatGPT authentication rejected"},
		{http.StatusTooManyRequests, "ChatGPT usage limit reached"},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			authPath := filepath.Join(t.TempDir(), "account.json")
			credentials := `{"version":1,"ext_agent_host_id":"host-fixture","issuer":"https://auth.openai.com","subject":"user-fixture","client_id":"oaiapp_fixture","access_token":"private access token","refresh_token":"private refresh token","scope":"chatgpt.tokens.use.direct offline_access","expires_at":"2100-01-01T00:00:00Z"}`
			if err := os.WriteFile(authPath, []byte(credentials), 0600); err != nil {
				t.Fatal(err)
			}
			encodedPath, err := json.Marshal(authPath)
			if err != nil {
				t.Fatal(err)
			}
			path := configFile(t, fmt.Sprintf(`{"mode":"hybrid","llm_api":"chatgpt","llm_model":"fixture","llm_chatgpt_auth_file":%s}`, encodedPath))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCheckBackendsReportsSanitizedChatGPTFailure$")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "QUASAR_") {
					child.Env = append(child.Env, entry)
				}
			}
			child.Env = append(child.Env, "QUASAR_HOME_MCP_URL=", "QUASAR_CHECK_FAILURE_CONFIG="+path, "QUASAR_CHECK_FAILURE_STATUS="+strconv.Itoa(tc.status))
			output, err := child.CombinedOutput()
			if err == nil || child.ProcessState == nil || child.ProcessState.ExitCode() != 1 {
				t.Fatalf("check-backends did not fail: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "LLM check failed") || !strings.Contains(string(output), tc.cause) {
				t.Fatalf("missing actionable failure cause:\n%s", output)
			}
			for _, private := range []string{"private upstream body", "private access token", "private refresh token"} {
				if strings.Contains(string(output), private) {
					t.Fatalf("check-backends leaked private data:\n%s", output)
				}
			}
		})
	}
}
