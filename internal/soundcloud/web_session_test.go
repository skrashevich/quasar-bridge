package soundcloud

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebSessionModeAndTokenPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := SaveWebToken(path, "OAuth file-token"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, api, direct, file, mode, token string
	}{
		{"file-preferred-over-official", "auto", "", path, "web", "file-token"},
		{"direct-preferred-over-file", "auto", "direct-token", path, "web", "direct-token"},
		{"explicit-web", "web", "direct-token", path, "web", "direct-token"},
		{"explicit-official", "official", "invalid token", "missing", "official", ""},
		{"missing-web-file-keeps-official", "auto", "", path + ".missing", "official", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewClient(Config{API: tc.api, WebAccessToken: tc.direct, WebTokenFile: tc.file, AccessToken: "official-token"}, nil)
			if err != nil || client.mode != tc.mode || client.webToken != tc.token || !client.authorizedUser() {
				t.Fatal("incorrect credential selection", err)
			}
		})
	}
	if err := os.WriteFile(path, []byte(`{"access_token":"official-file-token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(Config{API: "web", WebTokenFile: path}, nil); err == nil {
		t.Fatal("official token file accepted as a web session")
	}
	if client, err := NewClient(Config{API: "web", AccessToken: "official-token", TokenFile: path + ".bad"}, nil); err != nil || client.authorizedUser() {
		t.Fatal("official credentials leaked into explicit web mode", err)
	}
}

func TestWebSessionStorageIsTypedPrivateAndSeparate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := SaveWebToken(path, " OAuth 2-web-token\n"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("web session was not private", err)
	}
	if token, err := readWebToken(path); err != nil || token != "2-web-token" {
		t.Fatal("web token did not roundtrip", err)
	}
	if err := SaveWebToken(path, "replacement"); err != nil {
		t.Fatal(err)
	}
	if token, err := readWebToken(path); err != nil || token != "replacement" {
		t.Fatal("replacement was not saved", err)
	}
	if err := RemoveWebToken(path); err != nil {
		t.Fatal(err)
	}
	if err := RemoveWebToken(path); err != nil {
		t.Fatal("logout of missing session failed", err)
	}
	official := []byte(`{"access_token":"official","user_authorized":true}`)
	if err := os.WriteFile(path, official, 0600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveWebToken(path); err == nil {
		t.Fatal("logout removed an official credential file")
	}
	if err := SaveWebToken(path, "web-replacement"); err == nil {
		t.Fatal("web login overwrote an official credential file")
	}
	if remaining, err := os.ReadFile(path); err != nil || string(remaining) != string(official) {
		t.Fatal("official credentials changed", err)
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := SaveWebToken(link, "web-replacement"); err == nil {
		t.Fatal("web login replaced a symlink")
	}
	if err := RemoveWebToken(link); err == nil {
		t.Fatal("web logout accepted a symlink")
	}
	for _, raw := range []string{"", "OAuth ", "secret\r\nInjected: header", `{"access_token":"secret"}`, "secret token"} {
		if token, err := NormalizeWebToken(raw); err == nil || token != "" || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid credential was accepted or disclosed")
		}
	}
}

func TestOfficialTokenFileRejectsTypedWebSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.json")
	if err := SaveWebToken(path, "private-browser-token"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	})
	if _, err := NewClient(Config{API: "official", TokenFile: path}, hc); err == nil || strings.Contains(err.Error(), "private-browser-token") || calls != 0 {
		t.Fatal("official loader accepted or disclosed a browser token", err, calls)
	}
	if err := os.WriteFile(path, []byte(`{"access_token":"official-token","user_authorized":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if client, err := NewClient(Config{API: "official", TokenFile: path}, hc); err != nil || !client.authorizedUser() {
		t.Fatal("legacy official token schema changed", err)
	}
}

func TestWebSessionAccountLikesAndMutations(t *testing.T) {
	var mutations []string
	pages := 0
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "OAuth web-user-token" || r.URL.Query().Get("client_id") != "public-client" {
			t.Error("web account request lost its selected user credentials")
		}
		if r.Header.Get("Origin") != "https://soundcloud.com" || r.Header.Get("Referer") != "https://soundcloud.com/" || r.Header.Get("Accept") != "application/json" || r.URL.Query().Get("app_locale") != "en" {
			t.Error("web account request lost its web request context")
		}
		switch r.URL.Path {
		case "/me":
			fmt.Fprint(w, `{"id":42,"username":"Account"}`)
		case "/users/42/track_likes":
			pages++
			if pages == 1 {
				fmt.Fprint(w, `{"collection":[{"track":{"id":1,"title":"First","duration":1000}}],"next_href":"?cursor=second"}`)
			} else {
				fmt.Fprint(w, `{"collection":[{"track":{"id":2,"title":"Second","duration":1000}}]}`)
			}
		case "/users/42/track_likes/1":
			mutations = append(mutations, r.Method)
			body, _ := io.ReadAll(r.Body)
			if r.Method == http.MethodPut && (string(body) != "{}" || r.Header.Get("Content-Type") != "application/json") || r.Method == http.MethodDelete && len(body) != 0 {
				t.Error("like mutation has incorrect JSON body")
			}
			w.WriteHeader(http.StatusNoContent)
		case "/tracks/1", "/tracks/2":
			id := strings.TrimPrefix(r.URL.Path, "/tracks/")
			fmt.Fprintf(w, `{"id":%s,"title":"Full","duration":1000,"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/%s","preset":"aac_160","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}`, id, id)
		default:
			t.Errorf("unexpected web endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	client, err := NewClient(Config{API: "web", WebAccessToken: "web-user-token", ClientID: "public-client"}, hc)
	if err != nil {
		t.Fatal(err)
	}
	account, err := client.Account(t.Context())
	if err != nil || account.ID != 42 || account.Username != "Account" {
		t.Fatal("account validation failed", err)
	}
	tracks, err := client.Likes(t.Context(), 2)
	if err != nil || len(tracks) != 2 || pages != 2 {
		t.Fatal("personal pagination failed", err, len(tracks), pages)
	}
	for _, like := range []bool{true, false} {
		if err := client.Like(t.Context(), tracks[0], like); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(mutations, ",") != "PUT,DELETE" {
		t.Fatal("web mutations did not use native endpoints", mutations)
	}
}

func TestWebSessionSparsePrivateLikesArePersonallyHydrated(t *testing.T) {
	hydrated := 0
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "OAuth web-token" {
			t.Error("personal liked track lost user authorization")
		}
		switch r.URL.Path {
		case "/me":
			fmt.Fprint(w, `{"id":42}`)
		case "/users/42/track_likes":
			fmt.Fprint(w, `{"collection":[{"track":{"id":7}},{"track":{"id":8,"title":"Preview","access":"preview","duration":1000}}]}`)
		case "/tracks/7":
			hydrated++
			fmt.Fprint(w, `{"id":7,"title":"Private liked","sharing":"private","duration":1000,"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/7","preset":"aac_160","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}`)
		case "/media/7":
			fmt.Fprint(w, `{"url":"https://cf-media.sndcdn.com/private.m3u8"}`)
		default:
			t.Error("unexpected personal hydration endpoint", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	client, err := NewClient(Config{API: "web", WebAccessToken: "web-token", ClientID: "public"}, hc)
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := client.Likes(t.Context(), 2)
	if err != nil || len(tracks) != 1 || tracks[0].Title != "Private liked" || hydrated != 1 {
		t.Fatal("sparse private like was not hydrated", err, len(tracks), hydrated)
	}
	if upstream, err := client.Stream(t.Context(), tracks[0]); err != nil || upstream != "https://cf-media.sndcdn.com/private.m3u8" {
		t.Fatal("private like lost its full authorized stream", err)
	}
}

func TestAccountOfficialURNAndWebNumericIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, api, response string
		id                  int64
		valid               bool
	}{
		{"official-urn-only", "official", `{"urn":"soundcloud:users:12345","username":"Account"}`, 12345, true},
		{"web-numeric-only", "web", `{"id":42,"username":"Account"}`, 42, true},
		{"matching-numeric-and-urn", "official", `{"id":42,"urn":"soundcloud:users:42"}`, 42, true},
		{"mismatched-numeric-and-urn", "official", `{"id":42,"urn":"soundcloud:users:43"}`, 0, false},
		{"wrong-resource", "official", `{"urn":"soundcloud:tracks:42"}`, 0, false},
		{"signed-id", "official", `{"urn":"soundcloud:users:+42"}`, 0, false},
		{"missing-identity", "official", `{"username":"Account"}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/me" || r.Header.Get("Authorization") != "OAuth user-token" {
					t.Error("profile request lost its user principal")
				}
				fmt.Fprint(w, tc.response)
			})
			cfg := Config{API: tc.api, AccessToken: "user-token", ClientID: "public"}
			if tc.api == "web" {
				cfg.AccessToken = ""
				cfg.WebAccessToken = "user-token"
			}
			client, err := NewClient(cfg, hc)
			if err != nil {
				t.Fatal(err)
			}
			account, err := client.Account(t.Context())
			if tc.valid && (err != nil || account.ID != tc.id) || !tc.valid && err == nil {
				t.Fatal("invalid profile identity handling", account, err)
			}
			if tc.name == "official-urn-only" && account.URN != "soundcloud:users:12345" {
				t.Fatal("canonical account URN was lost")
			}
		})
	}
}

func TestWebSessionRejectionNeverRefreshesOrDowngrades(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/":
					fmt.Fprint(w, `<script src="https://a-v2.sndcdn.com/session.js"></script>`)
					return
				case "/session.js":
					fmt.Fprint(w, `client_id:"0123456789abcdef0123456789abcdef"`)
					return
				}
				calls++
				if r.URL.Path != "/me" || r.Header.Get("Authorization") != "OAuth web-token" {
					t.Error("web rejection attempted anonymous/application fallback")
				}
				w.WriteHeader(status)
				fmt.Fprint(w, `{"private":"web-token"}`)
			})
			client, err := NewClient(Config{WebAccessToken: "web-token", AccessToken: "official-token", ClientID: "app", ClientSecret: "app-secret"}, hc)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Account(t.Context())
			want := ErrAuthorization
			if status == http.StatusForbidden {
				want = ErrWebChallenge
			}
			if !errors.Is(err, want) || calls != 1 || strings.Contains(err.Error(), "web-token") {
				t.Fatal("rejection was not safely propagated", err, calls)
			}
		})
	}
}

func TestWebSessionDiscoversBrowserIDWithPreservedOfficialCredentials(t *testing.T) {
	for _, api := range []string{"auto", "web"} {
		t.Run(api, func(t *testing.T) {
			const browserID = "0123456789abcdef0123456789abcdef"
			var discovered, profiles int
			hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/":
					if r.Header.Get("Authorization") != "" {
						t.Error("discovery leaked account credentials")
					}
					fmt.Fprint(w, `<script src="https://a-v2.sndcdn.com/browser.js"></script>`)
				case "/browser.js":
					discovered++
					fmt.Fprintf(w, `client_id:"%s"`, browserID)
				case "/me":
					profiles++
					if r.URL.Query().Get("client_id") != browserID || r.Header.Get("Authorization") != "OAuth web-token" {
						t.Error("web account reused official application credentials")
					}
					fmt.Fprint(w, `{"id":42,"username":"Account"}`)
				default:
					t.Error("web account attempted an unexpected official/OAuth endpoint")
					w.WriteHeader(http.StatusNotFound)
				}
			})
			cfg := Config{API: api, WebAccessToken: "web-token", ClientID: "official-app", ClientSecret: "official-secret", AccessToken: "official-token", RefreshToken: "official-refresh", TokenFile: filepath.Join(t.TempDir(), "official.json")}
			cfg.Defaults()
			client, err := NewClient(cfg, hc)
			if err != nil {
				t.Fatal(err)
			}
			account, err := client.Account(t.Context())
			if err != nil || account.ID != 42 || discovered != 1 || profiles != 1 {
				t.Fatal("browser application discovery failed", err, discovered, profiles)
			}
			if client.cfg != cfg {
				t.Fatal("web selection changed saved official credentials")
			}
		})
	}
}

func TestWebSessionCredentialsStayOnCatalogOrigin(t *testing.T) {
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("asset request received web credentials")
		}
		fmt.Fprint(w, "asset")
	})
	client, _ := NewClient(Config{API: "web", WebAccessToken: "web-token", ClientID: "public"}, hc)
	if _, _, err := client.request(t.Context(), http.MethodGet, "https://i1.sndcdn.com/art.jpg", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := client.api(t.Context(), http.MethodGet, "https://i1.sndcdn.com/art.jpg", nil); err == nil {
		t.Fatal("authenticated request allowed outside catalog")
	}
	public, _ := NewClient(Config{API: "web", ClientID: "public"}, hc)
	if _, err := public.Account(t.Context()); !errors.Is(err, ErrAuthorization) {
		t.Fatal("anonymous client claimed an account", err)
	}
}

func TestWebSessionMutationRedirectIsNotSuccessfulLike(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect} {
		for _, like := range []bool{true, false} {
			t.Run(fmt.Sprintf("%d/like=%v", status, like), func(t *testing.T) {
				redirected := 0
				hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/me":
						fmt.Fprint(w, `{"id":42}`)
					case "/users/42/track_likes/1":
						http.Redirect(w, r, "https://api-v2.soundcloud.com/login", status)
					case "/login":
						redirected++
						fmt.Fprint(w, "<html>Sign in</html>")
					default:
						t.Error("unexpected endpoint", r.URL.Path)
					}
				})
				client, _ := NewClient(Config{API: "web", WebAccessToken: "web-token", ClientID: "public"}, hc)
				if err := client.Like(t.Context(), Track{URN: "soundcloud:tracks:1"}, like); err == nil || redirected != 0 {
					t.Fatal("redirected mutation claimed success", err, redirected)
				}
			})
		}
	}
}

func TestWebSessionMutationRequiresTerminalNonChallengeResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		confirmed  bool
	}{
		{"accepted-empty", "", http.StatusAccepted, false},
		{"accepted-json", `{"status":"pending"}`, http.StatusAccepted, false},
		{"html-login", "<html>Sign in</html>", http.StatusOK, false},
		{"json-challenge", `{"challenge":{"url":"https://soundcloud.com/confirm"}}`, http.StatusOK, false},
		{"json-error", `{"errors":["confirmation required"]}`, http.StatusOK, false},
		{"ordinary-track-title", `{"id":1,"title":"CAPTCHA","errors":[]}`, http.StatusOK, true},
		{"no-content", "", http.StatusNoContent, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutations := 0
			hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/me" {
					fmt.Fprint(w, `{"id":42}`)
					return
				}
				mutations++
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			client, _ := NewClient(Config{API: "web", WebAccessToken: "web-token", ClientID: "public"}, hc)
			err := client.Like(t.Context(), Track{URN: "soundcloud:tracks:1"}, true)
			if tc.confirmed && err != nil || !tc.confirmed && !errors.Is(err, ErrWebChallenge) || mutations != 1 {
				t.Fatal("mutation success was not confirmed safely", err, mutations)
			}
		})
	}
}

func TestCatalogRedirectOriginAndPublicAssetCompatibility(t *testing.T) {
	var profile, track, asset int
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me":
			http.Redirect(w, r, "https://soundcloud.com/public-profile", http.StatusFound)
		case "/public-profile":
			profile++
			fmt.Fprint(w, `{"id":99}`)
		case "/resolve":
			http.Redirect(w, r, "https://api.soundcloud.com/tracks/1", http.StatusFound)
		case "/tracks/1":
			track++
			if r.Header.Get("Authorization") != "OAuth official-token" {
				t.Error("same-origin catalog redirect lost its principal")
			}
			fmt.Fprint(w, `{"id":1,"title":"Resolved"}`)
		case "/art.jpg":
			http.Redirect(w, r, "https://cf-media.sndcdn.com/image.jpg", http.StatusFound)
		case "/image.jpg":
			asset++
			if r.Header.Get("Authorization") != "" {
				t.Error("public asset redirect received credentials")
			}
			fmt.Fprint(w, "art")
		}
	})
	web, _ := NewClient(Config{API: "web", WebAccessToken: "web-token", ClientID: "public"}, hc)
	if _, err := web.Account(t.Context()); err == nil || profile != 0 {
		t.Fatal("cross-origin redirect falsely authenticated a public profile", err, profile)
	}
	official, _ := NewClient(Config{API: "official", AccessToken: "official-token"}, hc)
	if tracks, err := official.Resolve(t.Context(), "https://soundcloud.com/a/b", 1); err != nil || len(tracks) != 1 || track != 1 {
		t.Fatal("same-origin official Resolve redirect broke", err, track)
	}
	if _, _, err := web.request(t.Context(), http.MethodGet, "https://i1.sndcdn.com/art.jpg", nil, false); err != nil || asset != 1 {
		t.Fatal("public CDN redirect compatibility broke", err, asset)
	}
}

func TestPersonalTrackFilteringPreservesPublicBoundary(t *testing.T) {
	item := catalogItem{ID: 1, Title: "Private", Sharing: "private"}
	if _, ok := item.track(); ok {
		t.Fatal("public search accepted private metadata")
	}
	if _, ok := item.personalTrack(); !ok {
		t.Fatal("authenticated library rejected private metadata")
	}
	for _, kind := range []string{"blocked", "preview", "snip", "unstreamable"} {
		candidate := item
		switch kind {
		case "blocked", "preview":
			candidate.Access = kind
		case "snip":
			candidate.Policy = "SNIP"
		case "unstreamable":
			candidate.Streamable = new(false)
		}
		if _, ok := candidate.personalTrack(); ok {
			t.Fatal("personal access bypassed a playback restriction", kind)
		}
	}
}
