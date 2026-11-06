package soundcloud

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	relayMaxManifestBytes = 1 << 20
	relayMaxArtworkBytes  = 8 << 20
	relayResourceTTL      = 2 * time.Hour
	relayMaxLifetime      = 26 * time.Hour // Covers a 24-hour track plus two hours of grace.
	relayRequestTimeout   = 35 * time.Second
	relayMaxVSIDBytes     = 512
)

// Media contains opaque URLs suitable for delivery to a speaker.
type Media struct{ URL, ArtworkURL string }

// Relay serves registered HLS resources and artwork without exposing signed URLs.
// Registrations have an idle lease and bounded lifetime; no audio data is retained.
type Relay struct {
	base         string
	client       *http.Client
	mu           sync.Mutex
	resources    map[string]*relayResource
	maxResources int
	now          func() time.Time
	writeTimeout time.Duration
}

type relayGroup struct {
	mu         sync.Mutex // Serializes manifest resolution/rewrite and signed URL refresh.
	resolve    func(context.Context) (string, error)
	root       *relayResource
	generation uint64
	// Both lease timestamps are protected by Relay.mu, including during pruning.
	expires, absoluteExpires time.Time
}

type relayResource struct {
	path                      string
	group                     *relayGroup
	parent                    *relayResource
	upstream                  string
	manifest, artwork, active bool
	generation                uint64
	children                  map[int]*relayResource
	cache                     []byte
	cacheUntil                time.Time
}

func NewRelay(baseURL string, client *http.Client) (*Relay, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("soundcloud: invalid relay base URL")
	}
	if client == nil {
		client = &http.Client{}
	}
	c := *client
	c.Jar = nil
	if c.Timeout <= 0 || c.Timeout > relayRequestTimeout {
		c.Timeout = relayRequestTimeout
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("soundcloud: too many media redirects")
		}
		if err := ValidateAssetURL(req.URL); err != nil {
			return errors.New("soundcloud: forbidden media redirect")
		}
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		req.Header.Del("Proxy-Authorization")
		return nil
	}
	return &Relay{base: strings.TrimRight(u.String(), "/"), client: &c, resources: make(map[string]*relayResource), maxResources: 4096, now: time.Now, writeTimeout: relayRequestTimeout}, nil
}

