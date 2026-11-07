package bridge

import (
	"os"
	"sync"
	"time"
)

// wakewordRouteMaxAge bounds how long a marker stays valid after it is written.
const wakewordRouteMaxAge = 3 * time.Second

var wakewordRouteMu sync.Mutex

// claimWakewordRoute reports whether a fresh marker file asks for the next
// voice request to be answered by the LLM. The marker is written by a local
// wake-word daemon (for example station-wakeword's "llm" action) just before it
// starts a conversation. It is consumed at most once, so an abandoned or stale
// activation never reroutes a later ordinary Alice request.
func claimWakewordRoute(path string, now time.Time) bool {
	if path == "" {
		return false
	}
	wakewordRouteMu.Lock()
	defer wakewordRouteMu.Unlock()
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	defer os.Remove(path)
	age := now.Sub(info.ModTime())
	if !info.Mode().IsRegular() || age < 0 || age > wakewordRouteMaxAge {
		return false
	}
	b, err := os.ReadFile(path)
	return err == nil && string(b) == "llm\n"
}
