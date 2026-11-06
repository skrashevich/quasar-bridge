package soundcloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestWebDiscoverySearchFallbackOverSOCKS(t *testing.T) {
	var mu sync.Mutex
	counts := make(map[string]int)
	server, roots := proxyTLSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		mu.Unlock()
		switch r.Host + r.URL.Path {
		case "soundcloud.com/":
			w.WriteHeader(http.StatusAccepted)
		case "soundcloud.com/search":
			if r.URL.Query().Get("q") != "music" {
				t.Error("discovery fallback did not use generic public search")
			}
			io.WriteString(w, `<script src="https://a-v2.sndcdn.com/assets/public-app.js"></script>`)
		case "a-v2.sndcdn.com/assets/public-app.js":
			io.WriteString(w, `client_id:"0123456789abcdef0123456789abcdef"`)
		case "api-v2.soundcloud.com/search/tracks":
			if r.URL.Query().Get("client_id") != "0123456789abcdef0123456789abcdef" {
				t.Error("catalog did not use discovered client ID")
			}
			io.WriteString(w, `{"collection":[{"id":1,"title":"Song","policy":"ALLOW","streamable":true}]}`)
		default:
			t.Errorf("unexpected discovery request: %s%s", r.Host, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	proxy := newSOCKSFixture(t, server.Listener.Addr().String(), "listener", "password", false, false)
	client, err := NewClient(Config{API: "web"}, proxyClient(t, proxy.proxyURL("socks5h"), roots))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		tracks, err := client.Search(t.Context(), "Song", TrackKind, 1)
		if err != nil || len(tracks) != 1 {
			t.Fatalf("public search fallback failed: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["/"] != 1 || counts["/search"] != 1 || counts["/assets/public-app.js"] != 1 || counts["/search/tracks"] != 2 {
		t.Fatalf("discovery was not cached: %v", counts)
	}
}

type discoveryTransport func(*http.Request) (*http.Response, error)

func (f discoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWebDiscoveryDoesNotFallbackAfterNetworkFailure(t *testing.T) {
	for _, failPath := range []string{"/", "/app.js"} {
		for _, failure := range []error{errors.New("network unavailable"), context.DeadlineExceeded} {
			t.Run(failPath+failure.Error(), func(t *testing.T) {
				var paths []string
				hc := &http.Client{Transport: discoveryTransport(func(r *http.Request) (*http.Response, error) {
					paths = append(paths, r.URL.Path)
					if r.URL.Path == failPath {
						return nil, failure
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`<script src="https://a-v2.sndcdn.com/app.js"></script>`)), Header: make(http.Header), Request: r}, nil
				})}
				client, err := NewClient(Config{API: "web"}, hc)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := client.webID(t.Context(), false); err == nil {
					t.Fatal("network failure unexpectedly discovered a client ID")
				}
				want := 1
				if failPath == "/app.js" {
					want = 2
				}
				if len(paths) != want {
					t.Fatalf("network failure was retried: %v", paths)
				}
			})
		}
	}
}

func TestWebDiscoveryCanceledBeforeRequest(t *testing.T) {
	calls := 0
	client, err := NewClient(Config{API: "web"}, &http.Client{Transport: discoveryTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected request")
	})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.webID(ctx, false); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("canceled discovery started a network request: calls=%d err=%v", calls, err)
	}
}

func TestWebDiscoveryBundlesHaveSharedBudgetAndNoDuplicates(t *testing.T) {
	for _, budget := range []bool{false, true} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			var mu sync.Mutex
			counts := make(map[string]int)
			hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				counts[r.URL.Path]++
				mu.Unlock()
				if r.URL.Path == "/" || r.URL.Path == "/search" {
					if budget {
						start, end := 0, 4
						if r.URL.Path == "/search" {
							end = 12
						}
						for i := start; i < end; i++ {
							fmt.Fprintf(w, `<script src="https://a-v2.sndcdn.com/bundle-%d.js"></script>`, i)
						}
					} else {
						if r.URL.Path == "/search" {
							io.WriteString(w, `<script src="https://a-v2.sndcdn.com/new.js"></script>`)
						}
						io.WriteString(w, `<script src="https://a-v2.sndcdn.com/shared.js"></script><script src="https://a-v2.sndcdn.com/shared.js"></script>`)
					}
				} else if r.URL.Path == "/new.js" {
					io.WriteString(w, `clientId="0123456789abcdef0123456789abcdef"`)
				} else {
					io.WriteString(w, "no client ID in this bundle")
				}
			})
			client, err := NewClient(Config{API: "web"}, hc)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.webID(t.Context(), false)
			if (err != nil) != budget {
				t.Fatalf("unexpected discovery result: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			bundles := 0
			for path, count := range counts {
				if strings.HasSuffix(path, ".js") {
					if count != 1 {
						t.Fatalf("bundle downloaded repeatedly: %s", path)
					}
					bundles++
				}
			}
			if counts["/"] != 1 || counts["/search"] != 1 || budget && bundles != 8 {
				t.Fatalf("discovery exceeded page or script budget: pages=%v bundles=%d", counts, bundles)
			}
		})
	}
}

func TestWebDiscoveryForceRefreshUsesFallback(t *testing.T) {
	var homepage atomic.Int32
	client, err := NewClient(Config{API: "web", ClientID: strings.Repeat("a", 32)}, fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			homepage.Add(1)
			w.WriteHeader(http.StatusAccepted)
		case "/search":
			io.WriteString(w, `<script src="https://a-v2.sndcdn.com/new.js"></script>`)
		case "/new.js":
			fmt.Fprintf(w, `clientId="%s"`, strings.Repeat("b", 32))
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := client.webID(t.Context(), false); err != nil || id != strings.Repeat("a", 32) || homepage.Load() != 0 {
		t.Fatal("cached public client ID was ignored")
	}
	if id, err := client.webID(t.Context(), true); err != nil || id != strings.Repeat("b", 32) || homepage.Load() != 1 {
		t.Fatalf("forced public client ID refresh failed: %v", err)
	}
}