func (r *Relay) Register(artworkURL string, resolve func(context.Context) (string, error)) (Media, error) {
	if resolve == nil {
		return Media{}, errors.New("soundcloud: missing stream resolver")
	}
	if artworkURL != "" {
		u, err := url.Parse(artworkURL)
		if err != nil || ValidateAssetURL(u) != nil {
			return Media{}, errors.New("soundcloud: forbidden artwork URL")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	count := 1
	if artworkURL != "" {
		count++
	}
	if len(r.resources)+count > r.maxResources {
		return Media{}, errors.New("soundcloud: relay resource limit reached")
	}
	now := r.now()
	g := &relayGroup{resolve: resolve, expires: now.Add(relayResourceTTL), absoluteExpires: now.Add(relayMaxLifetime)}
	root := r.addLocked(g, nil, "", true, false)
	g.root = root
	m := Media{URL: r.base + root.path}
	if artworkURL != "" {
		art := r.addLocked(g, nil, artworkURL, false, true)
		m.ArtworkURL = r.base + art.path
	}
	return m, nil
}

func (r *Relay) pruneLocked() {
	now := r.now()
	for path, res := range r.resources {
		if !now.Before(res.group.expires) {
			delete(r.resources, path)
		}
	}
}

func (r *Relay) addLocked(g *relayGroup, parent *relayResource, upstream string, manifest, artwork bool) *relayResource {
	path := "/soundcloud/" + rand.Text()
	if manifest {
		path += ".m3u8"
	}
	res := &relayResource{path: path, group: g, parent: parent, upstream: upstream, manifest: manifest, artwork: artwork, active: true, generation: g.generation, children: make(map[int]*relayResource)}
	r.resources[path] = res
	return res
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(r.writeTimeout)); err != nil {
		// An arbitrary writer without deadlines cannot safely be interrupted:
		// do not start upstream work or hand a body to an unbounded Write.
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if req.Method != "GET" && req.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", 405)
		return
	}
	allowedQuery := relayAllowedQuery(req.URL.RawQuery)
	r.mu.Lock()
	res := r.resources[req.URL.Path]
	now := r.now()
	if res != nil && !now.Before(res.group.expires) {
		delete(r.resources, req.URL.Path)
		res = nil
	}
	if res != nil && allowedQuery {
		// Renew before upstream work so legitimate in-progress playback keeps
		// sibling resources alive, while never extending the absolute lifetime.
		lease := now.Add(relayResourceTTL)
		if lease.After(res.group.absoluteExpires) {
			lease = res.group.absoluteExpires
		}
		res.group.expires = lease
	}
	r.mu.Unlock()
	if res == nil || !allowedQuery {
		http.NotFound(w, req)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), relayRequestTimeout)
	defer cancel()
	deadlineDone := make(chan struct{})
	stopDeadline := context.AfterFunc(ctx, func() {
		_ = controller.SetWriteDeadline(time.Now())
		close(deadlineDone)
	})
	defer func() {
		if !stopDeadline() {
			<-deadlineDone
		}
	}()
	g := res.group
	g.mu.Lock()
	if err := r.ensureResource(ctx, res); err != nil {
		g.mu.Unlock()
		r.fail(w)
		return
	}
	if res.manifest {
		body, err := r.manifest(ctx, res, true)
		g.mu.Unlock()
		if err != nil {
			r.fail(w)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Cache-Control", "no-store")
		if req.Method != "HEAD" {
			_, _ = w.Write(body)
		}
		return
	}
	upstream, generation := res.upstream, g.generation
	g.mu.Unlock()
	resp, err := r.fetch(ctx, req.Method, upstream, req.Header.Get("Range"))
	if err == nil && (resp.StatusCode == 401 || resp.StatusCode == 403) && !res.artwork {
		resp.Body.Close()
		g.mu.Lock()
		err = r.refresh(ctx, res, generation)
		upstream = res.upstream
		g.mu.Unlock()
		if err == nil {
			resp, err = r.fetch(ctx, req.Method, upstream, req.Header.Get("Range"))
		}
	}
	if err != nil {
		r.fail(w)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 206 && resp.StatusCode != 416 {
		r.fail(w)
		return
	}
	if res.artwork && req.Method != "HEAD" && resp.StatusCode != 416 {
		body, err := relayReadBounded(resp.Body, relayMaxArtworkBytes)
		if err != nil {
			r.fail(w)
			return
		}
		relayCopyHeaders(w.Header(), resp.Header)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}
	relayCopyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if req.Method != "HEAD" {
		_, _ = io.Copy(w, resp.Body)
	}
}

// Native speakers append a vsid telemetry token to HLS requests. It does not
// select or alter an upstream resource and is never copied into media requests.
func relayAllowedQuery(raw string) bool {
	if raw == "" {
		return true
	}
	// Bound parsing work even when every byte of the value/key is escaped.
	if len(raw) > 3*relayMaxVSIDBytes+16 || strings.Contains(raw, "&") {
		return false
	}
	values, err := url.ParseQuery(raw)
	if err != nil || len(values) != 1 || len(values["vsid"]) != 1 {
		return false
	}
	value := values["vsid"][0]
	if value == "" || len(value) > relayMaxVSIDBytes {
		return false
	}
	for i := range len(value) {
		if value[i] < 0x20 || value[i] == 0x7f {
			return false
		}
	}
	return true
}

func (r *Relay) fail(w http.ResponseWriter) {
	http.Error(w, "soundcloud media unavailable", http.StatusBadGateway)
}

// ensureResource rebuilds the ancestor chain after another request refreshed it.
// The caller holds group.mu. Children are identified by their stable URI position.
func (r *Relay) ensureResource(ctx context.Context, res *relayResource) error {
	g := res.group
	if res == g.root && res.upstream == "" {
		u, err := g.resolve(ctx)
		if err != nil {
			return errors.New("soundcloud: stream resolution failed")
		}
		parsed, err := url.Parse(u)
		if err != nil || ValidateAssetURL(parsed) != nil {
			return errors.New("soundcloud: forbidden stream URL")
		}
		res.upstream = u
		res.generation = g.generation
	}
	if res.parent != nil && res.generation != g.generation {
		if err := r.ensureResource(ctx, res.parent); err != nil {
			return err
		}
		if _, err := r.manifest(ctx, res.parent, false); err != nil {
			return err
		}
	}
	if !res.active || res.upstream == "" || (!res.artwork && res.generation != g.generation) {
		return errors.New("soundcloud: expired media resource")
	}
	return nil
}

func (r *Relay) refresh(ctx context.Context, res *relayResource, generation uint64) error {
	g := res.group
	if g.generation == generation {
		g.generation++
		g.root.upstream = ""
		g.root.cache = nil
		g.root.cacheUntil = time.Time{}
	}
	if err := r.ensureResource(ctx, g.root); err != nil {
		return err
	}
	if res == g.root {
		return nil
	}
	return r.ensureResource(ctx, res)
}

func (r *Relay) manifest(ctx context.Context, res *relayResource, retry bool) ([]byte, error) {
	if len(res.cache) > 0 && r.now().Before(res.cacheUntil) && res.generation == res.group.generation {
		return res.cache, nil
	}
	resp, err := r.fetch(ctx, "GET", res.upstream, "")
	if err != nil {
		return nil, err
	}
	if (resp.StatusCode == 401 || resp.StatusCode == 403) && retry {
		resp.Body.Close()
		if err := r.refresh(ctx, res, res.group.generation); err != nil {
			return nil, err
		}
		return r.manifest(ctx, res, false)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("soundcloud: manifest unavailable")
	}
	body, err := relayReadBounded(resp.Body, relayMaxManifestBytes)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "#EXTM3U") {
		return nil, errors.New("soundcloud: invalid HLS manifest")
	}
	// The final redirected URL is the base for relative HLS references.
	base, err := url.Parse(res.upstream)
	if err != nil {
		return nil, err
	}
	if resp.Request != nil {
		base = resp.Request.URL
	}
	body, err = r.rewrite(res, base, string(body))
	if err != nil {
		return nil, err
	}
	res.cache = body
	res.cacheUntil = r.now().Add(time.Second)
	return body, nil
}

var relayURIAttribute = regexp.MustCompile(`URI="([^"]*)"`)

func (r *Relay) rewrite(parent *relayResource, base *url.URL, body string) ([]byte, error) {
	for _, child := range parent.children {
		child.active = false
	}
	position := 0
	// Repeated URI occurrences must name the same media resource, including
	// implicit EXT-X-BYTERANGE offsets. Keep each occurrence's refresh slot.
	seen := make(map[string]*relayResource)
	used := make(map[*relayResource]bool)
	register := func(raw string, manifest bool) (string, error) {
		u, err := url.Parse(raw)
		if err != nil {
			return "", errors.New("soundcloud: invalid HLS reference")
		}
		u = base.ResolveReference(u)
		if ValidateAssetURL(u) != nil {
			return "", errors.New("soundcloud: forbidden HLS reference")
		}
		key := u.String()
		if manifest {
			key += "\x00manifest"
		}
		if child := seen[key]; child != nil {
			parent.children[position] = child
			position++
			return r.base + child.path, nil
		}
		child := parent.children[position]
		if used[child] {
			child = nil
		}
		if child == nil {
			r.mu.Lock()
			r.pruneLocked()
			if len(r.resources) >= r.maxResources {
				r.mu.Unlock()
				return "", errors.New("soundcloud: relay resource limit reached")
			}
			child = r.addLocked(parent.group, parent, u.String(), manifest, false)
			r.mu.Unlock()
		}
		parent.children[position] = child
		child.upstream = u.String()
		child.manifest = manifest
		child.active = true
		child.generation = parent.group.generation
		child.cache = nil
		seen[key] = child
		used[child] = true
		position++
		return r.base + child.path, nil
	}
	var out strings.Builder
	nextManifest := false
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			u, err := register(trimmed, nextManifest || strings.HasSuffix(strings.ToLower(strings.SplitN(trimmed, "?", 2)[0]), ".m3u8"))
			if err != nil {
				return nil, err
			}
			line = u
			nextManifest = false
		} else if strings.HasPrefix(trimmed, "#") {
			if strings.HasPrefix(trimmed, "#EXT-X-STREAM-INF:") {
				nextManifest = true
			}
			manifest := strings.HasPrefix(trimmed, "#EXT-X-MEDIA:") || strings.HasPrefix(trimmed, "#EXT-X-I-FRAME-STREAM-INF:") || strings.HasPrefix(trimmed, "#EXT-X-RENDITION-REPORT:")
			matches := relayURIAttribute.FindAllStringSubmatchIndex(line, -1)
			var changed strings.Builder
			start := 0
			for _, match := range matches {
				u, err := register(line[match[2]:match[3]], manifest)
				if err != nil {
					return nil, err
				}
				changed.WriteString(line[start:match[2]])
				changed.WriteString(u)
				start = match[3]
			}
			if len(matches) > 0 {
				changed.WriteString(line[start:])
				line = changed.String()
			}
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(line)
		if out.Len() > 4*relayMaxManifestBytes {
			return nil, errors.New("soundcloud: rewritten manifest too large")
		}
	}
	return []byte(out.String()), nil
}

func (r *Relay) fetch(ctx context.Context, method, raw, byteRange string) (*http.Response, error) {
	u, err := url.Parse(raw)
	if err != nil || ValidateAssetURL(u) != nil {
		return nil, errors.New("soundcloud: forbidden media URL")
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, errors.New("soundcloud: invalid media request")
	}
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, errors.New("soundcloud: media request failed")
	}
	return resp, nil
}

func relayReadBounded(body io.Reader, limit int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, errors.New("soundcloud: media read failed")
	}
	if len(b) > limit {
		return nil, fmt.Errorf("soundcloud: media exceeds %d byte limit", limit)
	}
	return b, nil
}

func relayCopyHeaders(dst, src http.Header) {
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
	dst.Set("Cache-Control", "no-store")
}
