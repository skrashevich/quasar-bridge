package localfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadRegularFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "regular")
	link := filepath.Join(dir, "link")
	fifo := filepath.Join(dir, "fifo")
	if err := os.WriteFile(path, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		links      Symlinks
		limit      int64
		ok         bool
	}{
		{"exact size", path, NoFollow, 3, true},
		{"over limit", path, NoFollow, 2, false},
		{"firmware symlink", link, Follow, 3, true},
		{"state symlink", link, NoFollow, 3, false},
		{"fifo without a writer", fifo, NoFollow, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _, err := Read(tc.path, tc.links, tc.limit)
			if (err == nil) != tc.ok || tc.ok && string(data) != "abc" {
				t.Fatalf("Read = %q, %v", data, err)
			}
		})
	}
}
