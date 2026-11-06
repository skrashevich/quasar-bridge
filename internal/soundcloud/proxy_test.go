package soundcloud

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
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

type socksRequest struct {
	host, user, password string
	port                 uint16
	addressType          byte
}

type socksFixture struct {
	listener               net.Listener
	requests               chan socksRequest
	user, password, target string
	stall, reject          bool
	mu                     sync.Mutex
	connections            map[net.Conn]bool
	wg                     sync.WaitGroup
}

func newSOCKSFixture(t *testing.T, target, user, password string, stall, reject bool) *socksFixture {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &socksFixture{listener: l, requests: make(chan socksRequest, 32), target: target, user: user, password: password, stall: stall, reject: reject, connections: make(map[net.Conn]bool)}
	t.Cleanup(func() {
		l.Close()
		f.mu.Lock()
		for c := range f.connections {
			c.Close()
		}
		f.mu.Unlock()
		f.wg.Wait()
	})
	f.wg.Go(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.connections[c] = true
			f.mu.Unlock()
			f.wg.Go(func() {
				defer c.Close()
				defer func() { f.mu.Lock(); delete(f.connections, c); f.mu.Unlock() }()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				f.serve(c)
			})
		}
	})
	return f
}

func (f *socksFixture) proxyURL(scheme string) string {
	u := &url.URL{Scheme: scheme, Host: f.listener.Addr().String()}
	if f.user != "" {
		u.User = url.UserPassword(f.user, f.password)
	}
	return u.String()
}

func (f *socksFixture) serve(c net.Conn) {
	var header [2]byte
	if _, err := io.ReadFull(c, header[:]); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, header[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	method := byte(0)
	if f.user != "" {
		method = 2
	}
	if _, err := c.Write([]byte{5, method}); err != nil {
		return
	}
	request := socksRequest{}
	if method == 2 {
		if _, err := io.ReadFull(c, header[:]); err != nil || header[0] != 1 {
			return
		}
		user := make([]byte, header[1])
		if _, err := io.ReadFull(c, user); err != nil {
			return
		}
		if _, err := io.ReadFull(c, header[:1]); err != nil {
			return
		}
		password := make([]byte, header[0])
		if _, err := io.ReadFull(c, password); err != nil {
			return
		}
		request.user, request.password = string(user), string(password)
		if request.user != f.user || request.password != f.password {
			c.Write([]byte{1, 1})
			return
		}
		if _, err := c.Write([]byte{1, 0}); err != nil {
			return
		}
	}
	var connect [4]byte
	if _, err := io.ReadFull(c, connect[:]); err != nil || connect[0] != 5 || connect[1] != 1 {
		return
	}
	request.addressType = connect[3]
	length := 0
	switch connect[3] {
	case 1:
		length = 4
	case 4:
		length = 16
	case 3:
		if _, err := io.ReadFull(c, header[:1]); err != nil {
			return
		}
		length = int(header[0])
	default:
		return
	}
	host := make([]byte, length)
	if _, err := io.ReadFull(c, host); err != nil {
		return
	}
	if _, err := io.ReadFull(c, header[:]); err != nil {
		return
	}
	request.port = binary.BigEndian.Uint16(header[:])
	request.host = string(host)
	if connect[3] != 3 {
		request.host = net.IP(host).String()
	}
	f.requests <- request
	if f.stall {
		io.Copy(io.Discard, c)
		return
	}
	if f.reject {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	upstream, err := net.DialTimeout("tcp", f.target, time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer upstream.Close()
		io.Copy(upstream, c)
	}()
	io.Copy(c, upstream)
	c.Close()
	<-done
}

func proxyTLSFixture(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"music.invalid", "soundcloud.com", "*.soundcloud.com", "*.sndcdn.com"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, roots
}

func proxyClient(t *testing.T, proxyURL string, roots *x509.CertPool) *http.Client {
	t.Helper()
	client, err := NewHTTPClient(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	if roots != nil {
		transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	}
	t.Cleanup(transport.CloseIdleConnections)
	return client
}

func TestSOCKSProxyValidation(t *testing.T) {
	for _, proxyURL := range []string{"", "socks5://proxy.example:1080", "socks5h://user:p%40ss@[::1]:1080"} {
		if _, err := NewHTTPClient(proxyURL); err != nil {
			t.Errorf("valid proxy rejected: %v", err)
		}
	}
	for _, proxyURL := range []string{"http://user:secret@proxy.example:1080", "socks4://proxy.example:1080", "socks5:proxy.example:1080", "socks5://proxy.example", "socks5://:1080", "socks5://proxy.example:0", "socks5://proxy.example:65536", "socks5://proxy.example:bad", "socks5://proxy.example:1080/", "socks5://proxy.example:1080?", "socks5://proxy.example:1080#secret", "socks5://:secret@proxy.example:1080", "socks5://" + strings.Repeat("x", 256) + ":secret@proxy.example:1080", "socks5://user:" + strings.Repeat("x", 256) + "@proxy.example:1080", "socks5://user:%ZZ@proxy.example:1080"} {
		_, err := NewHTTPClient(proxyURL)
		if err == nil {
			t.Error("invalid proxy accepted")
			continue
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "proxy.example") || strings.Contains(err.Error(), "%ZZ") {
			t.Errorf("proxy details leaked: %v", err)
		}
	}
}

func TestSOCKSProxyHTTPHTTPSRemoteDNSAndCredentials(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			for _, secure := range []bool{false, true} {
				t.Run(fmt.Sprint(secure), func(t *testing.T) {
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Header.Get("Proxy-Authorization") != "" {
							t.Error("proxy credentials reached origin HTTP headers")
						}
						io.WriteString(w, "via SOCKS")
					})
					server := httptest.NewServer(handler)
					t.Cleanup(server.Close)
					var roots *x509.CertPool
					raw := "http://music.invalid/track"
					port := uint16(80)
					if secure {
						server, roots = proxyTLSFixture(t, handler)
						raw, port = "https://music.invalid/track", 443
					}
					proxy := newSOCKSFixture(t, server.Listener.Addr().String(), "listener", "p@ss:word", false, false)
					client := proxyClient(t, proxy.proxyURL(scheme), roots)
					response, err := client.Get(raw)
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil || string(body) != "via SOCKS" {
						t.Fatalf("unexpected origin response: %q, %v", body, err)
					}
					request := <-proxy.requests
					if request.host != "music.invalid" || request.port != port || request.addressType != 3 || request.user != "listener" || request.password != "p@ss:word" {
						t.Fatal("SOCKS hostname, port or authentication mismatch")
					}
				})
			}
		})
	}
}

