package commandprompt

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPlayRequiresThreeDigitNumber(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "quasar.cfg")
	for _, n := range []int{-1, 0, 99, 100, 999, 1000} {
		err := Play(t.Context(), missing, n)
		// Only valid numbers reach the station client and its configuration lookup.
		if err == nil || errors.Is(err, os.ErrNotExist) != (100 <= n && n <= 999) {
			t.Errorf("Play(%d) = %v", n, err)
		}
	}
}
