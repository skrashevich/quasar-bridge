package soundcloud

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureTransport struct {
	base *url.URL
	next http.RoundTripper
}

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL = r.URL.Clone()
	r.URL.Scheme = f.base.Scheme
	r.URL.Host = f.base.Host
	return f.next.RoundTrip(r)
}
func fixture(t *testing.T, h http.HandlerFunc) *http.Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	return &http.Client{Transport: fixtureTransport{u, http.DefaultTransport}}
}

func TestOfficialRefreshPaginationAndCanonicalTrack(t *testing.T) {
	var refresh atomic.Int32
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" {
				t.Errorf("unexpected grant: %v", r.Form)
			}
			refresh.Add(1)
			fmt.Fprint(w, `{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600}`)
		case "/tracks":
			if r.Header.Get("Authorization") == "OAuth old-token" {
				w.WriteHeader(401)
				return
			}
			if r.Header.Get("Authorization") != "OAuth new-token" {
				t.Errorf("bad authorization")
			}
			if r.URL.Query().Get("offset") == "2" {
				fmt.Fprint(w, `{"collection":[{"urn":"soundcloud:tracks:3","title":"third","access":"playable"}],"next_href":null}`)
				return
			}
			fmt.Fprint(w, `{"collection":[{"urn":"soundcloud:tracks:1","title":"Song","duration":125000,"access":"playable","user":{"urn":"soundcloud:users:4","username":"Uploader","avatar_url":"https://i1.sndcdn.com/avatar-large.jpg"},"publisher_metadata":{"artist":"Canonical"}},{"urn":"soundcloud:tracks:2","title":"blocked","access":"blocked"}],"next_href":"https://api.soundcloud.com/tracks?offset=2"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	c, err := NewClient(Config{Enabled: true, API: "official", AccessToken: "old-token", RefreshToken: "old-refresh", ClientID: "id", ClientSecret: "secret"}, hc)
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := c.Search(t.Context(), "Song", TrackKind, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 || tracks[0].Artist != "Canonical" || tracks[0].Uploader != "Uploader" || tracks[0].ArtworkURL != "https://i1.sndcdn.com/avatar-large.jpg" || tracks[0].DurationMS != 125000 {
		t.Fatalf("tracks: %+v", tracks)
	}
	if refresh.Load() != 1 {
		t.Fatal("expected one refresh")
	}
}
func TestWebDiscoveryCachedAndAACStream(t *testing.T) {
	var homepage atomic.Int32
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			homepage.Add(1)
			fmt.Fprint(w, `<script src="https://a-v2.sndcdn.com/assets/app.js"></script>`)
		case "/assets/app.js":
			fmt.Fprint(w, `var config={client_id:"0123456789abcdef0123456789abcdef"};`)
		case "/search/tracks":
			if r.URL.Query().Get("client_id") == "" {
				t.Error("no client ID")
			}
			fmt.Fprint(w, `{"collection":[{"id":12,"title":"Song","policy":"ALLOW","streamable":true,"user":{"id":4,"username":"Artist"},"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/96","preset":"aac_96k","format":{"protocol":"hls","mime_type":"audio/mp4"}},{"url":"https://api-v2.soundcloud.com/media/160","preset":"aac_160k","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}]}`)
		case "/media/160":
			fmt.Fprint(w, `{"url":"https://cf-hls-media.sndcdn.com/playlist.m3u8?Policy=secret"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	c, err := NewClient(Config{}, hc)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		tracks, err := c.Search(t.Context(), "Song", TrackKind, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(tracks) != 1 {
			t.Fatalf("tracks: %+v", tracks)
		}
		s, err := c.Stream(t.Context(), tracks[0])
		if err != nil || !strings.Contains(s, "playlist.m3u8") {
			t.Fatalf("stream %q %v", s, err)
		}
	}
	if homepage.Load() != 1 {
		t.Fatalf("discovery count %d", homepage.Load())
	}
	if _, err := c.Likes(context.Background(), 1); err == nil {
		t.Fatal("web likes must require authorization")
	}
}
func TestAssetURLRestrictions(t *testing.T) {
	for _, s := range []string{"http://i1.sndcdn.com/a", "https://localhost/a", "https://127.0.0.1/a", "https://sndcdn.com.evil.test/a", "https://user:pass@api.soundcloud.com/a", "https://api.soundcloud.com:444/a"} {
		u, _ := url.Parse(s)
		if ValidateAssetURL(u) == nil {
			t.Errorf("allowed %s", s)
		}
	}
	u, _ := url.Parse("https://cf-hls-media.sndcdn.com/a?secret=ok")
	if err := ValidateAssetURL(u); err != nil {
		t.Fatal(err)
	}
}

func TestOfficialCollectionsResolveAndStreams(t *testing.T) {
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users":
			fmt.Fprint(w, `[{"kind":"user","urn":"soundcloud:users:9"}]`)
		case "/users/soundcloud:users:9/tracks":
			fmt.Fprint(w, `[{"urn":"soundcloud:tracks:10","title":"Artist song","metadata_artist":"Canonical","user":{"username":"Uploader"}}]`)
		case "/playlists":
			fmt.Fprint(w, `[{"kind":"playlist","urn":"soundcloud:playlists:7","playlist_type":"album"}]`)
		case "/playlists/soundcloud:playlists:7/tracks":
			fmt.Fprint(w, `{"collection":[{"urn":"soundcloud:tracks:11","title":"Playlist song"}]}`)
		case "/resolve":
			http.Redirect(w, r, "https://api.soundcloud.com/tracks/soundcloud:tracks:10", http.StatusFound)
		case "/tracks/soundcloud:tracks:10":
			fmt.Fprint(w, `{"kind":"track","urn":"soundcloud:tracks:10","title":"Resolved song","access":"playable"}`)
		case "/tracks/soundcloud:tracks:10/streams":
			fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-hls-media.sndcdn.com/a.m3u8","preview_mp3_128_url":"https://cf-media.sndcdn.com/preview.mp3"}`)
		case "/tracks/soundcloud:tracks:10/related":
			fmt.Fprint(w, `[{"urn":"soundcloud:tracks:11","title":"Related song"}]`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	c, err := NewClient(Config{AccessToken: "user-token"}, hc)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind Kind
		want string
	}{{ArtistKind, "Artist song"}, {AlbumKind, "Playlist song"}, {PlaylistKind, "Playlist song"}} {
		tracks, e := c.Search(t.Context(), "query", tc.kind, 10)
		if e != nil || len(tracks) != 1 || tracks[0].Title != tc.want {
			t.Fatalf("%s: %+v %v", tc.kind, tracks, e)
		}
	}
	tracks, e := c.Resolve(t.Context(), "https://soundcloud.com/artist/song", 10)
	if e != nil || len(tracks) != 1 || tracks[0].Title != "Resolved song" {
		t.Fatalf("resolve: %+v %v", tracks, e)
	}
	stream, e := c.Stream(t.Context(), tracks[0])
	if e != nil || stream != "https://cf-hls-media.sndcdn.com/a.m3u8" {
		t.Fatalf("stream: %q %v", stream, e)
	}
	related, e := c.Related(t.Context(), tracks[0], 1)
	if e != nil || len(related) != 1 || related[0].Title != "Related song" {
		t.Fatalf("related: %+v %v", related, e)
	}
}
func TestClientCredentialsCannotLikeAndPrivateTokenPersistence(t *testing.T) {
	tokenFile := t.TempDir() + "/tokens.json"
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			r.ParseForm()
			if r.Form.Get("grant_type") != "client_credentials" {
				t.Error("wrong grant")
			}
			fmt.Fprint(w, `{"access_token":"app-token","expires_in":3600}`)
		case "/tracks":
			fmt.Fprint(w, `[]`)
		default:
			t.Error("unexpected request")
			w.WriteHeader(500)
		}
	})
	c, e := NewClient(Config{ClientID: "app", ClientSecret: "secret", TokenFile: tokenFile}, hc)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Search(t.Context(), "query", TrackKind, 1); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Likes(t.Context(), 1); !errors.Is(e, ErrAuthorization) {
		t.Fatalf("likes: %v", e)
	}
	if e = c.Like(t.Context(), Track{URN: "soundcloud:tracks:1"}, true); !errors.Is(e, ErrAuthorization) {
		t.Fatalf("like: %v", e)
	}
	info, e := os.Stat(tokenFile)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("token file not private: %v %v", info, e)
	}
	loaded, e := NewClient(Config{TokenFile: tokenFile}, hc)
	if e != nil {
		t.Fatal(e)
	}
	if loaded.mode != "official" || loaded.authorizedUser() {
		t.Fatalf("loaded token authorization changed")
	}
}
func TestWebDiscoveryRefreshAndPlaylistHydration(t *testing.T) {
	var discovery atomic.Int32
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			discovery.Add(1)
			fmt.Fprint(w, `<script src="https://a-v2.sndcdn.com/app.js"></script>`)
		case "/app.js":
			fmt.Fprint(w, `client_id:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`)
		case "/resolve":
			if r.URL.Query().Get("client_id") == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"kind":"playlist","id":4,"tracks":[{"id":1},{"id":2,"title":"Blocked","policy":"SNIP"},{"id":3,"title":"Third"}]}`)
		case "/tracks/1":
			fmt.Fprint(w, `{"id":1,"title":"First","policy":"ALLOW","media":{"transcodings":[]}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	c, e := NewClient(Config{API: "web", ClientID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, hc)
	if e != nil {
		t.Fatal(e)
	}
	tracks, e := c.Resolve(t.Context(), "https://soundcloud.com/a/sets/b", 3)
	if e != nil || len(tracks) != 2 || tracks[0].Title != "First" || tracks[1].Title != "Third" {
		t.Fatalf("tracks %+v %v", tracks, e)
	}
	if discovery.Load() != 1 {
		t.Fatal("must rediscover once")
	}
}
func TestForbiddenPaginationAndCredentialRedirect(t *testing.T) {
	var leaked atomic.Bool
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tracks":
			fmt.Fprint(w, `{"collection":[],"next_href":"https://evil.example/tracks?access_token=secret"}`)
		case "/oauth/token":
			http.Redirect(w, r, "https://i1.sndcdn.com/leak", http.StatusTemporaryRedirect)
		case "/leak":
			leaked.Store(true)
			fmt.Fprint(w, `{"access_token":"oops"}`)
		}
	})
	c, _ := NewClient(Config{AccessToken: "secret"}, hc)
	_, e := c.Search(t.Context(), "query", TrackKind, 2)
	if e == nil || strings.Contains(e.Error(), "secret") {
		t.Fatalf("unsafe pagination error: %v", e)
	}
	c, _ = NewClient(Config{ClientID: "app", ClientSecret: "secret"}, hc)
	_, e = c.Search(t.Context(), "query", TrackKind, 2)
	if e == nil || leaked.Load() {
		t.Fatalf("token body redirected: error %v leaked %v", e, leaked.Load())
	}
}