func TestSOCKSProxyFailureDoesNotFallBackToDirect(t *testing.T) {
	var direct atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1) }))
	t.Cleanup(server.Close)
	for _, failure := range []string{"refused", "authentication", "unreachable"} {
		t.Run(failure, func(t *testing.T) {
			proxy := newSOCKSFixture(t, server.Listener.Addr().String(), "listener", "password", false, failure == "refused")
			raw := proxy.proxyURL("socks5")
			if failure == "authentication" {
				u, _ := url.Parse(raw)
				u.User = url.UserPassword("listener", "incorrect")
				raw = u.String()
			}
			if failure == "unreachable" {
				proxy.listener.Close()
			}
			client := proxyClient(t, raw, nil)
			client.Timeout = time.Second
			response, err := client.Get(server.URL)
			if err == nil {
				response.Body.Close()
				t.Fatal("unavailable proxy unexpectedly succeeded")
			}
			if direct.Load() != 0 {
				t.Fatal("failed proxy fell back to direct access")
			}
		})
	}
}

func TestSOCKSProxyContextCancellationAndDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			proxy := newSOCKSFixture(t, "", "", "", true, false)
			client := proxyClient(t, proxy.proxyURL("socks5"), nil)
			ctx, cancel := context.WithCancel(t.Context())
			want := context.Canceled
			if deadline {
				ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
				want = context.DeadlineExceeded
			}
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://music.invalid/track", nil)
			done := make(chan error, 1)
			go func() {
				response, err := client.Do(req)
				if response != nil {
					response.Body.Close()
				}
				done <- err
			}()
			select {
			case <-proxy.requests:
			case <-time.After(time.Second):
				t.Fatal("proxy handshake never started")
			}
			if !deadline {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("expected context error, got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("SOCKS handshake ignored context")
			}
		})
	}
}

