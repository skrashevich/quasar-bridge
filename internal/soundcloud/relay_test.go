package soundcloud

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type relayFixtureTransport struct{ target *url.URL }

// Memory writes cannot block; declare deadline support explicitly so fixtures
// exercise relay behavior without weakening the production fail-closed path.
type relayMemoryRecorder struct{ *httptest.ResponseRecorder }

func (*relayMemoryRecorder) SetWriteDeadline(time.Time) error { return nil }
func newRelayMemoryRecorder() *relayMemoryRecorder {
	return &relayMemoryRecorder{httptest.NewRecorder()}
}

func (f relayFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	c.URL = r.URL.Clone()
	c.URL.Scheme, c.URL.Host = f.target.Scheme, f.target.Host
	resp, err := http.DefaultTransport.RoundTrip(c)
	if resp != nil {
		resp.Request = r
	}
	return resp, err
}

func newRelayFixture(t *testing.T, handler http.HandlerFunc) *Relay {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	r, err := NewRelay("http://bridge.test:8080", &http.Client{Transport: relayFixtureTransport{u}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func relayGet(r *Relay, method, raw string) *relayMemoryRecorder {
	w := newRelayMemoryRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, raw, nil))
	return w
}

func relayLines(body string) []string {
	var out []string
	for line := range strings.SplitSeq(body, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func TestRelayNestedManifestsAndAttributes(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Authorization") != "" || q.Header.Get("Cookie") != "" {
			t.Error("credentials forwarded")
		}
		switch q.URL.Path {
		case "/root.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=128000\nlevels/main.m3u8\n")
		case "/levels/main.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"../key\"\n#EXT-X-MAP:URI=\"https://cf-hls-media.sndcdn.com/init\"\n#EXTINF:5,\n../segment.aac?secret=hidden\n")
		default:
			io.WriteString(w, "media")
		}
	})
	m, err := r.Register("", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(m.URL, ".m3u8") {
		t.Fatal(m)
	}
	w := relayGet(r, "GET", m.URL)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	nested := relayGet(r, "GET", relayLines(w.Body.String())[0])
	if nested.Code != 200 || strings.Contains(nested.Body.String(), "sndcdn") || strings.Contains(nested.Body.String(), "secret") {
		t.Fatal(nested.Code, nested.Body.String())
	}
	for _, part := range strings.Split(nested.Body.String(), "URI=\"")[1:] {
		u, _, _ := strings.Cut(part, "\"")
		if !strings.HasPrefix(u, "http://bridge.test:8080/soundcloud/") || relayGet(r, "GET", u).Body.String() != "media" {
			t.Fatal(u)
		}
	}
	if got := relayGet(r, "GET", relayLines(nested.Body.String())[0]); got.Body.String() != "media" {
		t.Fatal(got.Code, got.Body.String())
	}
}

func TestRelayHeadRangeAndArtwork(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/root.m3u8":
			io.WriteString(w, "#EXTM3U\nsegment.aac\n")
		case "/cover.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			io.WriteString(w, "cover")
		default:
			if q.Header.Get("Range") != "bytes=1-3" {
				t.Error("missing range")
			}
			w.Header().Set("Content-Type", "audio/aac")
			w.Header().Set("Content-Range", "bytes 1-3/8")
			w.WriteHeader(206)
			if q.Method != "HEAD" {
				io.WriteString(w, "bcd")
			}
		}
	})
	m, err := r.Register("https://i1.sndcdn.com/cover.jpg", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := relayGet(r, "GET", m.ArtworkURL); got.Body.String() != "cover" || got.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal(got)
	}
	manifest := relayGet(r, "GET", m.URL)
	segment := relayLines(manifest.Body.String())[0]
	for _, method := range []string{"GET", "HEAD"} {
		q := httptest.NewRequest(method, segment, nil)
		q.Header.Set("Range", "bytes=1-3")
		q.Header.Set("Authorization", "Bearer do-not-forward")
		w := newRelayMemoryRecorder()
		r.ServeHTTP(w, q)
		if w.Code != 206 || w.Header().Get("Content-Range") != "bytes 1-3/8" || (method == "HEAD" && w.Body.Len() != 0) {
			t.Fatal(w.Code, w.Header(), w.Body.String())
		}
	}
	if got := relayGet(r, "HEAD", m.URL); got.Code != 200 || got.Body.Len() != 0 {
		t.Fatal(got.Code, got.Body.String())
	}
}

