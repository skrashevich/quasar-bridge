//go:build linux || darwin

package mcpoauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOAuthRefreshSerializedAndRotated(t *testing.T) {
	for _, rotate := range []bool{true, false} {
		name, wantRefresh := "preserves old refresh token", "old-refresh"
		if rotate {
			name, wantRefresh = "stores rotated refresh token", "new-refresh"
		}
		t.Run(name, func(t *testing.T) {
			b := testBinding()
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/token" || r.FormValue("grant_type") != "refresh_token" ||
					r.FormValue("client_id") != b.ClientID || r.FormValue("resource") != b.Endpoint ||
					r.FormValue("refresh_token") != "old-refresh" || r.FormValue("client_secret") != "" ||
					r.Header.Get("Authorization") != "" {
					t.Error("invalid or overprivileged refresh request")
					w.WriteHeader(400)
					return
				}
				reply := map[string]any{"access_token": "new-access", "token_type": "Bearer", "expires_in": 300}
				if rotate {
					reply["refresh_token"] = wantRefresh
				}
				writeOAuthJSON(w, reply)
			}))
			defer server.Close()
			b.Issuer = server.URL
			config := filepath.Join(t.TempDir(), "config.json")
			s := testStore(t, config)
			s2, err := NewStore(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Save(t.Context(), Record{Binding: b, AccessToken: "old-access", RefreshToken: "old-refresh",
				TokenEndpoint: server.URL + "/token", Expiry: time.Now().Add(-time.Hour)}); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			for _, store := range []*Store{s, s2} {
				wg.Go(func() {
					<-start
					source := Source{Store: store, HTTP: server.Client().Transport}
					if got, err := source.AccessToken(t.Context(), b); err != nil || got != "new-access" {
						t.Errorf("concurrent refresh did not return the new token: %v", err)
					}
				})
			}
			close(start)
			wg.Wait()
			source := Source{Store: s2, HTTP: server.Client().Transport}
			if got, err := source.AccessToken(t.Context(), b); err != nil || got != "new-access" || calls.Load() != 1 {
				t.Fatalf("refresh was not shared: issuer calls=%d, error=%v", calls.Load(), err)
			}
			if r, err := s2.Load(b); err != nil || r.RefreshToken != wantRefresh {
				t.Fatalf("wrong refresh token persisted: %v", err)
			}
		})
	}
}

func TestOAuthRefreshInvalidGrantAndTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want error
	}{
		{"temporary", http.StatusServiceUnavailable, ErrRefreshUnavailable},
		{"invalid grant", http.StatusBadRequest, ErrSignInRequired},
		{"overbroad grant", http.StatusOK, ErrRefreshUnavailable},
		{"untrusted endpoint", 0, ErrSignInRequired},
		{"changed binding", -1, ErrSignInRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBinding()
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				if tc.code == http.StatusOK {
					writeOAuthJSON(w, map[string]any{
						"access_token": "write-access", "token_type": "Bearer", "expires_in": 120, "scope": "read write",
					})
					return
				}
				w.WriteHeader(tc.code)
				writeOAuthJSON(w, map[string]string{"error": "invalid_grant", "error_description": "private-server-detail"})
			}))
			defer server.Close()
			b.Issuer = server.URL
			store := testStore(t, filepath.Join(t.TempDir(), "config.json"))
			endpoint := server.URL + "/token"
			if tc.code == 0 {
				endpoint = "https://untrusted.example.invalid/token"
			}
			old := Record{Binding: b, AccessToken: "old-access", RefreshToken: "old-refresh",
				Expiry: time.Now().Add(-time.Hour), TokenEndpoint: endpoint}
			if err := store.Save(t.Context(), old); err != nil {
				t.Fatal(err)
			}
			source := Source{Store: store, HTTP: server.Client().Transport}
			request := b
			if tc.code == -1 {
				request.ClientID += "/other"
			}
			if got, err := source.AccessToken(t.Context(), request); got != "" || !errors.Is(err, tc.want) {
				t.Fatalf("unexpected refresh result: %v", err)
			}
			r, err := store.Load(b)
			if err != nil {
				t.Fatal(err)
			}
			if tc.code == http.StatusBadRequest {
				if !r.NeedsLogin || r.AccessToken != "" || r.RefreshToken != "" {
					t.Fatal("invalid_grant did not clear the saved access")
				}
				if _, err := source.AccessToken(t.Context(), b); !errors.Is(err, ErrSignInRequired) || requests.Load() != 1 {
					t.Fatalf("invalid grant retried: %v", err)
				}
			} else if r.NeedsLogin || r.AccessToken != old.AccessToken || r.RefreshToken != old.RefreshToken {
				t.Fatal("an unsuccessful refresh changed credentials")
			}
			if tc.code <= 0 && requests.Load() != 0 {
				t.Fatal("request made for invalid binding or endpoint")
			}
		})
	}
}
