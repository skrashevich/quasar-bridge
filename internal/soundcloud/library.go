package soundcloud

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

const libraryMaxPlaylists = 100

type Playlist struct {
	URN        string `json:"urn"`
	Title      string `json:"title"`
	TrackCount int    `json:"track_count,omitzero"`
	kind       string
}

type libraryResource struct {
	ID         jsontext.Value `json:"id"`
	URN        string         `json:"urn"`
	Kind       string         `json:"kind"`
	Title      string         `json:"title"`
	Sharing    string         `json:"sharing"`
	TrackCount int            `json:"track_count"`
	Tracks     []catalogItem  `json:"tracks"`
}

type libraryBudget struct{ pages, items int }

// Library lists owned and saved collections using the authenticated catalog.
// Track and album searches never participate in this personal name lookup.
func (c *Client) Library(ctx context.Context, limit int) ([]Playlist, error) {
	if !c.authorizedUser() {
		return nil, ErrAuthorization
	}
	if limit <= 0 {
		limit = c.cfg.QueueLimit
	}
	limit = min(libraryMaxPlaylists, max(1, limit))
	paths := []string{"/me/library/all"}
	if c.mode == "official" {
		paths = []string{"/me/playlists", "/me/likes/playlists", "/me/likes/system-playlists"}
	}
	budget := libraryBudget{pages: 10, items: 500}
	seen := make(map[string]bool)
	var result []Playlist
	for _, path := range paths {
		err := c.libraryPages(ctx, c.base()+path, url.Values{"limit": {itoa(min(50, limit))}, "linked_partitioning": {"true"}}, &budget, func(raw jsontext.Value) (bool, error) {
			var wrapper struct {
				Playlist jsontext.Value `json:"playlist"`
				System   jsontext.Value `json:"system_playlist"`
			}
			if json.Unmarshal(raw, &wrapper) != nil {
				return false, errors.New("soundcloud: invalid library item")
			}
			kind := ""
			if len(wrapper.Playlist) != 0 && string(wrapper.Playlist) != "null" {
				raw, kind = wrapper.Playlist, "playlist"
			} else if len(wrapper.System) != 0 && string(wrapper.System) != "null" {
				raw, kind = wrapper.System, "system-playlist"
			}
			var resource libraryResource
			if json.Unmarshal(raw, &resource) != nil {
				return false, errors.New("soundcloud: invalid library playlist")
			}
			if resource.Kind == "" {
				resource.Kind = kind
			}
			playlist, ok := resource.playlist()
			if ok && !seen[playlist.URN] {
				seen[playlist.URN] = true
				result = append(result, playlist)
			}
			return len(result) >= limit, nil
		})
		if err != nil {
			return nil, err
		}
		if len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (r libraryResource) playlist() (Playlist, bool) {
	if r.Kind != "playlist" && r.Kind != "system-playlist" || strings.TrimSpace(r.Title) == "" || len(r.Title) > 2048 {
		return Playlist{}, false
	}
	id := r.URN
	if id == "" {
		var value string
		if json.Unmarshal(r.ID, &value) != nil {
			value = string(r.ID)
		}
		kind := "playlists"
		if r.Kind == "system-playlist" {
			kind = "system-playlists"
		}
		id = value
		if !strings.HasPrefix(id, "soundcloud:") {
			id = "soundcloud:" + kind + ":" + id
		}
	}
	if r.Kind == "playlist" {
		if _, err := resourceID(id, "playlists"); err != nil {
			return Playlist{}, false
		}
	} else if !validSystemPlaylistID(id) {
		return Playlist{}, false
	}
	return Playlist{URN: id, Title: r.Title, TrackCount: r.TrackCount, kind: r.Kind}, true
}

func validSystemPlaylistID(value string) bool {
	value = strings.TrimPrefix(value, "soundcloud:system-playlists:")
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == ':' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// PersonalPlaylist selects only from the user's library: exact normalized
// titles win before a bounded all-query-words match. It never falls back global.
func (c *Client) PersonalPlaylist(ctx context.Context, name string, limit int) ([]Track, error) {
	query := normalizePlaylistTitle(name)
	if query == "" || len(name) > 2048 {
		return nil, errors.New("soundcloud: missing personal playlist name")
	}
	library, err := c.Library(ctx, libraryMaxPlaylists)
	if err != nil {
		return nil, err
	}
	best, score := -1, 0
	for index, playlist := range library {
		title := normalizePlaylistTitle(playlist.Title)
		current := 0
		if title == query {
			best = index
			break
		}
		words := strings.Fields(query)
		all := true
		for _, word := range words {
			if !strings.Contains(" "+title+" ", " "+word+" ") {
				all = false
				break
			}
		}
		if all {
			current = 1000 - min(999, len(strings.Fields(title))-len(words))
		}
		if current > score {
			best, score = index, current
		}
	}
	if best < 0 {
		return nil, ErrUnavailable
	}
	return c.personalPlaylistTracks(ctx, library[best], c.limit(limit))
}

func normalizePlaylistTitle(value string) string {
	value = strings.Map(func(r rune) rune {
		r = unicode.ToLower(r)
		if r == 'ё' {
			return 'е'
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return r
		}
		return ' '
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

func (c *Client) personalPlaylistTracks(ctx context.Context, playlist Playlist, limit int) ([]Track, error) {
	var resource libraryResource
	path := "/system-playlists/" + url.PathEscape(playlist.URN)
	if playlist.kind == "playlist" {
		id, err := c.resourcePath(playlist.URN, "playlists")
		if err != nil {
			return nil, err
		}
		path = "/playlists/" + id
		if c.mode == "official" {
			return c.personalTrackPages(ctx, c.base()+path+"/tracks", limit)
		}
	}
	{
		b, err := c.api(ctx, http.MethodGet, c.base()+path, nil)
		if err != nil {
			return nil, err
		}
		if json.Unmarshal(b, &resource) != nil {
			return nil, errors.New("soundcloud: invalid personal playlist")
		}
	}
	return c.personalTracks(ctx, resource.Tracks, limit)
}

// Uploaded reads the authenticated user's uploads, never an arbitrary profile.
func (c *Client) Uploaded(ctx context.Context, limit int) ([]Track, error) {
	if !c.authorizedUser() {
		return nil, ErrAuthorization
	}
	path := "/me/tracks"
	if c.mode == "web" {
		account, err := c.Account(ctx)
		if err != nil {
			return nil, err
		}
		path = "/users/" + strconv.FormatInt(account.ID, 10) + "/tracks"
	}
	return c.personalTrackPages(ctx, c.base()+path, c.limit(limit))
}

func (c *Client) personalTrackPages(ctx context.Context, path string, limit int) ([]Track, error) {
	remaining := inspectionLimit(limit)
	var items []catalogItem
	err := c.visitPages(ctx, path, url.Values{"limit": {itoa(limit)}, "linked_partitioning": {"true"}}, func(item catalogItem) (bool, error) {
		items = append(items, item)
		return len(items) >= remaining, nil
	})
	if err != nil {
		return nil, err
	}
	return c.personalTracks(ctx, items, limit)
}

func (c *Client) personalTracks(ctx context.Context, items []catalogItem, limit int) ([]Track, error) {
	budget := inspectionLimit(limit)
	seen := make(map[string]bool)
	var result []Track
	for _, item := range items[:min(len(items), budget)] {
		if item.Track != nil {
			item = *item.Track
		}
		urn := item.urn("tracks")
		if urn == "" || seen[urn] {
			continue
		}
		seen[urn] = true
		track, ok := item.personalTrack()
		if item.Title == "" || (c.mode == "web" && len(track.transcodings) == 0 && ok) {
			var err error
			track, err = c.personalTrack(ctx, urn)
			if errors.Is(err, ErrUnavailable) {
				continue
			}
			if err != nil {
				return nil, err
			}
			ok = true
		}
		if !ok || track.DurationMS <= 0 {
			continue
		}
		if c.mode == "web" {
			full := false
			for _, transcoding := range track.transcodings {
				if streamRank(transcoding) > 0 {
					full = true
					break
				}
			}
			if !full {
				continue
			}
		}
		result = append(result, track)
		if len(result) >= limit {
			break
		}
	}
	if len(result) == 0 && len(items) > budget {
		return nil, ErrHydrationBudget
	}
	return result, nil
}

func (c *Client) libraryPages(ctx context.Context, raw string, query url.Values, budget *libraryBudget, visit func(jsontext.Value) (bool, error)) error {
	seen := make(map[string]bool)
	for raw != "" && !seen[raw] {
		if budget.pages <= 0 {
			return ErrCollectionBudget
		}
		budget.pages--
		seen[raw] = true
		body, err := c.api(ctx, http.MethodGet, raw, query)
		if err != nil {
			return err
		}
		var items []jsontext.Value
		next := ""
		if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
			err = json.Unmarshal(body, &items)
		} else {
			var page struct {
				Items []jsontext.Value `json:"collection"`
				Next  string           `json:"next_href"`
			}
			err = json.Unmarshal(body, &page)
			items, next = page.Items, page.Next
		}
		if err != nil {
			return errors.New("soundcloud: invalid library page")
		}
		for _, item := range items {
			if budget.items <= 0 {
				return ErrCollectionBudget
			}
			budget.items--
			done, err := visit(item)
			if err != nil || done {
				return err
			}
		}
		if next == "" {
			return nil
		}
		current, _ := url.Parse(raw)
		link, err := url.Parse(next)
		if err != nil {
			return errors.New("soundcloud: invalid library pagination")
		}
		link = current.ResolveReference(link)
		origin, _ := url.Parse(c.base())
		if link.Scheme != "https" || link.Host != origin.Host || link.User != nil {
			return errors.New("soundcloud: invalid library pagination origin")
		}
		raw, query = link.String(), nil
	}
	return nil
}