func TestRelayRefreshesExpiredRootAndSegment(t *testing.T) {
	for _, expired := range []string{"root", "segment"} {
		t.Run(expired, func(t *testing.T) {
			var resolutions atomic.Int32
			r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
				if strings.Contains(q.URL.Path, "old") && (expired == "root" || strings.HasSuffix(q.URL.Path, ".aac")) {
					w.WriteHeader(403)
					return
				}
				if strings.HasSuffix(q.URL.Path, ".m3u8") {
					io.WriteString(w, "#EXTM3U\n"+strings.TrimSuffix(q.URL.Path, ".m3u8")+".aac\n")
					return
				}
				io.WriteString(w, "fresh")
			})
			m, _ := r.Register("", func(context.Context) (string, error) {
				if resolutions.Add(1) == 1 {
					return "https://cf-hls-media.sndcdn.com/old.m3u8", nil
				}
				return "https://cf-hls-media.sndcdn.com/new.m3u8", nil
			})
			w := relayGet(r, "GET", m.URL)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			w = relayGet(r, "GET", relayLines(w.Body.String())[0])
			if w.Code != 200 || w.Body.String() != "fresh" || resolutions.Load() != 2 {
				t.Fatal(w.Code, w.Body.String(), resolutions.Load())
			}
		})
	}
}

func TestRelayRejectsHostileURLsAndRedirects(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) { http.Redirect(w, q, "https://127.0.0.1/private", 302) })
	for _, raw := range []string{"http://i1.sndcdn.com/a", "https://127.0.0.1/a", "https://evil.test/a", "https://i1.sndcdn.com.evil.test/a", "https://user:pass@i1.sndcdn.com/a", "https://i1.sndcdn.com:8443/a"} {
		if _, err := r.Register(raw, func(context.Context) (string, error) { return raw, nil }); err == nil {
			t.Errorf("accepted %s", raw)
		}
		m, _ := r.Register("", func(context.Context) (string, error) { return raw, nil })
		if w := relayGet(r, "GET", m.URL); w.Code != 502 {
			t.Errorf("stream accepted %s: %d", raw, w.Code)
		}
	}
	m, _ := r.Register("https://i1.sndcdn.com/redirect", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	if got := relayGet(r, "GET", m.ArtworkURL); got.Code != 502 {
		t.Fatal(got.Code)
	}
	if got := relayGet(r, "GET", "http://bridge.test:8080/soundcloud/unknown?url=https://i1.sndcdn.com/a"); got.Code != 404 {
		t.Fatal(got.Code)
	}
}

func TestRelayBoundsStorageExpiryAndBodies(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		io.WriteString(w, strings.Repeat("x", relayMaxManifestBytes+1))
	})
	r.maxResources = 2
	m, err := r.Register("https://i1.sndcdn.com/a", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register("", func(context.Context) (string, error) { return "", nil }); err == nil {
		t.Fatal("resource limit not enforced")
	}
	if w := relayGet(r, "GET", m.URL); w.Code != 502 {
		t.Fatal(w.Code)
	}
	r.now = func() time.Time { return time.Now().Add(2 * relayResourceTTL) }
	if w := relayGet(r, "GET", m.URL); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if _, err := r.Register("", func(context.Context) (string, error) { return "", nil }); err != nil {
		t.Fatal("expired resources not pruned", err)
	}
}

func TestRelayCancellationReachesUpstream(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		if strings.HasSuffix(q.URL.Path, "m3u8") {
			io.WriteString(w, "#EXTM3U\nsegment.aac\n")
			return
		}
		w.Header().Set("Content-Type", "audio/aac")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(entered)
		<-q.Context().Done()
		close(canceled)
	})
	m, _ := r.Register("", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	segment := relayLines(relayGet(r, "GET", m.URL).Body.String())[0]
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	q := httptest.NewRequest("GET", segment, nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { r.ServeHTTP(newRelayMemoryRecorder(), q); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not entered")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not canceled")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay not canceled")
	}
}

func TestRelayCoalescesConcurrentManifestResolution(t *testing.T) {
	var resolves, manifests atomic.Int32
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		manifests.Add(1)
		time.Sleep(20 * time.Millisecond)
		io.WriteString(w, "#EXTM3U\nsegment.aac\n")
	})
	m, _ := r.Register("", func(context.Context) (string, error) {
		resolves.Add(1)
		return "https://cf-hls-media.sndcdn.com/root.m3u8", nil
	})
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if w := relayGet(r, "GET", m.URL); w.Code != 200 {
				t.Errorf("status %d", w.Code)
			}
		})
	}
	wg.Wait()
	if resolves.Load() != 1 || manifests.Load() != 1 {
		t.Fatal(resolves.Load(), manifests.Load())
	}
}

