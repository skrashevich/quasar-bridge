package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quasarbridge/internal/soundcloud"
)

type soundCloudAuthTransport func(*http.Request) (*http.Response, error)

func (f soundCloudAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func soundCloudAuthFixture(t *testing.T) (string, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	session := filepath.Join(dir, "custom session.json")
	raw, err := json.Marshal(map[string]any{"mode": "hybrid", "llm_base_url": "http://localhost/v1", "soundcloud": map[string]any{"api": "official", "client_id": "kept-client", "client_secret": "kept-secret", "access_token": "kept-official", "refresh_token": "kept-refresh", "token_file": filepath.Join(dir, "official.json"), "web_token_file": session, "default": true, "compare_yandex": true}, "airplay": map[string]any{"name": "kept"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path, session, raw
}

func accountSoundCloudHTTP(t *testing.T, token string, status int, onAccount func()) *http.Client {
	t.Helper()
	return &http.Client{Transport: soundCloudAuthTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"id":17,"username":"fixture-user","ignored_secret":"not-public"}`
		code := status
		switch {
		case r.URL.Hostname() == "soundcloud.com":
			body = `<script src="https://a-v2.sndcdn.com/assets/test.js"></script>`
			code = 200
		case r.URL.Hostname() == "a-v2.sndcdn.com":
			body = `client_id:"0123456789abcdef0123456789abcdef"`
			code = 200
		case r.URL.Hostname() == "api-v2.soundcloud.com" && r.URL.Path == "/me":
			if got := r.Header.Get("Authorization"); got != "OAuth "+token {
				t.Errorf("wrong web principal selected")
			}
			if onAccount != nil {
				onAccount()
			}
		default:
			t.Errorf("unexpected auth endpoint %s", r.URL.Hostname()+r.URL.Path)
			code = 500
		}
		if r.URL.Hostname() != "api-v2.soundcloud.com" && r.Header.Get("Authorization") != "" {
			t.Error("web credential left API host")
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
}

func TestSoundCloudAuthLoginStatusLogoutPreserveOfficialCredentials(t *testing.T) {
	path, session, _ := soundCloudAuthFixture(t)
	var output bytes.Buffer
	if err := runSoundCloudAuthHTTP(t.Context(), path, "login", strings.NewReader("OAuth new-web-token\nignored-second-line\n"), &output, accountSoundCloudHTTP(t, "new-web-token", 200, nil)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "new-web-token") || strings.Contains(output.String(), "not-public") {
		t.Fatal("login exposed credential/unapproved response fields")
	}
	cfg, err := readConfigForCommand(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SoundCloud.API != "web" || cfg.SoundCloud.WebTokenFile != session || !cfg.SoundCloud.Enabled || !cfg.SoundCloud.Default || !cfg.SoundCloud.CompareYandex || cfg.SoundCloud.ClientSecret != "kept-secret" || cfg.SoundCloud.AccessToken != "kept-official" || cfg.SoundCloud.RefreshToken != "kept-refresh" || cfg.AirPlay.Name != "kept" {
		t.Fatal("login changed unrelated settings")
	}
	info, err := os.Stat(session)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("session file is not private", err)
	}
	output.Reset()
	if err := runSoundCloudAuthHTTP(t.Context(), path, "status", strings.NewReader(""), &output, accountSoundCloudHTTP(t, "new-web-token", 200, nil)); err != nil {
		t.Fatal(err)
	}
	var status struct {
		Authenticated bool               `json:"authenticated"`
		Account       soundcloud.Account `json:"account"`
	}
	if err := json.Unmarshal(output.Bytes(), &status); err != nil || !status.Authenticated || status.Account.ID != 17 || status.Account.Username != "fixture-user" {
		t.Fatal("unsafe/invalid status", err, output.String())
	}
	if err := runSoundCloudAuthHTTP(t.Context(), path, "logout", strings.NewReader(""), io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(session); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("logout retained web session")
	}
	cfg, err = readConfigForCommand(path, true)
	if err != nil || cfg.SoundCloud.API != "web" || cfg.SoundCloud.AccessToken != "kept-official" || !cfg.SoundCloud.Default || !cfg.SoundCloud.CompareYandex {
		t.Fatal("logout enabled official fallback or changed routing", err)
	}
	output.Reset()
	if err := runSoundCloudAuthHTTP(t.Context(), path, "status", strings.NewReader(""), &output, &http.Client{Transport: soundCloudAuthTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("anonymous status contacted official service")
		return nil, errors.New("unreachable")
	})}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &status); err != nil || status.Authenticated {
		t.Fatal(err, output.String())
	}
}

func TestSoundCloudAuthValidationFailurePreservesPreviousAccount(t *testing.T) {
	path, session, original := soundCloudAuthFixture(t)
	if err := soundcloud.SaveWebToken(session, "old-web-token"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(session)
	var output bytes.Buffer
	err := runSoundCloudAuthHTTP(t.Context(), path, "login", strings.NewReader("private-invalid-token\n"), &output, accountSoundCloudHTTP(t, "private-invalid-token", 401, nil))
	if err == nil || strings.Contains(err.Error(), "private-invalid-token") || strings.Contains(output.String(), "private-invalid-token") {
		t.Fatal("invalid credential accepted or exposed")
	}
	after, _ := os.ReadFile(session)
	configAfter, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || !bytes.Equal(original, configAfter) {
		t.Fatal("failed validation changed account/config")
	}
}

func TestSoundCloudAuthExternalChangesDuringValidationArePreserved(t *testing.T) {
	for _, kind := range []string{"configuration", "session"} {
		t.Run(kind, func(t *testing.T) {
			path, session, original := soundCloudAuthFixture(t)
			soundcloud.SaveWebToken(session, "old-web-token")
			external := []byte(`{"mode":"observe"}`)
			err := runSoundCloudAuthHTTP(t.Context(), path, "login", strings.NewReader("new-web-token\n"), io.Discard, accountSoundCloudHTTP(t, "new-web-token", 200, func() {
				if kind == "configuration" {
					os.WriteFile(path, external, 0600)
				} else {
					soundcloud.SaveWebToken(session, "external-web-token")
				}
			}))
			if err == nil {
				t.Fatal("external change was overwritten")
			}
			after, _ := os.ReadFile(path)
			tokenAfter, _ := os.ReadFile(session)
			if kind == "configuration" && !bytes.Equal(after, external) || kind == "session" && (!bytes.Equal(after, original) || !strings.Contains(string(tokenAfter), "external-web-token")) {
				t.Fatal("external state was not preserved")
			}
		})
	}
}

func TestSoundCloudAuthPersistenceRollbackAndExternalSession(t *testing.T) {
	for _, kind := range []string{"existing", "created", "external", "logout"} {
		t.Run(kind, func(t *testing.T) {
			path, session, original := soundCloudAuthFixture(t)
			if kind != "created" {
				if err := soundcloud.SaveWebToken(session, "old-web-token"); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(session)
			updated, err := soundCloudAuthConfig(original, session, kind != "logout")
			if err != nil {
				t.Fatal(err)
			}
			err = persistSoundCloudSession(path, original, updated, session, "new-web-token", kind == "logout", func(string, []byte, os.FileMode) error {
				if kind == "external" {
					if err := soundcloud.SaveWebToken(session, "external-web-token"); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("private-write-failure")
			})
			if err == nil || strings.Contains(err.Error(), "private-write-failure") {
				t.Fatal("persistence error was not safely reported")
			}
			after, _ := os.ReadFile(session)
			configAfter, _ := os.ReadFile(path)
			if !bytes.Equal(configAfter, original) {
				t.Fatal("write failure changed config")
			}
			if kind == "external" {
				if !strings.Contains(string(after), "external-web-token") {
					t.Fatal("rollback clobbered external account")
				}
			} else if !bytes.Equal(before, after) {
				t.Fatal("rollback lost previous session")
			}
			if kind == "created" {
				if _, err = os.Stat(session); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("rollback retained newly created secret")
				}
			}
		})
	}
}

func TestSoundCloudAuthTokenPathAndEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{}`), 0600)
	cfg, err := readConfigForCommand(path, true)
	if err != nil || cfg.SoundCloud.WebTokenFile != filepath.Join(filepath.Dir(path), "soundcloud-session.json") {
		t.Fatal("default session is not adjacent absolute", err)
	}
	parsed, err := parseConfig([]byte(`{}`), true)
	if err != nil || parsed.SoundCloud.WebTokenFile != "" {
		t.Fatal("pathless parsing acquired a default file", err)
	}
	t.Setenv("QUASAR_SOUNDCLOUD_WEB_TOKEN_FILE", "custom session.json")
	t.Setenv("QUASAR_SOUNDCLOUD_WEB_ACCESS_TOKEN", "env-web-token")
	cfg, err = readConfigForCommand(path, true)
	if err != nil || cfg.SoundCloud.WebAccessToken != "env-web-token" || cfg.SoundCloud.WebTokenFile != filepath.Join(filepath.Dir(path), "custom session.json") {
		t.Fatal("web environment override/default path lost", err)
	}
	if err := runSoundCloudAuth(t.Context(), path, "logout", strings.NewReader(""), io.Discard); err == nil || !strings.Contains(err.Error(), "WEB_ACCESS_TOKEN") {
		t.Fatal("logout ignored active environment credential")
	}
	if err := runSoundCloudAuth(t.Context(), path, "login", strings.NewReader("new-token\n"), io.Discard); err == nil || !strings.Contains(err.Error(), "WEB_ACCESS_TOKEN") {
		t.Fatal("login falsely persisted behind environment override")
	}
}

func TestSoundCloudAuthRefusesCredentialPathCollisions(t *testing.T) {
	for _, kind := range []string{"configuration", "runtime", "official", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			path, session, raw := soundCloudAuthFixture(t)
			var doc map[string]any
			json.Unmarshal(raw, &doc)
			sc := doc["soundcloud"].(map[string]any)
			switch kind {
			case "configuration":
				session = path
			case "runtime":
				session = filepath.Join(filepath.Dir(path), "runtime.env")
				os.WriteFile(session, []byte("KEEP=yes\n"), 0600)
			case "official":
				session = sc["token_file"].(string)
				os.WriteFile(session, []byte(`{"access_token":"official"}`), 0600)
			case "directory":
				session = filepath.Dir(path)
			case "symlink":
				target := filepath.Join(filepath.Dir(path), "target")
				os.WriteFile(target, []byte("kept"), 0600)
				if err := os.Symlink(target, session); err != nil {
					t.Fatal(err)
				}
			}
			sc["web_token_file"] = session
			updated, _ := json.Marshal(doc)
			os.WriteFile(path, updated, 0600)
			if err := runSoundCloudAuthHTTP(t.Context(), path, "login", strings.NewReader("new-token\n"), io.Discard, accountSoundCloudHTTP(t, "new-token", 200, nil)); err == nil {
				t.Fatal("protected path accepted")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(after, updated) {
				t.Fatal("path collision changed configuration")
			}
		})
	}
}

func TestSoundCloudAuthBoundedFirstLineAndProxy(t *testing.T) {
	token, err := readSoundCloudWebToken(strings.NewReader("OAuth first-token\nsecond-token\n"))
	if err != nil || token != "first-token" {
		t.Fatal("first-line normalization failed", err)
	}
	for _, value := range []string{"", "\n", strings.Repeat("a", soundCloudTokenInputLimit+1), "secret token\n"} {
		if _, err := readSoundCloudWebToken(strings.NewReader(value)); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	proxy, targets := rejectingSoundCloudProxy(t)
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{"mode": "observe", "soundcloud": map[string]any{"client_id": "fixture", "proxy_url": proxy}})
	os.WriteFile(path, raw, 0600)
	if err := runSoundCloudAuth(t.Context(), path, "login", strings.NewReader("new-web-token\n"), io.Discard); err == nil {
		t.Fatal("refused SOCKS connection ignored")
	}
	select {
	case target := <-targets:
		if target != "api-v2.soundcloud.com" {
			t.Fatal("account validation bypassed configured SOCKS proxy")
		}
	case <-time.After(time.Second):
		t.Fatal("proxy was not used")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, raw) {
		t.Fatal("proxy failure changed configuration")
	}
}

func TestSoundCloudAuthCommandBeforeModelAndMutualExclusion(t *testing.T) {
	if os.Getenv("QUASAR_SC_AUTH_CHILD") == "1" {
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", os.Getenv("QUASAR_SC_AUTH_CONFIG"), "-soundcloud-auth", "token-file"}
		if os.Getenv("QUASAR_SC_AUTH_CONFLICT") == "1" {
			os.Args = append(os.Args, "-check-config")
		}
		main()
		return
	}
	path := filepath.Join(t.TempDir(), "config.json")
	original := []byte(`{"mode":"hybrid","llm_base_url":"http://localhost/v1"}`)
	os.WriteFile(path, original, 0600)
	for _, conflict := range []bool{false, true} {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSoundCloudAuthCommandBeforeModelAndMutualExclusion$")
		cmd.Env = append(os.Environ(), "QUASAR_SC_AUTH_CHILD=1", "QUASAR_SC_AUTH_CONFIG="+path)
		if conflict {
			cmd.Env = append(cmd.Env, "QUASAR_SC_AUTH_CONFLICT=1")
		}
		output, err := cmd.CombinedOutput()
		if conflict && err == nil || !conflict && (err != nil || !strings.Contains(string(output), "soundcloud-session.json")) {
			t.Fatalf("auth diagnostic/model handling incorrect: %v %s", err, output)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, original) {
		t.Fatal("diagnostic changed configuration")
	}
}

func TestSoundCloudDerivedSessionPathDoesNotRequireDefaultReloadRestart(t *testing.T) {
	for _, custom := range []string{"", "custom session.json"} {
		body := map[string]any{"mode": "hybrid", "llm_base_url": "http://localhost/v1", "llm_model": "fixture", "soundcloud": map[string]any{"enabled": true, "api": "web"}}
		if custom != "" {
			body["soundcloud"].(map[string]any)["web_token_file"] = custom
		}
		encoded, _ := json.Marshal(body)
		c := configurationFixture(t, string(encoded))
		p := soundcloud.NewProvider(c.active.SoundCloud, nil, nil)
		c.engine.SoundCloud = p
		value, err := c.Set(t.Context(), map[string]jsontext.Value{"soundcloud.default": jsontext.Value(`true`)})
		if err != nil || value.(configurationResult).RestartRequired {
			t.Fatal("derived session path caused false restart requirement", value, err)
		}
		if _, err = c.Apply(t.Context(), "reload"); err != nil || !p.IsDefault() {
			t.Fatal("derived session path blocked native default reload", err)
		}
		saved, _ := os.ReadFile(c.path)
		var doc map[string]any
		json.Unmarshal(saved, &doc)
		web, specified := doc["soundcloud"].(map[string]any)["web_token_file"]
		if custom == "" && specified || custom != "" && web != custom {
			t.Fatal("preview persisted/rewrote unrelated derived path", web)
		}
	}
}
