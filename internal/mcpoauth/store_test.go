//go:build linux || darwin

package mcpoauth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func testBinding() Binding {
	return Binding{
		Name: "catalog", Endpoint: "https://mcp.example.invalid/mcp",
		Issuer: "https://auth.example.invalid", ClientID: "https://client.example.invalid/client.json",
		Scopes: []string{"read", "offline_access"},
	}
}

func testStore(t *testing.T, config string) *Store {
	t.Helper()
	if err := os.WriteFile(config, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(config)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type cancelAtCredentialCommit struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelAtCredentialCommit) Err() error {
	c.checks++
	if c.checks == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestStoreSaveChecksCancellationAtCommit(t *testing.T) {
	store := testStore(t, filepath.Join(t.TempDir(), "config.json"))
	b := testBinding()
	if err := store.Save(t.Context(), Record{Binding: b, AccessToken: "previous"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	atCommit := &cancelAtCredentialCommit{Context: ctx, cancel: cancel}
	if err := store.Save(atCommit, Record{Binding: b, AccessToken: "replacement"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("commit did not observe cancellation: %v", err)
	}
	if record, err := store.Load(b); err != nil || record.AccessToken != "previous" {
		t.Fatalf("canceled commit did not preserve the prior record: %v", err)
	}
}

func TestStorePrivateAndIsolated(t *testing.T) {
	dir := t.TempDir()
	b := testBinding()
	a := testStore(t, filepath.Join(dir, "a.json"))
	s := testStore(t, filepath.Join(dir, "b.json"))
	for i, store := range []*Store{a, s} {
		if err := store.Save(t.Context(), Record{Binding: b, AccessToken: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	for i, store := range []*Store{a, s} {
		got, err := store.Load(b)
		if err != nil || got.AccessToken != fmt.Sprint(i) {
			t.Fatalf("store %d resolved to the wrong credential: %v", i, err)
		}
	}
	private := filepath.Join(dir, ".mcp-oauth")
	checked := 0
	if err := filepath.WalkDir(private, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0600)
		if info.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %o, want %o", path, info.Mode().Perm(), want)
		}
		checked++
		return nil
	}); err != nil || checked != 7 {
		t.Fatalf("inspected %d credential paths: %v", checked, err)
	}
	for _, edit := range []func(*Binding){
		func(b *Binding) { b.Endpoint += "/new" },
		func(b *Binding) { b.Issuer += "/new" },
		func(b *Binding) { b.ClientID += "/new" },
		func(b *Binding) { b.Scopes = []string{"write"} },
	} {
		changed := testBinding()
		edit(&changed)
		if _, err := a.Load(changed); !errors.Is(err, ErrSignInRequired) {
			t.Fatalf("changed binding did not require sign-in: %v", err)
		}
	}
	b.Scopes = []string{"offline_access", "read"}
	if _, err := a.Load(b); err != nil {
		t.Fatalf("scope order changed the binding: %v", err)
	}
	b.Name = "other"
	if _, err := a.Load(b); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("other server can incorrectly load the credential: %v", err)
	}
}

func TestStoreRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	s := testStore(t, filepath.Join(dir, "config.json"))
	b := testBinding()
	if err := s.Save(t.Context(), Record{Binding: b, AccessToken: "fixture"}); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, ".mcp-oauth", "*", "catalog.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected one credential: %v %v", files, err)
	}
	path := files[0]
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(b); err == nil {
		t.Fatal("accepted group-readable credentials")
	}
	target := filepath.Join(dir, "target.json")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(b); err == nil {
		t.Fatal("followed a credential symlink")
	}
}

func TestStoreFailedUpdateKeepsRecord(t *testing.T) {
	s := testStore(t, filepath.Join(t.TempDir(), "config.json"))
	b := testBinding()
	if err := s.Save(t.Context(), Record{Binding: b, AccessToken: "before"}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("transient failure")
	if _, err := s.Update(t.Context(), b, func(r Record) (Record, error) {
		r.AccessToken = "do not write"
		return r, want
	}); !errors.Is(err, want) {
		t.Fatalf("Update error = %v", err)
	}
	if r, err := s.Load(b); err != nil || r.AccessToken != "before" {
		t.Fatalf("failed update changed the credential: %v", err)
	}
	if err := s.Delete(t.Context(), b.Name); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), b.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(b); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("Delete did not remove credential: %v", err)
	}
}

func TestStoreLockAcrossProcesses(t *testing.T) {
	b := testBinding()
	if config := os.Getenv("QUASAR_TEST_OAUTH_LOCK_CONFIG"); config != "" {
		s, err := NewStore(config)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Update(t.Context(), b, func(r Record) (Record, error) {
			fmt.Fprintln(os.Stdout, "locked")
			_, err := io.Copy(io.Discard, os.Stdin)
			return r, err
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	config := filepath.Join(t.TempDir(), "config.json")
	s := testStore(t, config)
	if err := s.Save(t.Context(), Record{Binding: b, AccessToken: "before"}); err != nil {
		t.Fatal(err)
	}
	procCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	cmd := exec.CommandContext(procCtx, os.Args[0], "-test.run=^TestStoreLockAcrossProcesses$")
	cmd.Env = append(os.Environ(), "QUASAR_TEST_OAUTH_LOCK_CONFIG="+config)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop()
		_ = cmd.Wait()
	}()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("child did not acquire lock: %q, %v", line, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Millisecond)
	defer cancel()
	if err := s.Save(ctx, Record{Binding: b, AccessToken: "do not write"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second writer did not respect lock deadline: %v", err)
	}
	if r, err := s.Load(b); err != nil || r.AccessToken != "before" {
		t.Fatalf("blocked writer changed the credential: %v", err)
	}
}
