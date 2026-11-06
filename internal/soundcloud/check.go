package soundcloud

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const accessCheckTimeout = 15 * time.Second

// CheckAccess verifies catalog, HLS audio and optional artwork connectivity from
// the current host. It does not create a playback session or change settings.
func CheckAccess(ctx context.Context, cfg Config) (err error) {
	ctx, cancel := context.WithTimeout(ctx, accessCheckTimeout)
	defer cancel()
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	hc, err := NewHTTPClient(cfg.ProxyURL)
	if err != nil {
		return err
	}
	defer hc.CloseIdleConnections()
	client, err := NewClient(cfg, hc)
	if err != nil {
		return errors.New("soundcloud: cannot initialize access check")
	}
	tracks, err := client.Search(ctx, "music", TrackKind, 3)
	if err != nil {
		return errors.New("soundcloud: catalog access check failed")
	}
	for _, track := range tracks[:min(len(tracks), 3)] {
		upstream, err := client.Stream(ctx, track)
		if errors.Is(err, ErrUnavailable) {
			continue
		}
		if err != nil {
			return errors.New("soundcloud: stream access check failed")
		}
		if err := checkTrackAccess(ctx, client, hc, track, upstream); err != nil {
			return err
		}
		return nil
	}
	return errors.New("soundcloud: no playable track available for access check")
}

func checkTrackAccess(ctx context.Context, client *Client, hc *http.Client, track Track, upstream string) error {
	relay, err := NewRelay("http://probe.invalid", hc)
	if err != nil {
		return errors.New("soundcloud: cannot initialize media access check")
	}
	media, err := relay.Register(track.ArtworkURL, func(ctx context.Context) (string, error) { return client.Stream(ctx, track) })
	if err != nil {
		return errors.New("soundcloud: invalid media for access check")
	}
	u, _ := url.Parse(media.URL)
	root := relay.resources[u.Path]
	root.group.mu.Lock()
	defer root.group.mu.Unlock()
	root.upstream = upstream
	if err := checkPlaylistAccess(ctx, relay, root, make(map[string]bool), 0); err != nil {
		return errors.New("soundcloud: audio access check failed")
	}
	if track.ArtworkURL != "" {
		if err := checkMediaSample(ctx, relay, track.ArtworkURL); err != nil {
			return errors.New("soundcloud: artwork access check failed")
		}
	}
	return nil
}

func checkPlaylistAccess(ctx context.Context, relay *Relay, resource *relayResource, seen map[string]bool, depth int) error {
	if depth >= 4 || seen[resource.upstream] {
		return errors.New("soundcloud: access check playlist limit reached")
	}
	seen[resource.upstream] = true
	body, err := relay.manifest(ctx, resource, true)
	if err != nil {
		return err
	}
	var audioRendition string
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "#EXT-X-KEY:") || strings.HasPrefix(line, "#EXT-X-MAP:") {
				for _, match := range relayURIAttribute.FindAllStringSubmatch(line, -1) {
					if err := checkPlaylistChild(ctx, relay, match[1], seen, depth); err != nil {
						return err
					}
				}
			}
			if strings.HasPrefix(line, "#EXT-X-MEDIA:") && strings.Contains(line, "TYPE=AUDIO") {
				if matches := relayURIAttribute.FindStringSubmatch(line); len(matches) == 2 && audioRendition == "" {
					audioRendition = matches[1]
				}
			}
			continue
		}
		return checkPlaylistChild(ctx, relay, line, seen, depth)
	}
	if audioRendition != "" {
		return checkPlaylistChild(ctx, relay, audioRendition, seen, depth)
	}
	return errors.New("soundcloud: HLS access check found no audio segment")
}

func checkPlaylistChild(ctx context.Context, relay *Relay, raw string, seen map[string]bool, depth int) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("soundcloud: invalid access check resource")
	}
	child := relay.resources[u.Path]
	if child == nil || !child.active {
		return errors.New("soundcloud: missing access check resource")
	}
	if child.manifest {
		return checkPlaylistAccess(ctx, relay, child, seen, depth+1)
	}
	return checkMediaSample(ctx, relay, child.upstream)
}

func checkMediaSample(ctx context.Context, relay *Relay, raw string) error {
	response, err := relay.fetch(ctx, http.MethodGet, raw, "bytes=0-1023")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return errors.New("soundcloud: access check media unavailable")
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "text/html") || strings.HasPrefix(contentType, "application/json") {
		return errors.New("soundcloud: access check received an invalid media response")
	}
	bytes, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil || len(bytes) == 0 {
		return errors.New("soundcloud: access check received no media data")
	}
	return nil
}
