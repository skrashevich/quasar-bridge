package assistant

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestResolveMusicRPFMWithoutSpeaker(t *testing.T) {
	for _, source := range []string{"https://rpfm.ru", "rpfm точка ru", "Алиса, включи музыку с сайта rpfm точка ru"} {
		t.Run(source, func(t *testing.T) {
			reader := NewWebReader(DefaultConfig())
			reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://rpfm.ru" {
					t.Fatalf("fetch=%s", r.URL)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(`<a href="https://hls.rpfm.ru/rpfm/aac_hifi.m3u8">...</a>`)), Request: r}, nil
			})
			report, err := reader.ResolveMusic(t.Context(), source)
			if err != nil || report.Source != "https://rpfm.ru" || len(report.Tracks) != 1 || report.Tracks[0].URL != "https://hls.rpfm.ru/rpfm/aac_hifi.m3u8" {
				t.Fatalf("report=%+v err=%v", report, err)
			}
		})
	}
}

func TestResolveMusicDirectHLSDoesNotFetch(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.http.Transport = roundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("direct HLS should not fetch")
		return nil, nil
	})
	report, err := reader.ResolveMusic(t.Context(), "https://hls.rpfm.ru/rpfm/aac_hifi.m3u8?token=secret")
	if err != nil || report.Source != "https://hls.rpfm.ru/rpfm/aac_hifi.m3u8?token=secret" || len(report.Tracks) != 1 || report.Tracks[0].URL != "https://hls.rpfm.ru/rpfm/aac_hifi.m3u8?token=secret" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestResolveMusicFailurePreservesSourceAndSafeReason(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		typ    string
		body   string
		err    error
		want   string
	}{
		{name: "transport", err: errors.New("https://audio.example/?token=secret failed"), want: "web request failed"},
		{name: "HTTP", status: 503, typ: "text/html", want: "HTTP 503"},
		{name: "content type", status: 200, typ: "image/png", want: "content type"},
		{name: "no audio", status: 200, typ: "text/html", body: "A page without audio", want: "no playable audio"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := NewWebReader(DefaultConfig())
			reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
				if tt.err != nil {
					return nil, tt.err
				}
				return &http.Response{StatusCode: tt.status, Header: http.Header{"Content-Type": {tt.typ}}, Body: io.NopCloser(strings.NewReader(tt.body)), Request: r}, nil
			})
			report, err := reader.ResolveMusic(t.Context(), "https://audio.example/?token=secret")
			if report.Source != "https://audio.example/?token=secret" || len(report.Tracks) != 0 || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			if strings.Contains(err.Error(), "audio.example") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error exposes source: %v", err)
			}
		})
	}
}

func TestResolveMusicRejectsInvalidSourceBeforeFetching(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.http.Transport = roundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid source fetched")
		return nil, nil
	})
	for _, source := range []string{
		"", "notification", "ftp://audio.example/file.mp3", "http://127.0.0.1/music", "https://localhost/music", "https://u:p@audio.example/music", "https://audio.example/music#fragment",
		"https://audio.example/" + strings.Repeat("я", 400),
	} {
		if report, err := reader.ResolveMusic(t.Context(), source); err == nil || len(report.Tracks) != 0 {
			t.Fatalf("accepted source=%q report=%+v err=%v", source, report, err)
		}
	}
}

func TestResolveMusicNavigationStaysOnHostAndStopsAfterThreeSections(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	var fetched []string
	reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.String())
		body := "No audio here"
		if r.URL.Path == "/" {
			body = `<a href="https://other.example/music">Music</a><a href="http://audio.example/music">Music</a><a href="/music-one">Music</a><a href="/music-two">Music</a><a href="/music-three">Music</a><a href="/music-four">Music</a>`
		}
		if r.URL.Path == "/music-four" {
			body = `<a href="/track.mp3">Track</a>`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	report, err := reader.ResolveMusic(t.Context(), "https://audio.example/")
	if err == nil || len(report.Tracks) != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if !slices.Equal(fetched, []string{"https://audio.example/", "https://audio.example/music-one", "https://audio.example/music-two", "https://audio.example/music-three"}) {
		t.Fatalf("navigation=%v", fetched)
	}
}

func TestResolveMusicStopsNavigationWhenCanceled(t *testing.T) {
	for _, body := range []string{"No audio here", `<a href="/track.mp3">Track</a>`} {
		t.Run(body, func(t *testing.T) {
			reader := NewWebReader(DefaultConfig())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var fetched []string
			reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
				fetched = append(fetched, r.URL.String())
				responseBody := `<a href="/music-one">Music</a><a href="/music-two">Music</a><a href="/music-three">Music</a>`
				if r.URL.Path != "/" {
					cancel()
					responseBody = body
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(responseBody)), Request: r}, nil
			})
			report, err := reader.ResolveMusic(ctx, "https://audio.example/")
			if report.Source != "https://audio.example/" || len(report.Tracks) != 0 || !errors.Is(err, context.Canceled) {
				t.Fatalf("report=%+v err=%v fetched=%v", report, err, fetched)
			}
			if !slices.Equal(fetched, []string{"https://audio.example/", "https://audio.example/music-one"}) {
				t.Fatalf("navigation continued after cancellation: %v", fetched)
			}
		})
	}
}

func TestResolveMusicSectionFailurePreservesSourceAndSafeReason(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	var fetched []string
	reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.String())
		status := 200
		body := "No audio here"
		switch r.URL.Path {
		case "/":
			body = `<a href="/music?token=section-secret">Music</a><a href="/music-empty">Music</a>`
		case "/music":
			status = 503
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	report, err := reader.ResolveMusic(t.Context(), "https://audio.example/?token=source-secret")
	if report.Source != "https://audio.example/?token=source-secret" || len(report.Tracks) != 0 || err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if strings.Contains(err.Error(), "audio.example") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error exposes source: %v", err)
	}
	if !slices.Equal(fetched, []string{"https://audio.example/?token=source-secret", "https://audio.example/music?token=section-secret", "https://audio.example/music-empty"}) {
		t.Fatalf("navigation=%v", fetched)
	}
}

func TestResolveMusicContinuesAfterSectionFailure(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.http.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		status := 200
		body := `<a href="/music-one">Music</a><a href="/music-two">Music</a>`
		switch r.URL.Path {
		case "/music-one":
			status = 503
		case "/music-two":
			body = `<a href="https://hls.rpfm.ru/rpfm/aac_hifi.m3u8">...</a>`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	report, err := reader.ResolveMusic(t.Context(), "https://audio.example/")
	if err != nil || report.Source != "https://audio.example/" || len(report.Tracks) != 1 || report.Tracks[0].URL != "https://hls.rpfm.ru/rpfm/aac_hifi.m3u8" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestResolveMusicPreservesCancellation(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.http.Transport = roundTripper(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	report, err := reader.ResolveMusic(ctx, "https://audio.example/music")
	if report.Source != "https://audio.example/music" || !errors.Is(err, context.Canceled) {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestResolveMusicRejectsCanceledDirectSource(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.http.Transport = roundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("canceled direct source fetched")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	report, err := reader.ResolveMusic(ctx, "https://hls.rpfm.ru/rpfm/aac_hifi.m3u8")
	if report.Source != "https://hls.rpfm.ru/rpfm/aac_hifi.m3u8" || len(report.Tracks) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}