func TestSOCKSProxyTransportIsolationAndDefaultClient(t *testing.T) {
	original := http.DefaultTransport
	first := proxyClient(t, "socks5://proxy.example:1080", nil)
	second := proxyClient(t, "socks5://other.example:1080", nil)
	if first.Transport == original || first.Transport == second.Transport || http.DefaultTransport != original || first.Timeout != 0 {
		t.Fatal("SoundCloud proxy mutated or reused a global transport")
	}
	client, err := NewClient(Config{ProxyURL: "socks5h://proxy.example:1080"}, nil)
	if err != nil || client.http.Timeout != 20*time.Second {
		t.Fatalf("default catalog client unavailable: %v", err)
	}
	transport := client.http.Transport.(*http.Transport)
	t.Cleanup(transport.CloseIdleConnections)
	req, _ := http.NewRequest(http.MethodGet, "https://api.soundcloud.com/tracks", nil)
	proxy, err := transport.Proxy(req)
	if err != nil || proxy.Scheme != "socks5h" || proxy.Host != "proxy.example:1080" {
		t.Fatal("NewClient ignored configured proxy")
	}
	if err := (Config{ProxyURL: "https://user:secret@proxy.example:1080"}).Validate(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("configuration did not safely validate proxy URL")
	}
}

func TestSOCKSProxyCatalogOAuthHLSAndArtwork(t *testing.T) {
	var requests sync.Map
	server, roots := proxyTLSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Store(r.Host+r.URL.Path, true)
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("SOCKS credentials leaked to SoundCloud origin")
		}
		switch r.Host + r.URL.Path {
		case "soundcloud.com/":
			io.WriteString(w, `<script src="https://a-v2.sndcdn.com/assets/app.js"></script>`)
		case "a-v2.sndcdn.com/assets/app.js":
			io.WriteString(w, `var config={client_id:"0123456789abcdef0123456789abcdef"};`)
		case "api-v2.soundcloud.com/search/tracks":
			io.WriteString(w, `{"collection":[{"id":12,"title":"Song","policy":"ALLOW","streamable":true,"user":{"id":4,"username":"Artist"},"media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/1","preset":"aac_160k","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}]}`)
		case "api-v2.soundcloud.com/media/1":
			io.WriteString(w, `{"url":"https://cf-hls-media.sndcdn.com/playlist.m3u8"}`)
		case "cf-hls-media.sndcdn.com/playlist.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXTINF:2,\nsegment.aac\n")
		case "cf-hls-media.sndcdn.com/segment.aac":
			io.WriteString(w, "audio bytes")
		case "i1.sndcdn.com/cover.jpg":
			http.Redirect(w, r, "https://i2.sndcdn.com/final-cover.jpg", http.StatusFound)
		case "i2.sndcdn.com/final-cover.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			io.WriteString(w, "cover bytes")
		case "secure.soundcloud.com/oauth/token":
			if r.Method != http.MethodPost {
				t.Error("OAuth request was not POST")
			}
			user, password, ok := r.BasicAuth()
			if !ok || user != "client-id" || password != "client-secret" {
				t.Error("OAuth principal changed inside proxy")
			}
			io.WriteString(w, `{"access_token":"token","expires_in":3600}`)
		case "api.soundcloud.com/tracks":
			if r.Header.Get("Authorization") != "OAuth token" {
				t.Error("catalog access token changed inside proxy")
			}
			io.WriteString(w, `{"collection":[{"urn":"soundcloud:tracks:1","title":"Song","access":"playable"}]}`)
		default:
			t.Errorf("unexpected fixture origin %s%s", r.Host, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	proxy := newSOCKSFixture(t, server.Listener.Addr().String(), "proxy-user", "proxy-password", false, false)
	hc := proxyClient(t, proxy.proxyURL("socks5h"), roots)
	client, err := NewClient(Config{API: "web"}, hc)
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := client.Search(t.Context(), "Song", TrackKind, 1)
	if err != nil || len(tracks) != 1 {
		t.Fatalf("web catalog over SOCKS: %v", err)
	}
	relay, err := NewRelay("http://bridge.test", hc)
	if err != nil {
		t.Fatal(err)
	}
	media, err := relay.Register("https://i1.sndcdn.com/cover.jpg", func(ctx context.Context) (string, error) {
		return client.Stream(ctx, tracks[0])
	})
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(relay)
	t.Cleanup(local.Close)
	get := func(raw string) string {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		response, err := local.Client().Get(local.URL + u.RequestURI())
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("relay over SOCKS: status=%d err=%v", response.StatusCode, err)
		}
		return string(body)
	}
	manifest := get(media.URL)
	var segment string
	for line := range strings.SplitSeq(manifest, "\n") {
		if strings.HasPrefix(line, "http://bridge.test/soundcloud/") {
			segment = line
		}
	}
	if segment == "" || get(segment) != "audio bytes" || get(media.ArtworkURL) != "cover bytes" {
		t.Fatal("relay media or artwork not delivered through SOCKS")
	}
	official, err := NewClient(Config{API: "official", ClientID: "client-id", ClientSecret: "client-secret"}, hc)
	if err != nil {
		t.Fatal(err)
	}
	if tracks, err := official.Search(t.Context(), "Song", TrackKind, 1); err != nil || len(tracks) != 1 {
		t.Fatalf("OAuth catalog over SOCKS: %v", err)
	}
	for _, request := range []string{"soundcloud.com/", "a-v2.sndcdn.com/assets/app.js", "api-v2.soundcloud.com/search/tracks", "api-v2.soundcloud.com/media/1", "cf-hls-media.sndcdn.com/playlist.m3u8", "cf-hls-media.sndcdn.com/segment.aac", "i1.sndcdn.com/cover.jpg", "i2.sndcdn.com/final-cover.jpg", "secure.soundcloud.com/oauth/token", "api.soundcloud.com/tracks"} {
		if _, ok := requests.Load(request); !ok {
			t.Fatalf("missing proxied request: %s", request)
		}
	}
	if client.http.Timeout != 20*time.Second || relay.client.Timeout != 35*time.Second {
		t.Fatal("shared proxy changed catalog/media timeouts")
	}
}