func TestRelayExpiredNestedKeyRebuildsAncestors(t *testing.T) {
	var resolves atomic.Int32
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		switch {
		case strings.HasSuffix(q.URL.Path, "root.m3u8"):
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nlevel.m3u8\n")
		case strings.HasSuffix(q.URL.Path, "level.m3u8"):
			io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\nsegment.aac\n")
		case strings.Contains(q.URL.Path, "/old/"):
			w.WriteHeader(403)
		default:
			io.WriteString(w, "new key")
		}
	})
	m, _ := r.Register("", func(context.Context) (string, error) {
		if resolves.Add(1) == 1 {
			return "https://cf-hls-media.sndcdn.com/old/root.m3u8", nil
		}
		return "https://cf-hls-media.sndcdn.com/new/root.m3u8", nil
	})
	root := relayGet(r, "GET", m.URL)
	nested := relayGet(r, "GET", relayLines(root.Body.String())[0])
	_, key, _ := strings.Cut(nested.Body.String(), "URI=\"")
	key, _, _ = strings.Cut(key, "\"")
	w := relayGet(r, "GET", key)
	if w.Code != 200 || w.Body.String() != "new key" || resolves.Load() != 2 {
		t.Fatal(w.Code, w.Body.String(), resolves.Load())
	}
}

func TestRelayNeverLoopsOnForbiddenManifest(t *testing.T) {
	var resolves, requests atomic.Int32
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) { requests.Add(1); w.WriteHeader(403) })
	m, _ := r.Register("", func(context.Context) (string, error) {
		resolves.Add(1)
		return "https://cf-hls-media.sndcdn.com/root.m3u8?private=secret", nil
	})
	w := relayGet(r, "GET", m.URL)
	if w.Code != 502 || resolves.Load() != 2 || requests.Load() != 2 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Code, w.Body.String(), resolves.Load(), requests.Load())
	}
}

func TestRelayBoundsImagesAndRejectsHostileManifestReferences(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/cover" {
			io.WriteString(w, strings.Repeat("a", relayMaxArtworkBytes+1))
			return
		}
		io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://localhost/private\"\nsegment.aac\n")
	})
	m, _ := r.Register("https://i1.sndcdn.com/cover", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	for _, raw := range []string{m.URL, m.ArtworkURL} {
		if w := relayGet(r, "GET", raw); w.Code != 502 {
			t.Fatal(w.Code)
		}
	}
}

func TestRelayFollowsAllowedRedirectAndRelativeBase(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/root.m3u8":
			http.Redirect(w, q, "https://cf-hls-media.sndcdn.com/nested/final.m3u8", 302)
		case "/nested/final.m3u8":
			io.WriteString(w, "#EXTM3U\nsegment.aac\n")
		case "/nested/segment.aac":
			io.WriteString(w, "correct base")
		default:
			http.NotFound(w, q)
		}
	})
	m, _ := r.Register("", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	w := relayGet(r, "GET", m.URL)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = relayGet(r, "GET", relayLines(w.Body.String())[0])
	if w.Body.String() != "correct base" {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestRelayCoalescesConcurrentExpiredSegments(t *testing.T) {
	var resolves atomic.Int32
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		if strings.HasSuffix(q.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\nsegment.aac\n")
			return
		}
		if strings.Contains(q.URL.Path, "/old/") {
			w.WriteHeader(403)
			return
		}
		io.WriteString(w, "fresh")
	})
	m, _ := r.Register("", func(context.Context) (string, error) {
		if resolves.Add(1) == 1 {
			return "https://cf-hls-media.sndcdn.com/old/root.m3u8", nil
		}
		return "https://cf-hls-media.sndcdn.com/new/root.m3u8", nil
	})
	segment := relayLines(relayGet(r, "GET", m.URL).Body.String())[0]
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if w := relayGet(r, "GET", segment); w.Code != 200 || w.Body.String() != "fresh" {
				t.Errorf("status %d body %q", w.Code, w.Body.String())
			}
		})
	}
	wg.Wait()
	if resolves.Load() != 2 {
		t.Fatal("duplicate root refresh", resolves.Load())
	}
}

