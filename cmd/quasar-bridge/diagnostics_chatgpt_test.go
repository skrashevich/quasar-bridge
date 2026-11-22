package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quasarbridge/internal/chatgpt"
	"quasarbridge/internal/llm"
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

func TestPanelChatGPTSelectsSubscriptionModel(t *testing.T) {
	c := configurationFixture(t, `{"llm_api":"chatgpt","llm_model":"old"}`)
	store := chatgpt.Store{Path: filepath.Join(t.TempDir(), "account.json")}
	if err := os.WriteFile(store.Path, []byte(subscriptionFixture), 0600); err != nil {
		t.Fatal(err)
	}
	store.HTTP = &http.Client{Transport: chatGPTTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.openai.com" || r.Header.Get("Authorization") != "Bearer subscription-fixture" {
			t.Fatal("wrong subscription request")
		}
		body := `{"models":[{"slug":"new","display_name":"New Model","visibility":"list"},{"slug":"hidden","visibility":"hide"}]}`
		if r.URL.Path == "/v1/responses" {
			var request struct {
				Model string `json:"model"`
			}
			if err := json.UnmarshalRead(r.Body, &request); err != nil || request.Model != "new" {
				t.Fatalf("inference model = %q, err = %v", request.Model, err)
			}
			body = "data: " + `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}` + "\n\n"
		} else if r.URL.Path != "/v1/models" || r.Method != "GET" {
			t.Fatal("wrong catalog endpoint")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	client := &llm.Client{API: "chatgpt", Model: "old", SubscriptionModel: c.activeModel, AccessToken: store.Token, HTTP: store.HTTP}
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Configuration: c, ChatGPT: &panelChatGPT{store: store}})
	catalog := diagnosticAPIRequest(mux, "POST", "chatgpt/models", "{}", "application/json")
	if catalog.Code != 200 || !strings.Contains(catalog.Body.String(), `"name":"New Model"`) || strings.Contains(catalog.Body.String(), "hidden") {
		t.Fatal(catalog.Body.String())
	}
	selectModel := func(body string, code int) {
		t.Helper()
		res := diagnosticAPIRequest(mux, "POST", "chatgpt/model", body, "application/json")
		if res.Code != code {
			t.Fatalf("got %d %s, want %d", res.Code, res.Body, code)
		}
	}
	selectModel(`{"expected_model":"old","model":"hidden"}`, 422)
	selectModel(`{"expected_model":"stale","model":"new"}`, 409)
	if c.activeModel() != "old" {
		t.Fatal("invalid choice changed active model")
	}
	selectModel(`{"expected_model":"old","model":"new"}`, 200)
	if cfg, err := readConfig(c.path); err != nil || cfg.LLMModel != "new" {
		t.Fatal("choice was not persisted")
	}
	if answer, err := client.Complete(t.Context(), nil, false); err != nil || answer != "ok" {
		t.Fatalf("inference after model change: %q, %v", answer, err)
	}
}