func TestSOCKSProxyErrorsDoNotExposeCredentials(t *testing.T) {
	proxy := newSOCKSFixture(t, "", "private-proxy-user", "expected-password", false, false)
	u, _ := url.Parse(proxy.proxyURL("socks5"))
	u.User = url.UserPassword("private-proxy-user", "private-wrong-password")
	raw := u.String()
	assertRedacted := func(text string) {
		t.Helper()
		for _, secret := range []string{"private-proxy-user", "private-wrong-password", proxy.listener.Addr().String(), raw} {
			if strings.Contains(text, secret) {
				t.Fatal("proxy error exposed connection credentials")
			}
		}
	}
	for _, cfg := range []Config{
		{API: "web", ClientID: "public-client", ProxyURL: raw},
		{API: "official", ClientID: "client", ClientSecret: "client-secret", ProxyURL: raw},
	} {
		client, err := NewClient(cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.http.CloseIdleConnections)
		if _, err := client.Search(t.Context(), "Song", TrackKind, 1); err == nil {
			t.Fatal("catalog unexpectedly accepted invalid SOCKS credentials")
		} else {
			assertRedacted(err.Error())
		}
	}
	hc := proxyClient(t, raw, nil)
	relay, err := NewRelay("http://bridge.test", hc)
	if err != nil {
		t.Fatal(err)
	}
	media, err := relay.Register("https://i1.sndcdn.com/cover.jpg", func(context.Context) (string, error) {
		return "https://cf-hls-media.sndcdn.com/playlist.m3u8", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	for _, raw := range []string{media.URL, media.ArtworkURL} {
		u, _ := url.Parse(raw)
		response, err := server.Client().Get(server.URL + u.RequestURI())
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusBadGateway {
			t.Fatalf("expected media proxy error, got %d / %v", response.StatusCode, err)
		}
		assertRedacted(string(body))
	}
}
