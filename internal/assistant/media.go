package assistant

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"net/url"
	"path"
	"slices"
	"strings"

	"golang.org/x/net/html"
)

type WebLink struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

type mediaCollector struct {
	base  *url.URL
	links []WebLink
	audio []WebLink
	nodes int
}

func newMediaCollector(base *url.URL) *mediaCollector {
	return &mediaCollector{base: base.Clone()}
}

func mediaExtension(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	ext := strings.ToLower(path.Ext(u.Path))
	switch ext {
	case ".mp3", ".m4a", ".aac", ".ogg", ".opus", ".wav", ".flac", ".m3u8":
		return ext
	}
	return ""
}

func (c *mediaCollector) resolve(raw string) string {
	if raw == "" || len(raw) > 4096 || strings.HasPrefix(raw, "#") {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	u = c.base.ResolveReference(u)
	u.Fragment = ""
	if validateWebURL(u) != nil {
		return ""
	}
	return u.String()
}

func addWebLink(items *[]WebLink, raw, title string, limit int) {
	if raw == "" {
		return
	}
	title = strings.Join(strings.Fields(title), " ")
	if runes := []rune(title); len(runes) > 160 {
		title = string(runes[:160])
	}
	for i := range *items {
		if (*items)[i].URL == raw {
			if (*items)[i].Title == "" {
				(*items)[i].Title = title
			}
			return
		}
	}
	if len(*items) < limit {
		*items = append(*items, WebLink{URL: raw, Title: title})
	}
}

func (c *mediaCollector) addAudio(raw, title string, explicit bool) {
	if !explicit && mediaExtension(raw) == "" {
		return
	}
	addWebLink(&c.audio, c.resolve(raw), title, 64)
}

func (c *mediaCollector) walk(value any, depth int) {
	c.nodes++
	if depth > 32 || c.nodes > 16000 || len(c.audio) >= 64 {
		return
	}
	switch v := value.(type) {
	case map[string]any:
		title, _ := v["title"].(string)
		if title == "" {
			title, _ = v["name"].(string)
		}
		for _, key := range []string{"src", "url", "contentUrl", "audio", "audioUrl", "audio_url", "streamUrl", "downloadUrl"} {
			if raw, ok := v[key].(string); ok {
				c.addAudio(raw, title, false)
			}
		}
		for _, key := range slices.Sorted(maps.Keys(v)) {
			c.walk(v[key], depth+1)
		}
	case []any:
		for _, item := range v {
			c.walk(item, depth+1)
		}
	}
}

func (c *mediaCollector) json(data []byte) {
	var value any
	if json.Unmarshal(data, &value) == nil {
		c.walk(value, 0)
	}
}

func extractPageMedia(data []byte, base *url.URL) ([]WebLink, []WebLink, pageMusic) {
	c := newMediaCollector(base)
	var music pageMusic
	z := html.NewTokenizer(bytes.NewReader(data))
	var framework strings.Builder
	var script strings.Builder
	var anchor, anchorText string
	inScript := false
	inAudio := false
	finishAnchor := func() {
		if anchor != "" {
			addWebLink(&c.links, c.resolve(anchor), anchorText, 48)
			c.addAudio(anchor, anchorText, false)
		}
		anchor, anchorText = "", ""
	}
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			break
		}
		token := z.Token()
		switch kind {
		case html.StartTagToken, html.SelfClosingTagToken:
			attrs := map[string]string{}
			for _, a := range token.Attr {
				attrs[a.Key] = a.Val
			}
			switch token.Data {
			case "base":
				if raw := c.resolve(attrs["href"]); raw != "" {
					c.base, _ = url.Parse(raw)
				}
			case "a":
				finishAnchor()
				anchor, anchorText = attrs["href"], attrs["title"]
			case "audio":
				music.player = true
				inAudio = kind == html.StartTagToken
				c.addAudio(attrs["src"], attrs["title"], true)
			case "source":
				c.addAudio(attrs["src"], attrs["title"], inAudio || strings.HasPrefix(attrs["type"], "audio/"))
			case "meta":
				if strings.HasPrefix(attrs["property"], "og:audio") {
					c.addAudio(attrs["content"], "", false)
				}
			case "script":
				if raw := c.resolve(attrs["src"]); raw != "" && len(music.scripts) < 3 && !slices.Contains(music.scripts, raw) {
					u, _ := url.Parse(raw)
					if sameWebOrigin(base, u) {
						music.scripts = append(music.scripts, raw)
					}
				}
				inScript = true
				script.Reset()
			}
		case html.TextToken:
			if inScript {
				script.WriteString(token.Data)
			} else if anchor != "" {
				anchorText += " " + token.Data
			}
		case html.EndTagToken:
			if token.Data == "audio" {
				inAudio = false
			}
			if token.Data == "a" {
				finishAnchor()
			}
			if token.Data == "script" && inScript {
				inScript = false
				body := strings.TrimSpace(script.String())
				if strings.HasPrefix(body, "self.__next_f.push(") {
					// Decode data frames only; never evaluate the script.
					d := jsontext.NewDecoder(strings.NewReader(strings.TrimPrefix(body, "self.__next_f.push(")))
					if raw, err := d.ReadValue(); err == nil {
						var frame []any
						if json.Unmarshal(raw, &frame) == nil && len(frame) > 1 {
							if fragment, ok := frame[1].(string); ok {
								framework.WriteString(fragment)
							}
						}
					}
				} else {
					c.json([]byte(body))
					c.script(body)
				}
			}
		}
	}
	finishAnchor()
	for record := range strings.SplitSeq(framework.String(), "\n") {
		if _, payload, ok := strings.Cut(record, ":"); ok {
			c.json([]byte(payload))
		}
	}
	music.base = c.base.Clone()
	return c.links, c.audio, music
}
