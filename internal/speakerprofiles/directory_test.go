package speakerprofiles

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDirectoryAutomaticallyPersistsPeopleAndConflicts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "names.json")
	d, err := OpenDirectory(p)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Observe(id("a"), "Анна") || !d.Observe(id("b"), "Борис") {
		t.Fatal("names not learned")
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("cache permissions")
	}
	next, err := OpenDirectory(p)
	if err != nil || next.Lookup(id("a")) != "Анна" || next.Lookup(id("b")) != "Борис" {
		t.Fatal("names not persisted")
	}
	if next.Lookup(id("other")) != "" {
		t.Fatal("owner fallback")
	}
	if !next.Observe(id("a"), "Вера") || next.Lookup(id("a")) != "" {
		t.Fatal("conflict picked name")
	}
	if err := next.Flush(); err != nil {
		t.Fatal(err)
	}
	final, err := OpenDirectory(p)
	if err != nil || final.Lookup(id("a")) != "" {
		t.Fatal("conflict lost on restart")
	}
}
func TestDirectoryRejectsMalformedAndOversizedValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "names.json")
	d, _ := OpenDirectory(p)
	for _, v := range []struct{ id, name string }{{"raw-person", "Анна"}, {strings.ToUpper(id("a")), "Анна"}, {id("a"), "\n"}, {id("a"), strings.Repeat("Я", 81)}} {
		if d.Observe(v.id, v.name) {
			t.Fatal("invalid entry accepted")
		}
	}
	for i := range 64 {
		d.Observe(id(string(rune(i))), "Анна")
	}
	if d.Observe(id("overflow"), "Борис") {
		t.Fatal("unbounded catalogue")
	}
	if err := os.WriteFile(p, []byte(`{"version":99,"entries":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	bad, err := OpenDirectory(p)
	if err == nil || bad == nil || bad.Lookup(id("a")) != "" {
		t.Fatal("unsupported cache version accepted")
	}
}

func TestDirectoryConflictPresenceBlocksFallback(t *testing.T) {
	d, _ := OpenDirectory(filepath.Join(t.TempDir(), "names.json"))
	if name, exists := d.Name(id("a")); name != "" || exists {
		t.Fatal("missing entry is present")
	}
	d.Observe(id("a"), "Анна")
	d.Observe(id("a"), "Вера")
	if name, exists := d.Name(id("a")); name != "" || !exists {
		t.Fatal("conflict indistinguishable from absence")
	}
}

func TestDirectoryConcurrentObserveAndFlush(t *testing.T) {
	p := filepath.Join(t.TempDir(), "names.json")
	d, _ := OpenDirectory(p)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			key := id(string(rune(i)))
			d.Observe(key, "Анна")
			d.Name(key)
			if err := d.Flush(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	next, err := OpenDirectory(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 32 {
		if next.Lookup(id(string(rune(i)))) != "Анна" {
			t.Fatal("concurrent observation lost")
		}
	}
}

func id(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte("quasar-bridge/voice-biometry/v1\x00"+value)))
}
