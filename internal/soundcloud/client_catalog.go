package soundcloud

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

func itoa(n int) string { return strconv.Itoa(n) }
func (c *Client) Search(ctx context.Context, query string, kind Kind, limit int) ([]Track, error) {
	limit = c.limit(limit)
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("soundcloud: empty search")
	}
	if len(query) > 2048 {
		return nil, errors.New("soundcloud: search too long")
	}
	path := "/tracks"
	switch kind {
	case TrackKind, "":
	case ArtistKind:
		path = "/users"
	case AlbumKind, PlaylistKind:
		path = "/playlists"
	default:
		return nil, errors.New("soundcloud: unknown search kind")
	}
	if c.mode == "web" {
		path = "/search" + path
	}
	q := url.Values{"q": {query}, "limit": {itoa(limit)}, "linked_partitioning": {"1"}, "access": {"playable"}}
	if kind == AlbumKind && c.mode == "web" {
		q.Set("is_album", "true")
	}
	if kind == TrackKind || kind == "" {
		items, err := c.pages(ctx, c.base()+path, q, limit, true)
		if err != nil {
			return nil, err
		}
		tracks := tracksFrom(items, limit)
		rankTracks(tracks, query)
		return tracks, nil
	}
	var selected []Track
	inspected, hydrationRemaining := 0, inspectionLimit(limit)
	err := c.visitPages(ctx, c.base()+path, q, func(item catalogItem) (bool, error) {
		if inspected >= inspectionLimit(limit) {
			return false, ErrCollectionBudget
		}
		inspected++
		if kind == AlbumKind && !isAlbum(item) {
			return false, nil
		}
		tracks, err := c.expandWithBudget(ctx, item, kind, limit, &hydrationRemaining)
		if errors.Is(err, ErrUnavailable) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if len(tracks) == 0 {
			return false, nil
		}
		selected = tracks
		return true, nil
	})
	return selected, err
}

func rankTracks(tracks []Track, query string) {
	q := strings.ToLower(strings.TrimSpace(query))
	score := func(t Track) int {
		title, artist := strings.ToLower(t.Title), strings.ToLower(t.Artist)
		switch {
		case q == artist+" "+title || q == title+" "+artist:
			return 4
		case q == title:
			return 3
		case strings.Contains(q, title) && strings.Contains(q, artist):
			return 2
		case strings.Contains(title, q):
			return 1
		}
		return 0
	}
	slices.SortStableFunc(tracks, func(a, b Track) int { return score(b) - score(a) })
}
func (c *Client) Resolve(ctx context.Context, permalink string, limit int) ([]Track, error) {
	u, e := url.Parse(permalink)
	if e != nil || ValidateAssetURL(u) != nil || (u.Hostname() != "soundcloud.com" && u.Hostname() != "www.soundcloud.com" && u.Hostname() != "on.soundcloud.com") {
		return nil, errors.New("soundcloud: invalid permalink")
	}
	b, e := c.api(ctx, "GET", c.base()+"/resolve", url.Values{"url": {u.String()}})
	if e != nil {
		return nil, e
	}
	var i catalogItem
	if json.Unmarshal(b, &i) != nil {
		return nil, errors.New("soundcloud: invalid resolved resource")
	}
	kind := TrackKind
	if i.Kind == "user" {
		kind = ArtistKind
	} else if i.Kind == "playlist" || len(i.Tracks) > 0 {
		kind = PlaylistKind
	}
	return c.expand(ctx, i, kind, c.limit(limit))
}
func (c *Client) expand(ctx context.Context, i catalogItem, kind Kind, limit int) ([]Track, error) {
	remaining := inspectionLimit(limit)
	return c.expandWithBudget(ctx, i, kind, limit, &remaining)
}

