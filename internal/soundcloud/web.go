package soundcloud

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var scriptPattern = regexp.MustCompile(`(?i)<script[^>]+src=["']([^"']+)["']`)
var clientIDPattern = regexp.MustCompile(`(?:client_id|clientId)\s*[:=]\s*["']([a-zA-Z0-9]{32})["']`)

func (c *Client) webID(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.clientID != "" && !force {
		return c.clientID, nil
	}
	remaining := 8
	seen := make(map[string]bool)
	// A successful homepage response can be empty. The public search page is
	// another ordinary entry point, using the same transport and bundle budget.
	for _, page := range []string{"https://soundcloud.com/", "https://soundcloud.com/search?q=music"} {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if remaining == 0 {
			break
		}
		b, _, err := c.request(ctx, "GET", page, nil, false)
		if err != nil {
			return "", err
		}
		scripts := scriptPattern.FindAllSubmatch(b, 64)
		base, _ := url.Parse(page)
		// Application bundles are normally last; fetch eight unique scripts total.
		for i := len(scripts) - 1; i >= 0 && remaining > 0; i-- {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			u, err := url.Parse(string(scripts[i][1]))
			if err != nil {
				continue
			}
			u = base.ResolveReference(u)
			if ValidateAssetURL(u) != nil || (!strings.HasSuffix(u.Hostname(), ".sndcdn.com") && u.Hostname() != "soundcloud.com") || seen[u.String()] {
				continue
			}
			seen[u.String()] = true
			remaining--
			js, _, err := c.request(ctx, "GET", u.String(), nil, false)
			if err != nil {
				if _, statusError := errors.AsType[*httpStatusError](err); !statusError {
					return "", err
				}
				continue
			}
			m := clientIDPattern.FindSubmatch(js)
			if len(m) > 1 {
				c.clientID = string(m[1])
				return c.clientID, nil
			}
		}
	}
	return "", errors.New("SoundCloud: не удалось определить публичный client ID")
}
