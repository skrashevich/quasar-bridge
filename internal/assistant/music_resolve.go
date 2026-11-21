package assistant

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"quasarbridge/internal/webaddress"
)

// MusicResolution describes discovered audio without starting playback.
type MusicResolution struct {
	Source string    `json:"source"`
	Tracks []WebLink `json:"tracks,omitempty"`
}

func normalizeMusicSource(source string) (string, error) {
	raw := musicStartSource(source)
	if raw == "" {
		raw = webaddress.Normalize(strings.TrimSpace(source))
	}
	u, err := url.Parse(raw)
	if len(raw) > 2048 || err != nil || validateWebURL(u) != nil || u.Fragment != "" {
		return "", errors.New("invalid music source: public HTTP/HTTPS URL required")
	}
	return u.String(), nil
}

// ResolveMusic uses the same page discovery as play_music without a speaker or queue.
func (w *WebReader) ResolveMusic(ctx context.Context, source string) (MusicResolution, error) {
	raw, err := normalizeMusicSource(source)
	if err != nil {
		return MusicResolution{}, err
	}
	result := MusicResolution{Source: raw}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	tracks := []WebLink{{URL: raw}}
	var sectionErr error
	scriptBudget := 3
	if mediaExtension(raw) == "" {
		page, err := w.Read(ctx, raw)
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err != nil {
			return result, err
		}
		tracks, sectionErr = w.resolvePageMusic(ctx, page, &scriptBudget)
		if len(tracks) == 0 {
			base, _ := url.Parse(page.URL)
			attempts := 0
			for _, link := range page.Links {
				next, err := url.Parse(link.URL)
				if err != nil || !strings.EqualFold(next.Host, base.Host) || next.Scheme != base.Scheme || next.String() == base.String() || !musicNavigation(link) {
					continue
				}
				if err := ctx.Err(); err != nil {
					return result, err
				}
				attempts++
				section, err := w.Read(ctx, link.URL)
				if err := ctx.Err(); err != nil {
					return result, err
				}
				if err == nil {
					tracks, err = w.resolvePageMusic(ctx, section, &scriptBudget)
					if len(tracks) > 0 {
						break
					}
				}
				if err != nil && sectionErr == nil {
					sectionErr = err
				}
				if attempts >= 3 {
					break
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(tracks) == 0 {
		if sectionErr != nil {
			return result, sectionErr
		}
		return result, errors.New("no playable audio links found on the page; a JavaScript-only player may require a direct audio URL")
	}
	result.Tracks = tracks
	return result, nil
}

func (w *WebReader) resolvePageMusic(ctx context.Context, page WebResult, remaining *int) ([]WebLink, error) {
	if len(page.Audio) > 0 || !page.music.player {
		return page.Audio, nil
	}
	origin, _ := url.Parse(page.URL)
	base := page.music.base
	if base == nil {
		base = origin
	}
	var firstErr error
	for _, script := range page.music.scripts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if *remaining == 0 {
			break
		}
		*remaining -= 1
		body, err := w.readBody(ctx, script, origin)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		media := newMediaCollector(base)
		media.script(string(body.data))
		if len(media.audio) > 0 {
			return media.audio, nil
		}
	}
	return nil, firstErr
}
