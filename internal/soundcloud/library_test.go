package soundcloud

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func libraryWebFixture(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "OAuth web-user-token" {
			t.Error("personal request lost web authorization")
		}
		handler(w, r)
	})
	client, err := NewClient(Config{API: "web", WebAccessToken: "web-user-token", ClientID: "public", QueueLimit: 2}, hc)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestPersonalLibraryPaginationDedupAndExactPlaylist(t *testing.T) {
	var list, hydrated atomic.Int32
	client := libraryWebFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/library/all":
			list.Add(1)
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `{"collection":[{"type":"playlist","playlist":{"id":20,"title":"Ночной бег","sharing":"private"}},{"type":"playlist-like","playlist":{"id":10,"title":"Ночной бег remix"}}]}`)
				return
			}
			fmt.Fprint(w, `{"collection":[{"type":"playlist-like","playlist":{"id":10,"title":"Ночной бег remix"}},{"type":"system-playlist-like","system_playlist":{"id":"soundcloud:system-playlists:track-stations:7","title":"Станция"}}],"next_href":"/me/library/all?page=2"}`)
		case "/playlists/20":
			fmt.Fprint(w, `{"kind":"playlist","tracks":[{"id":1,"title":"Private full","sharing":"private","duration":120000,"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/1","preset":"aac_160","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}},{"id":2},{"id":3,"title":"Preview","access":"preview","duration":120000},{"id":1}]}`)
		case "/tracks/2":
			hydrated.Add(1)
			fmt.Fprint(w, `{"id":2,"title":"Hydrated","duration":130000,"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/2","preset":"aac_160","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}`)
		default:
			t.Error("personal lookup escaped the library", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	playlists, err := client.Library(t.Context(), 10)
	if err != nil || len(playlists) != 3 || playlists[2].URN != "soundcloud:playlists:20" {
		t.Fatal("library wrappers or deduplication failed", playlists, err)
	}
	tracks, err := client.PersonalPlaylist(t.Context(), "ночной БЕГ!", 2)
	if err != nil || len(tracks) != 2 || tracks[0].Title != "Private full" || tracks[1].Title != "Hydrated" || hydrated.Load() != 1 {
		t.Fatal("exact private collection expansion failed", tracks, err)
	}
	if list.Load() != 4 {
		t.Fatal("pagination performed unexpected work", list.Load())
	}
	if _, ok := (catalogItem{ID: 1, Title: "Private full", Sharing: "private"}).track(); ok {
		t.Fatal("personal library weakened the public private-track filter")
	}
}

func TestPersonalLibrarySystemPlaylistAndLexicalMatch(t *testing.T) {
	client := libraryWebFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me/library/all" {
			fmt.Fprint(w, `{"collection":[{"type":"system-playlist-like","system_playlist":{"urn":"soundcloud:system-playlists:track-stations:7","title":"Моя ночная станция"}}]}`)
			return
		}
		if r.URL.Path != "/system-playlists/soundcloud:system-playlists:track-stations:7" {
			t.Error("unexpected system playlist endpoint", r.URL.Path)
		}
		fmt.Fprint(w, `{"kind":"system-playlist","tracks":[{"id":7,"title":"Full","duration":120000,"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/7","preset":"aac_160","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}]}`)
	})
	tracks, err := client.PersonalPlaylist(t.Context(), "ночная станция", 1)
	if err != nil || len(tracks) != 1 || tracks[0].URN != "soundcloud:tracks:7" {
		t.Fatal(tracks, err)
	}
}

func TestPersonalLibraryOriginAndPageBudget(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			var calls atomic.Int32
			client := libraryWebFixture(t, func(w http.ResponseWriter, r *http.Request) {
				count := calls.Add(1)
				next := fmt.Sprintf("/me/library/all?page=%d", count)
				if foreign {
					next = "https://foreign.invalid/private"
				}
				fmt.Fprintf(w, `{"collection":[],"next_href":%q}`, next)
			})
			_, err := client.Library(t.Context(), 100)
			if err == nil {
				t.Fatal("unbounded/foreign library pagination accepted")
			}
			want := int32(10)
			if foreign {
				want = 1
			}
			if calls.Load() != want {
				t.Fatal("unexpected pagination budget", calls.Load(), want)
			}
		})
	}
}

func TestPersonalLibraryNotFoundDoesNotSearchGlobal(t *testing.T) {
	client := libraryWebFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/library/all" {
			t.Fatal("personal lookup fell back outside library", r.URL.Path)
		}
		fmt.Fprint(w, `{"collection":[{"type":"playlist","playlist":{"id":1,"title":"Another"}}]}`)
	})
	if _, err := client.PersonalPlaylist(t.Context(), "missing", 2); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing personal playlist was not rejected", err)
	}
}

func TestPersonalLibraryRequiresUserAndSupportsOfficialCollections(t *testing.T) {
	var calls atomic.Int32
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/me/playlists":
			fmt.Fprint(w, `{"collection":[{"id":1,"kind":"playlist","title":"Own"}]}`)
		case "/me/likes/playlists":
			fmt.Fprint(w, `{"collection":[{"id":1,"kind":"playlist","title":"Own"},{"id":2,"kind":"playlist","title":"Saved"}]}`)
		case "/me/likes/system-playlists":
			fmt.Fprint(w, `{"collection":[{"urn":"soundcloud:system-playlists:track-stations:3","kind":"system-playlist","title":"Station"}]}`)
		default:
			t.Error("unexpected official collection endpoint", r.URL.Path)
		}
	})
	public, err := NewClient(Config{API: "web", ClientID: "public"}, hc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := public.Library(t.Context(), 10); !errors.Is(err, ErrAuthorization) || calls.Load() != 0 {
		t.Fatal("public session accessed personal library", err)
	}
	client, err := NewClient(Config{API: "official", AccessToken: "user-token"}, hc)
	if err != nil {
		t.Fatal(err)
	}
	playlists, err := client.Library(t.Context(), 10)
	if err != nil || len(playlists) != 3 || calls.Load() != 3 {
		t.Fatal("official own/liked playlists unavailable", playlists, err)
	}
}

func TestPersonalUploadedTracksUseAuthenticatedAccount(t *testing.T) {
	client := libraryWebFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me":
			fmt.Fprint(w, `{"id":42,"username":"owner"}`)
		case "/users/42/tracks":
			fmt.Fprint(w, `{"collection":[{"id":1,"title":"Private","sharing":"private","duration":120000,"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/1","preset":"aac_160","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}},{"id":2,"title":"Preview","access":"preview","duration":120000}]}`)
		default:
			t.Error("uploads used a foreign profile", r.URL.Path)
		}
	})
	tracks, err := client.Uploaded(t.Context(), 2)
	if err != nil || len(tracks) != 1 || tracks[0].Title != "Private" || strings.Contains(tracks[0].Title, "Preview") {
		t.Fatal(tracks, err)
	}
}