func TestUserLikesAndLikeUnlike(t *testing.T) {
	var actions []string
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "OAuth user-token" {
			t.Error("missing user authorization")
		}
		switch r.URL.Path {
		case "/me/likes/tracks":
			fmt.Fprint(w, `{"collection":[{"track":{"urn":"soundcloud:tracks:5","title":"Liked song"}}]}`)
		case "/likes/tracks/soundcloud:tracks:5":
			actions = append(actions, r.Method)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	c, e := NewClient(Config{AccessToken: "user-token"}, hc)
	if e != nil {
		t.Fatal(e)
	}
	tracks, e := c.Likes(t.Context(), 4)
	if e != nil || len(tracks) != 1 || tracks[0].Title != "Liked song" {
		t.Fatalf("likes %+v %v", tracks, e)
	}
	for _, like := range []bool{true, false} {
		if e = c.Like(t.Context(), tracks[0], like); e != nil {
			t.Fatal(e)
		}
	}
	if strings.Join(actions, ",") != "POST,DELETE" {
		t.Fatalf("actions %v", actions)
	}
}
func TestAAC96Fallback(t *testing.T) {
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/160":
			w.WriteHeader(http.StatusNotFound)
		case "/media/96":
			fmt.Fprint(w, `{"url":"https://cf-hls-media.sndcdn.com/fallback.m3u8"}`)
		default:
			t.Error("unexpected request")
			w.WriteHeader(404)
		}
	})
	c, e := NewClient(Config{API: "web", ClientID: "public-client"}, hc)
	if e != nil {
		t.Fatal(e)
	}
	var i catalogItem
	if e = json.Unmarshal([]byte(`{"id":1,"title":"Full song","media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/160","preset":"aac_160k","format":{"protocol":"hls","mime_type":"audio/mp4"}},{"url":"https://api-v2.soundcloud.com/media/96","preset":"aac_96k","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}`), &i); e != nil {
		t.Fatal(e)
	}
	track, _ := i.track()
	s, e := c.Stream(t.Context(), track)
	if e != nil || s != "https://cf-hls-media.sndcdn.com/fallback.m3u8" {
		t.Fatalf("stream %q %v", s, e)
	}
}