func TestRelayImplicitByteRangesKeepSharedResourceIdentity(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		if strings.HasSuffix(q.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-TARGETDURATION:5\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:5,\n#EXT-X-BYTERANGE:4@0\nwhole.aac\n#EXTINF:5,\n#EXT-X-BYTERANGE:4\nwhole.aac\n#EXT-X-ENDLIST\n")
			return
		}
		w.Header().Set("Content-Type", "audio/aac")
		switch q.Header.Get("Range") {
		case "bytes=0-3":
			w.Header().Set("Content-Range", "bytes 0-3/8")
			w.WriteHeader(206)
			io.WriteString(w, "abcd")
		case "bytes=4-7":
			w.Header().Set("Content-Range", "bytes 4-7/8")
			w.WriteHeader(206)
			io.WriteString(w, "efgh")
		default:
			t.Errorf("unexpected range %q", q.Header.Get("Range"))
			w.WriteHeader(416)
		}
	})
	m, _ := r.Register("", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	w := relayGet(r, "GET", m.URL)
	lines := relayLines(w.Body.String())
	if len(lines) != 2 || lines[0] != lines[1] {
		t.Fatalf("implicit BYTERANGE must retain one media resource: %v", lines)
	}
	if !strings.Contains(w.Body.String(), "#EXT-X-BYTERANGE:4\n") {
		t.Fatal("implicit range modified", w.Body.String())
	}
	for i, byteRange := range []string{"bytes=0-3", "bytes=4-7"} {
		q := httptest.NewRequest("GET", lines[i], nil)
		q.Header.Set("Range", byteRange)
		got := newRelayMemoryRecorder()
		r.ServeHTTP(got, q)
		if got.Code != 206 || got.Body.String() != []string{"abcd", "efgh"}[i] {
			t.Fatal(got.Code, got.Body.String())
		}
	}
}

type relayPipeListener struct {
	conn chan net.Conn
	done chan struct{}
	once sync.Once
	addr net.Addr
}

func (l *relayPipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conn:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *relayPipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *relayPipeListener) Addr() net.Addr { return l.addr }

type relayObservedWriter struct {
	http.ResponseWriter
	once    sync.Once
	writing chan struct{}
}

func (w *relayObservedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.writing) })
	return w.ResponseWriter.Write(p)
}
func (w *relayObservedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestRelayCanceledSlowDownstreamUnblocksHandler(t *testing.T) {
	testRelaySlowDownstream(t, true)
}

func TestRelayWriteDeadlineUnblocksSlowDownstream(t *testing.T) {
	testRelaySlowDownstream(t, false)
}

func testRelaySlowDownstream(t *testing.T, cancelRequest bool) {
	t.Helper()
	upstreamDone := make(chan struct{})
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		if strings.HasSuffix(q.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\nsegment.aac\n")
			return
		}
		defer close(upstreamDone)
		chunk := strings.Repeat("a", 32<<10)
		for {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			select {
			case <-q.Context().Done():
				return
			default:
			}
		}
	})
	if !cancelRequest {
		r.writeTimeout = 75 * time.Millisecond
	}
	m, _ := r.Register("", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	segment := relayLines(relayGet(r, "GET", m.URL).Body.String())[0]
	u, _ := url.Parse(segment)
	clientConn, serverConn := net.Pipe()
	l := &relayPipeListener{conn: make(chan net.Conn, 1), done: make(chan struct{}), addr: serverConn.LocalAddr()}
	l.conn <- serverConn
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writing, finished := make(chan struct{}), make(chan struct{})
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		defer close(finished)
		r.ServeHTTP(&relayObservedWriter{ResponseWriter: w, writing: writing}, q.WithContext(ctx))
	})}
	go s.Serve(l)
	t.Cleanup(func() { clientConn.Close(); s.Close() })
	if _, err := io.WriteString(clientConn, "GET "+u.Path+" HTTP/1.1\r\nHost: bridge.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// Keep the client connection open without reading any bytes. net.Pipe has no
	// socket buffer, so the actual HTTP server's response write remains blocked.
	select {
	case <-writing:
	case <-time.After(3 * time.Second):
		t.Fatal("downstream write not entered")
	}
	if cancelRequest {
		cancel()
	}
	select {
	case <-finished:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("slow downstream pinned relay handler")
	}
	select {
	case <-upstreamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not released")
	}
}

