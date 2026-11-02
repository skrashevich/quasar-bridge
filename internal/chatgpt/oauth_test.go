package chatgpt

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func signedToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"fixture-key"}`))
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := header + "." + base64.RawURLEncoding.EncodeToString(body)
	hash := sha256.Sum256([]byte(payload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	return payload + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func keyServer(t *testing.T, key *rsa.PrivateKey) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.MarshalWrite(w, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "fixture-key", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestLoginRegistersAndReauthorizesWithoutPrintingSecrets(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := keyServer(t, key)
	s := fixtureStore(t)
	s.jwksURL = jwks.URL
	var nonce, verifierChallenge, redirect, hostID string
	var exchanges int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		verifier := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("client_id") != "issued-client" || r.Form.Get("code") != "code-secret" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("resource") != resource || r.Form.Get("redirect_uri") != redirect || base64.RawURLEncoding.EncodeToString(verifier[:]) != verifierChallenge {
			t.Error("incorrect authorization exchange")
		}
		claims := map[string]any{"iss": issuer, "aud": "issued-client", "sub": "verified-subject", "email": "user@example.invalid", "exp": time.Now().Add(time.Hour).Unix(), "nonce": nonce}
		json.MarshalWrite(w, map[string]any{"access_token": "new-access-secret", "refresh_token": "new-refresh-secret", "id_token": signedToken(t, key, claims), "token_type": "Bearer", "expires_in": 3600, "scope": planScope + " offline_access"})
	}))
	defer server.Close()
	s.tokenURL = server.URL
	for attempt := range 2 {
		err = s.Login(t.Context(), "127.0.0.1:0", func(raw string) error {
			if strings.Contains(raw, "secret") || strings.Contains(raw, "id_token_hint") {
				t.Error("login URL exposed retained tokens")
			}
			u, err := url.Parse(raw)
			if err != nil {
				return err
			}
			q := u.Query()
			if u.Scheme != "https" || u.Host != "auth.openai.com" || u.Path != "/api/accounts/authorize" || q.Get("response_type") != "code" || q.Get("resource") != resource || !hasPlanScope(q.Get("scope")) || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" {
				t.Error("incorrect authorization URL")
			}
			if attempt == 0 && (q.Get("client_id") != dynamicClient || q.Get("agent_name_hint") != "Quasar Bridge") {
				t.Error("registration did not use actual app name and dynamic client")
			}
			if attempt == 1 && (q.Get("client_id") != "issued-client" || q.Has("agent_name_hint")) {
				t.Error("returning login did not reuse issued registration")
			}
			c, err := s.load()
			if err != nil || c.HostID == "" || c.HostID != q.Get("ext_agent_host_id") || (attempt == 1 && c.HostID != hostID) {
				t.Error("host identity was not persisted before login")
			}
			hostID = c.HostID
			nonce, verifierChallenge, redirect = q.Get("nonce"), q.Get("code_challenge"), q.Get("redirect_uri")
			callback := redirect + "?code=code-secret&state=" + q.Get("state")
			if attempt == 0 {
				callback += "&client_id=issued-client"
			}
			resp, err := http.Get(callback)
			if err != nil {
				return err
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("callback HTTP %d", resp.StatusCode)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("attempt=%d error=%v", attempt, err)
		}
		status, err := s.Status(t.Context())
		if err != nil || !status.SignedIn || status.Subject != "verified-subject" || status.ClientID != "issued-client" {
			t.Fatalf("status=%+v error=%v", status, err)
		}
	}
	if exchanges != 2 {
		t.Fatalf("exchanges=%d", exchanges)
	}
}

func TestLoginRejectsMismatchedStateAndDeniedAuthorization(t *testing.T) {
	s := fixtureStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := s.Login(ctx, "127.0.0.1:0", func(raw string) error {
		u, _ := url.Parse(raw)
		q := u.Query()
		resp, err := http.Get(q.Get("redirect_uri") + "?state=wrong&code=secret")
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Error("mismatched state accepted")
		}
		resp, err = http.Get(q.Get("redirect_uri") + "?state=" + q.Get("state") + "&error=access_denied&error_description=secret")
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "denied") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error=%v", err)
	}
	status, err := s.Status(t.Context())
	if err != nil || status.SignedIn {
		t.Fatal("denied sign-in became active", err)
	}
}

func TestLoginRejectsPublicBindAndCanceledAttempt(t *testing.T) {
	s := fixtureStore(t)
	if err := s.Login(t.Context(), "0.0.0.0:1455", func(string) error { t.Error("URL shown for public listener"); return nil }); err == nil {
		t.Fatal("public callback listener accepted")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if err := s.Login(ctx, "127.0.0.1:0", func(string) error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error=%v", err)
	}
}

func TestLoginRetainsIssuedRegistrationAfterFailedExchange(t *testing.T) {
	s := fixtureStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"code-secret"}`))
	}))
	defer server.Close()
	s.tokenURL = server.URL
	var previousHost string
	for attempt := range 2 {
		err := s.Login(t.Context(), "127.0.0.1:0", func(raw string) error {
			u, _ := url.Parse(raw)
			q := u.Query()
			if attempt == 0 {
				if q.Get("client_id") != dynamicClient {
					t.Error("first registration did not start dynamically")
				}
				previousHost = q.Get("ext_agent_host_id")
			} else if q.Get("client_id") != "issued-client" || q.Get("ext_agent_host_id") != previousHost || q.Has("agent_name_hint") {
				t.Error("failed registration was not reused")
			}
			resp, err := http.Get(q.Get("redirect_uri") + "?state=" + q.Get("state") + "&client_id=issued-client&code=code-secret")
			if err == nil {
				resp.Body.Close()
			}
			return err
		})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("exchange failure error=%v", err)
		}
		c, err := s.load()
		if err != nil || c.ClientID != "issued-client" || c.Subject != "" || c.AccessToken != "" || c.signedIn() {
			t.Fatal("issued registration was discarded or unverified tokens became active", err)
		}
	}
}