func TestPanelChatGPTCatalogReadsNeverRefreshCredentials(t *testing.T) {
	c := configurationFixture(t, `{"llm_api":"chatgpt","llm_model":"old"}`)
	dir := filepath.Join(t.TempDir(), "private")
	store := chatgpt.Store{Path: filepath.Join(dir, "account.json")}
	var calls atomic.Int32
	store.HTTP = &http.Client{Transport: chatGPTTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Host != "api.openai.com" || r.URL.Path != "/v1/models" {
			return nil, errors.New("unexpected provider request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"demo","visibility":"list"}]}`))}, nil
	})}
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Configuration: c, ChatGPT: &panelChatGPT{store: store}})
	server := httptest.NewServer(mux)
	defer server.Close()
	endpoint := server.URL + "/diagnostics/api/chatgpt/models"
	checkReads := func(wantModels int) {
		t.Helper()
		before := calls.Load()
		for _, method := range []string{"GET", "HEAD"} {
			req, err := http.NewRequestWithContext(t.Context(), method, endpoint, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("%s catalog: status %d, error %v", method, resp.StatusCode, err)
			}
			if method == "HEAD" {
				if len(body) != 0 {
					t.Fatal("HEAD returned a body")
				}
				continue
			}
			var catalog struct {
				Models []llm.Model `json:"models"`
			}
			if err := json.Unmarshal(body, &catalog); err != nil || catalog.Models == nil || len(catalog.Models) != wantModels {
				t.Fatalf("unexpected cached catalog: %s", body)
			}
		}
		if calls.Load() != before {
			t.Fatal("reading the catalog contacted the provider")
		}
	}
	checkReads(0)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("catalog read created the credential directory: %v", err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeCredentials := func(data string) {
		t.Helper()
		if err := os.WriteFile(store.Path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	expired := strings.Replace(subscriptionFixture, "2100-", "2000-", 1)
	writeCredentials(expired)
	checkReads(0)
	if _, err := os.Stat(store.Path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cold catalog read created a credential lock")
	}
	writeCredentials(subscriptionFixture)
	for _, input := range []struct {
		body, mediaType string
		status          int
	}{
		{"{}", "text/plain", 415}, {`{"invalid":true}`, "application/json", 400}, {"", "application/json", 400},
	} {
		res := diagnosticAPIRequest(mux, "POST", "chatgpt/models", input.body, input.mediaType)
		if res.Code != input.status || calls.Load() != 0 {
			t.Fatalf("invalid refresh accepted: status %d, calls %d", res.Code, calls.Load())
		}
	}
	if res := diagnosticAPIRequest(mux, "POST", "chatgpt/models", "{}", "application/json"); res.Code != 200 || calls.Load() != 1 {
		t.Fatalf("refresh failed: status %d, calls %d", res.Code, calls.Load())
	}
	writeCredentials(expired)
	checkReads(1)
	if raw, err := os.ReadFile(store.Path); err != nil || string(raw) != expired {
		t.Fatal("reading a warm catalog changed expired credentials")
	}
	writeCredentials(strings.Replace(expired, "user-fixture", "other-user", 1))
	checkReads(0)
	writeCredentials(strings.Replace(expired, "oaiapp_fixture", "other-client", 1))
	checkReads(0)
	if err := os.Remove(store.Path); err != nil {
		t.Fatal(err)
	}
	checkReads(0)
}

func TestPanelChatGPTInstallerModel(t *testing.T) {
	t.Setenv("QUASAR_LLM_MODEL", "installed")
	c := configurationFixture(t, `{"llm_api":"chatgpt"}`)
	dir := filepath.Dir(c.path)
	t.Setenv("QUASAR_DATA_DIR", dir)
	path := filepath.Join(dir, "runtime.env")
	const original = "KEEP='value'\n# BEGIN QUASAR SETUP\nQUASAR_LLM_MODEL=\"installed\"\nQUASAR_MODE=\"hybrid\"\n# END QUASAR SETUP\n"
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, data := range []string{original + "export QUASAR_LLM_MODEL=manual\n", strings.ReplaceAll(original, "installed", "external"), "QUASAR_LLM_MODEL=installed\n"} {
		write(data)
		if _, err := c.saveChatGPTModel(t.Context(), "installed", "new"); err == nil {
			t.Fatal("manual override accepted")
		}
	}
	write(original)
	if _, err := c.saveChatGPTModel(t.Context(), "installed", "provider/model-$x"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	want := strings.Replace(original, "installed", `provider/model-\$x`, 1)
	if err != nil || string(raw) != want || c.activeModel() != "provider/model-$x" {
		t.Fatal("incorrect installer model change")
	}
	if state, err := c.chatGPTModel(); err != nil || !state.Writable || state.Active != state.Saved {
		t.Fatal("saved and active model diverged")
	}
}