func TestRelayUnsupportedWriteDeadlineFailsWithoutUpstreamOrBody(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) { t.Error("unexpected upstream request") })
	m, _ := r.Register("", func(context.Context) (string, error) { t.Error("unexpected stream resolution"); return "", nil })
	w := httptest.NewRecorder() // Deliberately no deadline interface.
	r.ServeHTTP(w, httptest.NewRequest("GET", m.URL, nil))
	if w.Code != 503 || w.Body.Len() != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestRelayRepeatedByteRangeResourceRemainsSharedAfterRefresh(t *testing.T) {
	var resolves atomic.Int32
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		if strings.HasSuffix(q.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXT-X-BYTERANGE:4@0\nwhole.aac\n#EXT-X-BYTERANGE:4\nwhole.aac\n")
			return
		}
		if strings.Contains(q.URL.Path, "/old/") {
			w.WriteHeader(403)
			return
		}
		io.WriteString(w, "new")
	})
	m, _ := r.Register("", func(context.Context) (string, error) {
		if resolves.Add(1) == 1 {
			return "https://cf-hls-media.sndcdn.com/old/root.m3u8", nil
		}
		return "https://cf-hls-media.sndcdn.com/new/root.m3u8", nil
	})
	before := relayLines(relayGet(r, "GET", m.URL).Body.String())
	if len(before) != 2 || before[0] != before[1] {
		t.Fatal(before)
	}
	if w := relayGet(r, "GET", before[0]); w.Code != 200 || w.Body.String() != "new" {
		t.Fatal(w.Code, w.Body.String())
	}
	after := relayLines(relayGet(r, "GET", m.URL).Body.String())
	if len(after) != 2 || after[0] != after[1] || after[0] != before[0] || resolves.Load() != 2 {
		t.Fatal(before, after, resolves.Load())
	}
}

func relaySimulatedClock(r *Relay) func(time.Duration) {
	start := time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()) }
	return func(elapsed time.Duration) { clock.Store(start.Add(elapsed).UnixNano()) }
}

func TestRelayActivePlaybackRenewsIdleLeasePastTwoHours(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		if strings.HasSuffix(q.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\nsegment.aac\n")
			return
		}
		io.WriteString(w, "audio")
	})
	at := relaySimulatedClock(r)
	m, _ := r.Register("https://i1.sndcdn.com/cover", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	segment := relayLines(relayGet(r, "GET", m.URL).Body.String())[0]
	for _, elapsed := range []time.Duration{90 * time.Minute, 150 * time.Minute, 210 * time.Minute} {
		at(elapsed)
		if w := relayGet(r, "GET", segment); w.Code != 200 || w.Body.String() != "audio" {
			t.Fatalf("active playback at %v: status %d body %q", elapsed, w.Code, w.Body.String())
		}
	}
	// Lease is shared by all resources of the same registered track.
	if w := relayGet(r, "GET", m.ArtworkURL); w.Code != 200 {
		t.Fatal(w.Code)
	}
	at(210*time.Minute + relayResourceTTL)
	if w := relayGet(r, "GET", segment); w.Code != 404 {
		t.Fatal("idle registration survived two hours", w.Code)
	}
}

func TestRelayActiveLeaseStillHasAbsoluteLifetimeBound(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) { io.WriteString(w, "#EXTM3U\n#EXT-X-ENDLIST\n") })
	at := relaySimulatedClock(r)
	m, _ := r.Register("", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	for hour := range 26 {
		at(time.Duration(hour) * time.Hour)
		if w := relayGet(r, "GET", m.URL); w.Code != 200 {
			t.Fatalf("active lease at hour %d: %d", hour, w.Code)
		}
	}
	at(26 * time.Hour)
	if w := relayGet(r, "GET", m.URL); w.Code != 404 {
		t.Fatal("absolute lifetime not enforced", w.Code)
	}
	r.maxResources = 1
	if _, err := r.Register("", func(context.Context) (string, error) { return "", nil }); err != nil {
		t.Fatal("absolutely expired storage not pruned", err)
	}
}

func TestRelayInvalidQueryCannotRenewIdleLease(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) { t.Error("unexpected upstream request") })
	at := relaySimulatedClock(r)
	m, _ := r.Register("", func(context.Context) (string, error) { t.Error("unexpected resolution"); return "", nil })
	at(90 * time.Minute)
	if w := relayGet(r, "GET", m.URL+"?url=ignored"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	at(relayResourceTTL)
	if w := relayGet(r, "GET", m.URL); w.Code != 404 {
		t.Fatal("invalid request renewed lease", w.Code)
	}
}