func TestPaginationFillsPlayableQueueAndRelativeNext(t *testing.T) {
	var pages atomic.Int32
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tracks" {
			t.Errorf("relative pagination lost path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		pages.Add(1)
		if r.URL.Query().Get("offset") == "1" {
			fmt.Fprint(w, `{"collection":[{"id":2,"title":"Second"}]}`)
			return
		}
		fmt.Fprint(w, `{"collection":[{"id":1,"title":"Preview","access":"preview"}],"next_href":"?offset=1"}`)
	})
	c, _ := NewClient(Config{AccessToken: "user-token"}, hc)
	tracks, e := c.Search(t.Context(), "query", TrackKind, 1)
	if e != nil || len(tracks) != 1 || tracks[0].Title != "Second" || pages.Load() != 2 {
		t.Fatalf("tracks %+v pages %d error %v", tracks, pages.Load(), e)
	}
}
func TestConfigurationAndBlockedTracks(t *testing.T) {
	if e := (Config{}).Validate(); e != nil {
		t.Fatal(e)
	}
	for _, cfg := range []Config{{QueueLimit: -1}, {QueueLimit: 101}, {API: "bad"}, {BaseURL: "https://user:password@bridge.example/"}} {
		if cfg.Validate() == nil {
			t.Errorf("invalid config accepted %+v", cfg)
		}
	}
	for _, fixture := range []string{`{"id":1,"title":"private","sharing":"private"}`, `{"id":2,"title":"blocked","policy":"BLOCK"}`, `{"id":3,"title":"preview","policy":"SNIP"}`, `{"id":4,"title":"non-streamable","streamable":false}`} {
		var i catalogItem
		if e := json.Unmarshal([]byte(fixture), &i); e != nil {
			t.Fatal(e)
		}
		if _, ok := i.track(); ok {
			t.Errorf("unavailable item playable: %s", fixture)
		}
	}
	c, _ := NewClient(Config{ClientID: "public-id"}, nil)
	if c.mode != "web" {
		t.Fatal("lone public ID selected official")
	}
}

