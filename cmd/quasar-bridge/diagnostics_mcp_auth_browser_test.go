//go:build linux || darwin

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestPanelMCPOAuthBrowser(t *testing.T) {
	if os.Getenv("QUASAR_TEST_BROWSER") != "1" {
		t.Skip("set QUASAR_TEST_BROWSER=1 after installing Playwright and Chromium")
	}
	var f *panelOAuthFixture
	f = newPanelOAuthFixture(t, func(r *http.Request) {
		if r.URL.Path == "/token" && f.transport.tokens.Load() == 3 {
			<-r.Context().Done()
		}
	}, nil)
	f.mux.HandleFunc("GET /fixture-token-requests", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strconv.Itoa(int(f.transport.tokens.Load()))))
	})
	panel := httptest.NewServer(f.mux)
	defer panel.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "../../scripts/web-screenshots/mcp-account.mjs", panel.URL, f.issuer.URL)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("manual MCP browser flow: %v\n%s", err, out)
	}
	if f.transport.tokens.Load() != 3 || f.calls.Load() != 0 || f.lists.Load() != 0 {
		t.Fatalf("expected three exchange requests and no tools, got %d exchanges, %d calls, %d lists",
			f.transport.tokens.Load(), f.calls.Load(), f.lists.Load())
	}
}