func TestRelayInProgressSegmentRenewsSiblingLease(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/root.m3u8":
			io.WriteString(w, "#EXTM3U\nsegment.aac\n")
		case "/cover":
			io.WriteString(w, "cover")
		default:
			close(entered)
			select {
			case <-release:
				io.WriteString(w, "audio")
			case <-q.Context().Done():
			}
		}
	})
	at := relaySimulatedClock(r)
	m, _ := r.Register("https://i1.sndcdn.com/cover", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	segment := relayLines(relayGet(r, "GET", m.URL).Body.String())[0]
	at(90 * time.Minute)
	done := make(chan int, 1)
	go func() { done <- relayGet(r, "GET", segment).Code }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("segment not entered")
	}
	at(150 * time.Minute)
	if w := relayGet(r, "GET", m.ArtworkURL); w.Code != 200 || w.Body.String() != "cover" {
		t.Fatal("in-progress playback lost sibling lease", w.Code, w.Body.String())
	}
	unblock()
	select {
	case code := <-done:
		if code != 200 {
			t.Fatal(code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("segment not completed")
	}
}

func TestRelayNativeVSIDTelemetryKeepsOpaqueResourceIdentity(t *testing.T) {
	var requests, resolves atomic.Int32
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) {
		requests.Add(1)
		if q.URL.RawQuery != "signed=retained" {
			t.Errorf("native telemetry changed upstream query: %q", q.URL.RawQuery)
		}
		io.WriteString(w, "#EXTM3U\nsegment.aac\n")
	})
	m, _ := r.Register("", func(context.Context) (string, error) {
		resolves.Add(1)
		return "https://cf-hls-media.sndcdn.com/root.m3u8?signed=retained", nil
	})
	first := relayGet(r, "GET", m.URL+"?vsid=native%2Fopaque%2Btoken%3D")
	if first.Code != 200 {
		t.Fatal("native vsid rejected", first.Code, first.Body.String())
	}
	for _, query := range []string{"", "?vsid=another-token", "?%76sid=percent-encoded-key", "?vsid=" + strings.Repeat("a", 512), "?%76%73%69%64=" + strings.Repeat("%41", 512)} {
		w := relayGet(r, "GET", m.URL+query)
		if w.Code != 200 || w.Body.String() != first.Body.String() {
			t.Fatal(query, w.Code, w.Body.String())
		}
	}
	if w := relayGet(r, "HEAD", m.URL+"?vsid=head-token"); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	if requests.Load() != 1 || resolves.Load() != 1 {
		t.Fatal("telemetry changed resource identity", requests.Load(), resolves.Load())
	}
}

func TestRelayRejectsUnsafeNativeTelemetryQueries(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) { t.Error("unsafe query reached upstream") })
	m, _ := r.Register("", func(context.Context) (string, error) { t.Error("unsafe query invoked resolver"); return "", nil })
	for _, query := range []string{
		"url=https%3A%2F%2Fi1.sndcdn.com%2Fa", "unknown=token", "vsid=a&url=ignored", "vsid=a&vsid=b", "vsid=a&%76sid=b",
		"vsid=", "vsid", "vsid=%ZZ", "vsid=%", "vsid=a;other=b", "vsid=a&", "vsid=%00", "vsid=%0A", "vsid=%09",
		"vsid=" + strings.Repeat("a", 513), "vsid=" + strings.Repeat("%41", 513),
	} {
		t.Run(query, func(t *testing.T) {
			if w := relayGet(r, "GET", m.URL+"?"+query); w.Code != 404 {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestRelayNativeVSIDTelemetryRenewsIdleLease(t *testing.T) {
	r := newRelayFixture(t, func(w http.ResponseWriter, q *http.Request) { io.WriteString(w, "#EXTM3U\n#EXT-X-ENDLIST\n") })
	at := relaySimulatedClock(r)
	m, _ := r.Register("", func(context.Context) (string, error) { return "https://cf-hls-media.sndcdn.com/root.m3u8", nil })
	at(90 * time.Minute)
	if w := relayGet(r, "GET", m.URL+"?vsid=native-token"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	at(150 * time.Minute)
	if w := relayGet(r, "GET", m.URL); w.Code != 200 {
		t.Fatal("native request failed to renew lease", w.Code)
	}
}