func TestJSONArrayWhitespace(t *testing.T) {
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "\n  [ {\"id\":1,\"title\":\"Song\"} ]\n") })
	c, _ := NewClient(Config{AccessToken: "user-token"}, hc)
	tracks, e := c.Search(t.Context(), "Song", TrackKind, 1)
	if e != nil || len(tracks) != 1 {
		t.Fatalf("tracks %+v error %v", tracks, e)
	}
}

func TestUserOperationsNeverFallBackToClientCredentials(t *testing.T) {
	var appGrants atomic.Int32
	var userRequests atomic.Int32
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			appGrants.Add(1)
			fmt.Fprint(w, `{"access_token":"app-token"}`)
			return
		}
		if r.Header.Get("Authorization") == "OAuth app-token" {
			t.Error("personal operation used app principal")
			w.WriteHeader(204)
			return
		}
		userRequests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	c, e := NewClient(Config{AccessToken: "expired-user-token", ClientID: "app", ClientSecret: "secret"}, hc)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Likes(t.Context(), 1); !errors.Is(e, ErrAuthorization) {
		t.Fatalf("likes error %v", e)
	}
	if e = c.Like(t.Context(), Track{URN: "soundcloud:tracks:1"}, true); !errors.Is(e, ErrAuthorization) {
		t.Fatalf("like error %v", e)
	}
	if appGrants.Load() != 0 || userRequests.Load() != 2 {
		t.Fatalf("app grants %d user requests %d", appGrants.Load(), userRequests.Load())
	}
}
func TestOfficialAlbumSearchFiltersBeforePaginationLimit(t *testing.T) {
	var searchPages atomic.Int32
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/playlists":
			searchPages.Add(1)
			if r.URL.Query().Get("offset") == "1" {
				fmt.Fprint(w, `{"collection":[{"urn":"soundcloud:playlists:9","title":"Album","playlist_type":"album"}]}`)
				return
			}
			fmt.Fprint(w, `{"collection":[{"urn":"soundcloud:playlists:8","title":"Ordinary playlist","playlist_type":"playlist"}],"next_href":"?offset=1"}`)
		case "/playlists/soundcloud:playlists:9/tracks":
			fmt.Fprint(w, `[{"urn":"soundcloud:tracks:1","title":"Album song"}]`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	c, _ := NewClient(Config{AccessToken: "user-token"}, hc)
	tracks, e := c.Search(t.Context(), "album", AlbumKind, 1)
	if e != nil || len(tracks) != 1 || tracks[0].Title != "Album song" || searchPages.Load() != 2 {
		t.Fatalf("tracks %+v pages %d error %v", tracks, searchPages.Load(), e)
	}
}

func TestWebHydrationInspectionBudget(t *testing.T) {
	var hydrated atomic.Int32
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/resolve" {
			fmt.Fprint(w, `{"kind":"playlist","id":9,"tracks":[`)
			for i := 1; i <= 500; i++ {
				if i > 1 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"id":%d}`, i)
			}
			fmt.Fprint(w, `]}`)
			return
		}
		hydrated.Add(1)
		fmt.Fprint(w, `{"id":1,"title":"Unavailable","policy":"BLOCK"}`)
	})
	c, _ := NewClient(Config{API: "web", ClientID: "public-id"}, hc)
	tracks, e := c.Resolve(t.Context(), "https://soundcloud.com/u/sets/s", 1)
	if !errors.Is(e, ErrHydrationBudget) || len(tracks) != 0 || hydrated.Load() != 20 {
		t.Fatalf("tracks %d hydrated %d error %v", len(tracks), hydrated.Load(), e)
	}
}
func TestWebHydrationSkipsMissingAndPreservesOrder(t *testing.T) {
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/resolve":
			fmt.Fprint(w, `{"kind":"playlist","id":9,"tracks":[{"id":1},{"id":2},{"id":3},{"id":4}]}`)
		case "/tracks/1":
			w.WriteHeader(http.StatusNotFound)
		case "/tracks/2":
			fmt.Fprint(w, `{"id":2,"title":"First playable"}`)
		case "/tracks/3":
			w.WriteHeader(http.StatusGone)
		case "/tracks/4":
			fmt.Fprint(w, `{"id":4,"title":"Second playable"}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	c, _ := NewClient(Config{API: "web", ClientID: "public-id"}, hc)
	tracks, e := c.Resolve(t.Context(), "https://soundcloud.com/u/sets/s", 2)
	if e != nil || len(tracks) != 2 || tracks[0].Title != "First playable" || tracks[1].Title != "Second playable" {
		t.Fatalf("tracks %+v error %v", tracks, e)
	}
}
func TestWebHydrationPreservesServiceAndContextErrors(t *testing.T) {
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/resolve" {
			fmt.Fprint(w, `{"kind":"playlist","id":9,"tracks":[{"id":1},{"id":2}]}`)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	c, _ := NewClient(Config{API: "web", ClientID: "public-id"}, hc)
	if _, e := c.Resolve(t.Context(), "https://soundcloud.com/u/sets/s", 2); e == nil {
		t.Fatal("service error swallowed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := c.Resolve(ctx, "https://soundcloud.com/u/sets/s", 2); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation lost: %v", e)
	}
}

func TestInjectedNegativeTimeoutIsBounded(t *testing.T) {
	c, e := NewClient(Config{}, &http.Client{Timeout: -1})
	if e != nil || c.http.Timeout <= 0 || c.http.Timeout > 30*time.Second {
		t.Fatalf("timeout %v error %v", c.http.Timeout, e)
	}
}

func TestCollectionSearchContinuesAfterEmptyExpansion(t *testing.T) {
	for _, kind := range []Kind{ArtistKind, PlaylistKind, AlbumKind} {
		t.Run(string(kind), func(t *testing.T) {
			var secondPage atomic.Bool
			hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/users", "/playlists":
					collection := "users"
					resourceKind := "user"
					extra := ""
					if kind != ArtistKind {
						collection = "playlists"
						resourceKind = "playlist"
						extra = `,"playlist_type":"album"`
					}
					if r.URL.Query().Get("offset") == "1" {
						secondPage.Store(true)
						fmt.Fprintf(w, `{"collection":[{"kind":"%s","urn":"soundcloud:%s:2"%s}]}`, resourceKind, collection, extra)
						return
					}
					fmt.Fprintf(w, `{"collection":[{"kind":"%s","urn":"soundcloud:%s:1"%s}],"next_href":"?offset=1"}`, resourceKind, collection, extra)
				case "/users/soundcloud:users:1/tracks", "/playlists/soundcloud:playlists:1/tracks":
					fmt.Fprint(w, `[]`)
				case "/users/soundcloud:users:2/tracks", "/playlists/soundcloud:playlists:2/tracks":
					fmt.Fprint(w, `[{"urn":"soundcloud:tracks:3","title":"Later playable"}]`)
				default:
					t.Errorf("unexpected %s", r.URL.Path)
					w.WriteHeader(404)
				}
			})
			c, _ := NewClient(Config{AccessToken: "user-token"}, hc)
			tracks, e := c.Search(t.Context(), "query", kind, 1)
			if e != nil || len(tracks) != 1 || tracks[0].Title != "Later playable" || !secondPage.Load() {
				t.Fatalf("tracks %+v next %v error %v", tracks, secondPage.Load(), e)
			}
		})
	}
}

func TestCollectionSearchInspectionBudget(t *testing.T) {
	var expansions atomic.Int32
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users" {
			fmt.Fprint(w, `{"collection":[`)
			for i := 1; i <= 100; i++ {
				if i > 1 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"urn":"soundcloud:users:%d"}`, i)
			}
			fmt.Fprint(w, `]}`)
			return
		}
		expansions.Add(1)
		fmt.Fprint(w, `[]`)
	})
	c, _ := NewClient(Config{AccessToken: "user-token"}, hc)
	tracks, e := c.Search(t.Context(), "query", ArtistKind, 1)
	if !errors.Is(e, ErrCollectionBudget) || len(tracks) != 0 || expansions.Load() != 20 {
		t.Fatalf("tracks %d expansions %d error %v", len(tracks), expansions.Load(), e)
	}
}