func TestLoginRejectsAccountSubstitutionAndMissingPermission(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := keyServer(t, key)
	for _, tc := range []struct {
		name              string
		newAccount        bool
		wrongSubject      bool
		wrongNonce        bool
		wrongClient       bool
		missingPermission bool
	}{
		{name: "account substitution", wrongSubject: true},
		{name: "nonce substitution", wrongNonce: true},
		{name: "client substitution", wrongClient: true},
		{name: "returning missing permission", missingPermission: true},
		{name: "new missing permission", newAccount: true, missingPermission: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixtureStore(t)
			s.jwksURL = jwks.URL
			if !tc.newAccount {
				saveFixture(t, s, activeCredentials())
			}
			var nonce string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.wrongClient {
					t.Error("client substitution reached token endpoint")
				}
				claims := map[string]any{"iss": issuer, "aud": "client-fixture", "sub": "subject-fixture", "exp": time.Now().Add(time.Hour).Unix(), "nonce": nonce}
				if tc.wrongSubject {
					claims["sub"] = "other-subject"
				}
				if tc.wrongNonce {
					claims["nonce"] = "wrong-nonce"
				}
				scope := planScope
				if tc.missingPermission {
					scope = "openid"
				}
				json.MarshalWrite(w, map[string]any{"access_token": "other-access-secret", "refresh_token": "other-refresh-secret", "id_token": signedToken(t, key, claims), "token_type": "Bearer", "expires_in": 3600, "scope": scope})
			}))
			defer server.Close()
			s.tokenURL = server.URL
			err := s.Login(t.Context(), "127.0.0.1:0", func(raw string) error {
				u, _ := url.Parse(raw)
				q := u.Query()
				nonce = q.Get("nonce")
				clientID := "client-fixture"
				if tc.wrongClient {
					clientID = "wrong-client"
				}
				resp, err := http.Get(q.Get("redirect_uri") + "?state=" + q.Get("state") + "&client_id=" + clientID + "&code=code-secret")
				if err == nil {
					resp.Body.Close()
				}
				return err
			})
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe sign-in was accepted: %v", err)
			}
			c, loadErr := s.load()
			if loadErr != nil || c.ClientID != "client-fixture" || (tc.newAccount && (c.AccessToken != "" || c.Subject != "")) || (!tc.newAccount && (c.AccessToken != "access-secret" || c.Subject != "subject-fixture")) {
				t.Fatal("failed sign-in replaced active account or discarded registration", loadErr)
			}
		})
	}
}

func TestValidateIDTokenRejectsInvalidClaimsAndSignatures(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := keyServer(t, key)
	s := &Store{jwksURL: server.URL}
	for _, test := range []struct {
		name   string
		modify func(map[string]any)
		key    *rsa.PrivateKey
	}{
		{"issuer", func(c map[string]any) { c["iss"] = "https://example.invalid" }, key},
		{"audience", func(c map[string]any) { c["aud"] = "other-client" }, key},
		{"expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Second).Unix() }, key},
		{"nonce", func(c map[string]any) { c["nonce"] = "other-nonce" }, key},
		{"subject", func(c map[string]any) { delete(c, "sub") }, key},
		{"authorized party", func(c map[string]any) { c["azp"] = "other-client" }, key},
		{"not yet valid", func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() }, key},
		{"invalid signature", func(map[string]any) {}, other},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := map[string]any{"iss": issuer, "aud": "client", "sub": "subject", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "nonce"}
			test.modify(claims)
			if _, err := s.validateIDToken(t.Context(), signedToken(t, test.key, claims), "client", "nonce"); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("bad ID token accepted: %v", err)
			}
		})
	}
	claims := map[string]any{"iss": issuer, "aud": []string{"client"}, "sub": "subject", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "nonce"}
	if _, err := s.validateIDToken(t.Context(), signedToken(t, key, claims), "client", "nonce"); err != nil {
		t.Fatal("valid array audience rejected", err)
	}
}

func TestLogoutClearsLocalTokensAndRetainsRegistration(t *testing.T) {
	for _, statusCode := range []int{200, 503} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			s := fixtureStore(t)
			saveFixture(t, s, activeCredentials())
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				r.ParseForm()
				if r.Method != "POST" || r.Form.Get("client_id") != "client-fixture" || r.Form.Get("token") != "refresh-secret" || r.Form.Get("token_type_hint") != "refresh_token" {
					t.Error("bad revocation request")
				}
				w.WriteHeader(statusCode)
			}))
			defer server.Close()
			s.revokeURL = server.URL
			err := s.Logout(t.Context())
			if (err != nil) != (statusCode != 200) || (err != nil && !strings.Contains(err.Error(), "not confirmed")) || calls.Load() != 1 {
				t.Fatalf("logout error=%v calls=%d", err, calls.Load())
			}
			c, err := s.load()
			if err != nil || c.AccessToken != "" || c.RefreshToken != "" || c.IDToken != "" || c.ClientID != "client-fixture" || c.Subject != "subject-fixture" || c.HostID == "" {
				t.Fatal("wrong local sign-out state", err)
			}
			raw, _ := os.ReadFile(s.Path)
			if strings.Contains(string(raw), "secret") {
				t.Fatal("signed-out store retained tokens")
			}
		})
	}
}