func (c *Client) expandWithBudget(ctx context.Context, i catalogItem, kind Kind, limit int, hydrationRemaining *int) ([]Track, error) {
	if kind == TrackKind {
		t, ok := i.track()
		if !ok {
			return nil, ErrUnavailable
		}
		return []Track{t}, nil
	}
	if kind == ArtistKind {
		id, e := c.resourcePath(i.urn("users"), "users")
		if e != nil {
			return nil, e
		}
		items, e := c.pages(ctx, c.base()+"/users/"+id+"/tracks", url.Values{"limit": {itoa(limit)}, "linked_partitioning": {"1"}}, limit, true)
		return tracksFrom(items, limit), e
	}
	id, e := c.resourcePath(i.urn("playlists"), "playlists")
	if e != nil {
		return nil, e
	}
	if c.mode == "official" {
		items, e := c.pages(ctx, c.base()+"/playlists/"+id+"/tracks", url.Values{"limit": {itoa(limit)}, "linked_partitioning": {"1"}}, limit, true)
		return tracksFrom(items, limit), e
	}
	if len(i.Tracks) == 0 {
		b, e := c.api(ctx, "GET", c.base()+"/playlists/"+id, nil)
		if e != nil {
			return nil, e
		}
		if json.Unmarshal(b, &i) != nil {
			return nil, errors.New("soundcloud: invalid playlist")
		}
	}
	tracks := make([]Track, 0, min(limit, len(i.Tracks)))
	// Count every inspected candidate, including blocked and missing entries. The
	// request budget is independent of how many playable tracks are found.
	inspectionBudget := *hydrationRemaining
	for _, item := range i.Tracks[:min(len(i.Tracks), inspectionBudget)] {
		*hydrationRemaining--
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if item.Title == "" {
			t, e := c.Track(ctx, item.urn("tracks"))
			if e != nil {
				if errors.Is(e, ErrUnavailable) {
					continue
				}
				return nil, e
			}
			tracks = append(tracks, t)
		} else if t, ok := item.track(); ok {
			tracks = append(tracks, t)
		}
		if len(tracks) >= limit {
			break
		}
	}
	if len(tracks) == 0 && len(i.Tracks) > inspectionBudget {
		return nil, ErrHydrationBudget
	}
	// A partially filled queue remains useful when its inspection budget expires.
	return tracks, nil
}
func (c *Client) Related(ctx context.Context, t Track, limit int) ([]Track, error) {
	id, e := c.resourcePath(t.URN, "tracks")
	if e != nil {
		return nil, e
	}
	limit = c.limit(limit)
	items, e := c.pages(ctx, c.base()+"/tracks/"+id+"/related", url.Values{"limit": {itoa(limit)}, "linked_partitioning": {"1"}}, limit, true)
	return tracksFrom(items, limit), e
}
func (c *Client) Stream(ctx context.Context, t Track) (string, error) {
	if t.Access == "blocked" || t.Access == "preview" {
		return "", ErrUnavailable
	}
	if c.mode == "official" {
		id, e := c.resourcePath(t.URN, "tracks")
		if e != nil {
			return "", e
		}
		b, e := c.api(ctx, "GET", c.base()+"/tracks/"+id+"/streams", nil)
		if e != nil {
			if missingStream(e) {
				return "", ErrUnavailable
			}
			return "", e
		}
		var streams struct {
			AAC160 string `json:"hls_aac_160_url"`
			AAC96  string `json:"hls_aac_96_url"`
			AAC    string `json:"hls_aac_url"`
		}
		if json.Unmarshal(b, &streams) != nil {
			return "", errors.New("soundcloud: invalid streams response")
		}
		for _, s := range []string{streams.AAC160, streams.AAC96, streams.AAC} {
			if s != "" {
				return checkedStream(s)
			}
		}
		return "", ErrUnavailable
	}
	if len(t.transcodings) == 0 {
		full, e := c.Track(ctx, t.URN)
		if e != nil {
			return "", e
		}
		t = full
	}
	candidates := slices.Clone(t.transcodings)
	slices.SortStableFunc(candidates, func(a, b transcoding) int { return streamRank(b) - streamRank(a) })
	var lastErr error
	for _, candidate := range candidates {
		if streamRank(candidate) == 0 {
			continue
		}
		b, e := c.api(ctx, "GET", candidate.URL, url.Values{"track_authorization": {t.trackAuthorization}})
		if e != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// A missing alternate must not erase a transient failure from a
			// different alternate. Any successful full AAC stream still wins.
			if !missingStream(e) {
				lastErr = e
			}
			continue
		}
		var stream struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(b, &stream) != nil {
			return "", errors.New("soundcloud: invalid stream response")
		}
		if stream.URL != "" {
			return checkedStream(stream.URL)
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", ErrUnavailable
}
func streamRank(t transcoding) int {
	if t.Snipped || t.Format.Protocol != "hls" || (!strings.Contains(t.Preset, "aac") && !strings.Contains(t.Format.MIME, "mp4") && !strings.Contains(t.Format.MIME, "aac")) {
		return 0
	}
	if strings.Contains(t.Preset, "160") {
		return 3
	}
	if strings.Contains(t.Preset, "96") {
		return 2
	}
	return 1
}
func checkedStream(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || ValidateAssetURL(u) != nil {
		return "", errors.New("soundcloud: invalid stream origin")
	}
	return raw, nil
}

func isAlbum(i catalogItem) bool {
	return i.IsAlbum || i.PlaylistType == "album" || i.Type == "album"
}

func inspectionLimit(limit int) int { return min(200, max(20, 4*limit)) }

func missingStream(err error) bool {
	status, ok := errors.AsType[*httpStatusError](err)
	return ok && (status.Code == 404 || status.Code == 410)
}
