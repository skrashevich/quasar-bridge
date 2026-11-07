package bridge

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWakewordRouteMarkerOneShotAndExpiry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "marker")
	if claimWakewordRoute(p, time.Now()) {
		t.Fatal("missing marker claimed")
	}
	if err := os.WriteFile(p, []byte("llm\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !claimWakewordRoute(p, time.Now()) {
		t.Fatal("valid marker missed")
	}
	if claimWakewordRoute(p, time.Now()) {
		t.Fatal("marker claimed twice")
	}
	if err := os.WriteFile(p, []byte("llm\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-4 * time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if claimWakewordRoute(p, time.Now()) {
		t.Fatal("stale marker claimed")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("stale marker left behind")
	}
}

func TestWakewordRouteDisabledWithoutPath(t *testing.T) {
	if claimWakewordRoute("", time.Now()) {
		t.Fatal("empty path must disable the marker")
	}
}
