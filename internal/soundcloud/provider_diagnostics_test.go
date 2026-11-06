package soundcloud

import (
	"bytes"
	"encoding/json/v2"
	"log/slog"
	"strings"
	"testing"
)

func TestProviderCallbackDiagnosticRejectsWithoutSecrets(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "speaker", r)
	req := providerStart(t, p, ctx, r)
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(old)
	bad := eventPayload(req)
	bad["token"] = "private-token"
	p.HandleEvent(ctx, EventPrefix+"stopped", bad)
	var d map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &d); err != nil {
		t.Fatalf("missing diagnostic: %v", err)
	}
	for k, want := range map[string]any{"action": "stopped", "outcome": "token_mismatch", "session_present": true, "session_match": true, "stream_match": true, "token_match": false, "generation_current": true, "terminal": false} {
		if d[k] != want {
			t.Errorf("%s=%v want=%v", k, d[k], want)
		}
	}
	for _, secret := range []string{req.SessionID, req.StreamID, req.CallbackToken, "private-token"} {
		if secret != "" && strings.Contains(output.String(), secret) {
			t.Fatal("diagnostic leaked private field")
		}
	}
}
