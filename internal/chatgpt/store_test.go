package chatgpt

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Path: filepath.Join(t.TempDir(), "private", "chatgpt.json")}
}

func saveFixture(t *testing.T, s *Store, c credentials) {
	t.Helper()
	if err := s.save(c); err != nil {
		t.Fatal(err)
	}
}

func activeCredentials() credentials {
	return credentials{Version: 1, HostID: "host-fixture", Issuer: issuer, Subject: "subject-fixture", ClientID: "client-fixture", Email: "user@example.invalid", AccessToken: "access-secret", RefreshToken: "refresh-secret", IDToken: "id-secret", Scope: planScope, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestTokenAndStatusDoNotExposeCredentials(t *testing.T) {
	s := fixtureStore(t)
	c := activeCredentials()
	saveFixture(t, s, c)
	token, err := s.Token(t.Context())
	if err != nil || token != c.AccessToken {
		t.Fatalf("unexpected token result: %v", err)
	}
	status, err := s.Status(t.Context())
	if err != nil || !status.SignedIn || status.Email != c.Email || status.Subject != c.Subject || status.ClientID != c.ClientID {
		t.Fatalf("status=%+v error=%v", status, err)
	}
	data, err := json.Marshal(status)
	if err != nil || strings.Contains(string(data), "secret") {
		t.Fatal("status exposed credentials")
	}
	info, err := os.Stat(s.Path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credentials are not owner-only", err)
	}
}

func TestTokenRefreshesOnceAcrossStoresAndRotatesAtomically(t *testing.T) {
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Method != "POST" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "client-fixture" || r.Form.Get("refresh_token") != "refresh-secret" || r.Form.Get("resource") != resource || r.Form.Has("scope") {
			t.Error("incorrect refresh form")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"new-access-secret","refresh_token":"new-refresh-secret","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()
	s := fixtureStore(t)
	s.tokenURL = server.URL
	c := activeCredentials()
	c.ExpiresAt = time.Now().Add(-time.Minute)
	saveFixture(t, s, c)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			other := &Store{Path: s.Path, tokenURL: server.URL}
			got, err := other.Token(t.Context())
			if err != nil || got != "new-access-secret" {
				t.Errorf("refresh result error=%v", err)
			}
		})
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("refreshes=%d", count.Load())
	}
	next, err := s.load()
	if err != nil || next.RefreshToken != "new-refresh-secret" || next.Scope != planScope || next.ClientID != c.ClientID || next.Subject != c.Subject {
		t.Fatal("rotated credentials were not preserved", err)
	}
}

func TestRefreshFailuresPreserveOrClearTokensWithoutLeaking(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		clear  bool
	}{
		{"network service", 503, `{"error":"server_error","error_description":"refresh-secret"}`, false},
		{"revoked", 400, `{"error":"invalid_grant","error_description":"refresh-secret"}`, true},
		{"invalid client", 400, `{"error":"invalid_client"}`, false},
		{"missing replacement", 200, `{"access_token":"access-secret","token_type":"Bearer","expires_in":3600}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixtureStore(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			s.tokenURL = server.URL
			c := activeCredentials()
			c.ExpiresAt = time.Now().Add(-time.Minute)
			saveFixture(t, s, c)
			got, err := s.Token(t.Context())
			if err == nil || got != "" || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), s.Path) {
				t.Fatalf("unsafe error=%v", err)
			}
			next, err := s.load()
			if err != nil || (next.RefreshToken == "") != tc.clear || next.ClientID != c.ClientID || next.Subject != c.Subject || next.HostID != c.HostID {
				t.Fatal("incorrect failure persistence", err)
			}
		})
	}
}

func TestRefreshPersistsRotatedTokensWhenPlanPermissionIsRemoved(t *testing.T) {
	s := fixtureStore(t)
	c := activeCredentials()
	c.ExpiresAt = time.Now().Add(-time.Minute)
	saveFixture(t, s, c)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"replacement-access","refresh_token":"replacement-refresh","token_type":"Bearer","expires_in":3600,"scope":"openid"}`))
	}))
	defer server.Close()
	s.tokenURL = server.URL
	if token, err := s.Token(t.Context()); err == nil || token != "" || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("plan use was allowed after permission removal: %v", err)
	}
	next, err := s.load()
	if err != nil || next.RefreshToken != "replacement-refresh" || next.Scope != "openid" || next.signedIn() {
		t.Fatal("revoked permission retained stale rotating token", err)
	}
}

func TestCredentialFailuresAndCanceledLock(t *testing.T) {
	s := fixtureStore(t)
	if _, err := s.Token(t.Context()); err == nil {
		t.Fatal("missing login accepted")
	}
	status, err := s.Status(t.Context())
	if err != nil || status.SignedIn {
		t.Fatal("missing credential status", err)
	}
	saveFixture(t, s, activeCredentials())
	if err := os.Chmod(s.Path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(t.Context()); err == nil {
		t.Fatal("insecure credentials accepted")
	}
	if err := os.Chmod(s.Path, 0600); err != nil {
		t.Fatal(err)
	}
	unlock, err := s.lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := (&Store{Path: s.Path}).Token(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock cancellation error=%v", err)
	}
}

func TestLegacyCodexCredentialsAreRejected(t *testing.T) {
	s := fixtureStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, []byte(`{"tokens":{"access_token":"access-secret","refresh_token":"refresh-secret"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(t.Context()); err == nil || !strings.Contains(err.Error(), "ChatGPT") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error=%v", err)
	}
}

func TestRefreshDoesNotFollowTokenBearingRedirect(t *testing.T) {
	s := fixtureStore(t)
	c := activeCredentials()
	c.ExpiresAt = time.Now().Add(-time.Minute)
	saveFixture(t, s, c)
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	s.tokenURL = server.URL
	if _, err := s.Token(t.Context()); err == nil || strings.Contains(err.Error(), "secret") || redirected.Load() != 0 {
		t.Fatalf("token-bearing redirect followed: requests=%d error=%v", redirected.Load(), err)
	}
	next, err := s.load()
	if err != nil || next.RefreshToken != c.RefreshToken {
		t.Fatal("redirect modified credentials", err)
	}
}

func TestCredentialAndLockSymlinksAreRejected(t *testing.T) {
	for _, suffix := range []string{"", ".lock"} {
		t.Run("symlink"+suffix, func(t *testing.T) {
			s := fixtureStore(t)
			if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("do-not-touch"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, s.Path+suffix); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Token(t.Context()); err == nil {
				t.Fatal("symlink accepted")
			}
			data, err := os.ReadFile(target)
			info, statErr := os.Stat(target)
			if err != nil || statErr != nil || string(data) != "do-not-touch" || info.Mode().Perm() != 0644 {
				t.Fatal("symlink target modified", err, statErr)
			}
		})
	}
}
