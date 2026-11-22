package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quasarbridge/internal/chatgpt"
)

func TestPanelChatGPTLogin(t *testing.T) {
	panel := &panelChatGPT{store: chatgpt.Store{Path: filepath.Join(t.TempDir(), "account.json")}}
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{ChatGPT: panel})
	start := func() url.Values {
		t.Helper()
		res := diagnosticAPIRequest(mux, "POST", "chatgpt/start", "{}", "application/json")
		var result struct {
			URL       string `json:"authorization_url"`
			Automatic bool   `json:"automatic_return"`
		}
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &result) != nil || !result.Automatic {
			t.Fatal("could not start local login")
		}
		u, err := url.Parse(result.URL)
		if err != nil || u.Host != "auth.openai.com" || u.Query().Get("redirect_uri") != "http://127.0.0.1:18443/auth/callback" {
			t.Fatal("wrong authorization destination")
		}
		return u.Query()
	}
	q := start()
	complete := func(link string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"callback": link})
		return diagnosticAPIRequest(mux, "POST", "chatgpt/complete", string(body), "application/json")
	}
	link := q.Get("redirect_uri") + "?code=code-fixture&client_id=registered&state=" + q.Get("state")
	if complete(strings.Replace(link, "127.0.0.1", "example.invalid", 1)).Code != 400 || complete(link+"wrong").Code != 400 {
		t.Fatal("unrelated callback accepted")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	calls := 0
	panel.store.HTTP = &http.Client{Transport: chatGPTTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "auth.openai.com" {
			t.Fatalf("unexpected issuer host")
		}
		var body any
		switch r.URL.Path {
		case "/api/accounts/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if enc(digest[:]) != q.Get("code_challenge") || r.Form.Get("code") != "code-fixture" || r.Form.Get("redirect_uri") != q.Get("redirect_uri") {
				t.Fatal("exchange did not match the original login")
			}
			claims, _ := json.Marshal(map[string]any{"iss": "https://auth.openai.com", "sub": "demo", "aud": "registered", "email": "demo@example.invalid", "exp": time.Now().Add(time.Hour).Unix(), "nonce": q.Get("nonce")})
			jwt := enc([]byte(`{"alg":"RS256","kid":"demo"}`)) + "." + enc(claims)
			hash := sha256.Sum256([]byte(jwt))
			sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
			if err != nil {
				t.Fatal(err)
			}
			body = map[string]any{"access_token": "access-fixture", "refresh_token": "refresh-fixture", "token_type": "Bearer", "expires_in": 3600, "scope": "chatgpt.tokens.use.direct", "id_token": jwt + "." + enc(sig)}
		case "/.well-known/jwks.json":
			body = map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "demo", "n": enc(key.N.Bytes()), "e": "AQAB"}}}
		case "/api/accounts/oauth/revoke":
			if r.FormValue("token") != "refresh-fixture" {
				t.Fatal("wrong token revoked")
			}
		default:
			t.Fatalf("unexpected issuer endpoint")
		}
		raw, _ := json.Marshal(body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})}
	res := complete(link)
	if res.Code != 200 || strings.Contains(res.Body.String(), "fixture") || calls != 2 {
		t.Fatalf("exchange failed: %d %s", res.Code, res.Body)
	}
	if complete(link).Code != 400 || calls != 2 {
		t.Fatal("used callback accepted twice")
	}
	status := diagnosticAPIRequest(mux, "GET", "chatgpt", "", "").Body.String()
	if !strings.Contains(status, "demo@example.invalid") || !strings.Contains(status, `"signed_in":true`) || strings.Contains(status, "fixture") {
		t.Fatal("incorrect account status")
	}
	if token, err := panel.store.Token(t.Context()); err != nil || token != "access-fixture" {
		t.Fatal("model credential unavailable after panel login")
	}
	q = start()
	if diagnosticAPIRequest(mux, "POST", "chatgpt/logout", "{}", "application/json").Code != 200 || calls != 3 {
		t.Fatal("logout failed")
	}
	if complete(q.Get("redirect_uri")+"?state="+q.Get("state")+"&code=old").Code != 400 {
		t.Fatal("logout left a pending login")
	}
	if status = diagnosticAPIRequest(mux, "GET", "chatgpt", "", "").Body.String(); strings.Contains(status, "demo@example.invalid") || !strings.Contains(status, `"signed_in":false`) {
		t.Fatal("incorrect signed-out status")
	}
	q = start()
	panel.pending[0].expires = time.Now().Add(-time.Second)
	if complete(q.Get("redirect_uri")+"?state="+q.Get("state")+"&code=expired").Code != 400 || calls != 3 {
		t.Fatal("expired login accepted")
	}
}

func TestPanelLoginRemoteReturn(t *testing.T) {
	req := httptest.NewRequest("POST", "http://192.0.2.1:2704/diagnostics/api/chatgpt/start", nil)
	if port, automatic := panelCallbackPort(req); port != 1455 || automatic {
		t.Fatal("remote browser cannot return to the speaker's loopback")
	}
}
