package bridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/musicmatch"
	"quasarbridge/internal/soundcloud"
)

type implicitComparisonProvider struct {
	*comparisonProvider
	eligible bool
	checks   atomic.Int32
	prepares atomic.Int32
	entered  chan struct{}
}

func (p *implicitComparisonProvider) ImplicitComparisonEnabled(context.Context, string) bool {
	p.checks.Add(1)
	return p.eligible
}
func (p *implicitComparisonProvider) Prepare(ctx context.Context, text string) (musicmatch.Prepared, error) {
	p.prepares.Add(1)
	if p.entered != nil {
		close(p.entered)
	}
	return p.comparisonProvider.Prepare(ctx, text)
}

func TestImplicitMusicComparisonPositiveClassifierAdmission(t *testing.T) {
	p := &implicitComparisonProvider{comparisonProvider: new(comparisonProvider), eligible: true}
	var classifications atomic.Int32
	e := &Engine{SoundCloud: p, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		classifications.Add(1)
		return `{"route":"yandex"}`, nil
	})}
	ctx := e.Prepare(dialogue.WithSpeaker(t.Context(), "implicit-speaker"))
	decision := e.Decide(ctx, "Включи нас нет")
	if decision.Route != "music_compare" || decision.Source != "implicit_music_compare" || classifications.Load() != 1 || p.checks.Load() != 1 || p.prepares.Load() != 0 {
		t.Fatalf("late admission=%+v classifier=%d eligible=%d prepares=%d", decision, classifications.Load(), p.checks.Load(), p.prepares.Load())
	}
}

func TestImplicitMusicComparisonPreservesPriorRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, choice, static, route, source string
		home, disabled, fail, timeout, late bool
	}{
		{name: "home", choice: "home", home: true, route: "home", source: "classifier"},
		{name: "disabled-home", choice: "home", route: "yandex", source: "home_disabled"},
		{name: "llm", choice: "llm", route: "llm", source: "classifier"},
		{name: "classifier-error", fail: true, route: "yandex", source: "classifier_error"},
		{name: "classifier-timeout", timeout: true, route: "yandex", source: "classifier_error"},
		{name: "late-positive-after-timeout", timeout: true, late: true, route: "yandex", source: "classifier"},
		{name: "default-yandex", disabled: true, route: "yandex", source: "default"},
		{name: "static-yandex", static: "yandex", route: "yandex", source: "rule"},
		{name: "static-home-disabled", static: "home", route: "yandex", source: "home_disabled"},
		{name: "static-llm", static: "llm", route: "llm", source: "rule"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &implicitComparisonProvider{comparisonProvider: new(comparisonProvider), eligible: true}
			var calls atomic.Int32
			e := &Engine{SoundCloud: p, LLM: completionFunc(func(ctx context.Context, _ []llm.Message, _ bool) (string, error) {
				calls.Add(1)
				if tc.timeout {
					<-ctx.Done()
					if tc.late {
						return `{"route":"yandex"}`, nil
					}
					return "", ctx.Err()
				}
				if tc.fail {
					return "", errors.New("offline")
				}
				return `{"route":"` + tc.choice + `"}`, nil
			})}
			if tc.home {
				e.Home = homeFunc(func(context.Context, string) (string, error) { return "", nil })
			}
			cfg := DefaultRoutingConfig()
			cfg.Rules = nil
			cfg.Classifier.TimeoutMS = 10
			if tc.disabled {
				cfg.DefaultRoute = "yandex"
			}
			if tc.static != "" {
				cfg.Rules = []RoutingRule{{Name: "user-route", Route: tc.static, Match: RoutingMatch{ContainsAny: []string{"нас нет"}}}}
			}
			if err := e.SetRouting(cfg); err != nil {
				t.Fatal(err)
			}
			ctx := e.Prepare(dialogue.WithSpeaker(t.Context(), "priority-speaker"))
			d := e.Decide(ctx, "Включи нас нет")
			wantCalls := int32(1)
			if tc.static != "" || tc.disabled {
				wantCalls = 0
			}
			if d.Route != tc.route || d.Source != tc.source || p.checks.Load() != 0 || p.prepares.Load() != 0 || calls.Load() != wantCalls {
				t.Fatalf("decision=%+v implicit-checks=%d prepares=%d classifier=%d", d, p.checks.Load(), p.prepares.Load(), calls.Load())
			}
		})
	}
}

func TestImplicitMusicComparisonStrictFastPathAndUnsupportedProvider(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsupported", true: "strict-fast-path"}[strict], func(t *testing.T) {
			var calls atomic.Int32
			p := new(comparisonProvider)
			implicit := &implicitComparisonProvider{comparisonProvider: p, eligible: true}
			e := &Engine{SoundCloud: p, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
				calls.Add(1)
				return `{"route":"yandex"}`, nil
			})}
			text, route, source, wantCalls := "Включи нас нет", "yandex", "classifier", int32(1)
			if strict {
				e.SoundCloud = implicit
				text, route, source, wantCalls = "включи песню Нас нет", "music_compare", "music_compare", 0
			}
			d := e.Decide(e.Prepare(dialogue.WithSpeaker(t.Context(), "fast-path-speaker")), text)
			if d.Route != route || d.Source != source || calls.Load() != wantCalls || implicit.checks.Load() != 0 {
				t.Fatalf("decision=%+v classifier=%d", d, calls.Load())
			}
		})
	}
}

func TestNativeImplicitMusicComparisonRunsParallelAndBounded(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "soundcloud-winner", true: "bounded-timeout"}[timeout], func(t *testing.T) {
			release := make(chan struct{})
			p := &implicitComparisonProvider{comparisonProvider: &comparisonProvider{candidate: musicmatch.Candidate{Title: "Нас нет", Artist: "svk"}, release: release}, eligible: true, entered: make(chan struct{})}
			var calls atomic.Int32
			e := &Engine{SoundCloud: p, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
				calls.Add(1)
				return `{"route":"yandex"}`, nil
			})}
			n, up, down := soundSession(t, e)
			if timeout {
				n.ComparisonTimeout = 40 * time.Millisecond
			}
			if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("implicit", "Включи нас нет")); err != nil {
				t.Fatal(err)
			}
			if f := nextSoundFrame(t, up); f.Kind() != "TextInput" {
				t.Fatal("Yandex search was not delegated while SoundCloud resolves")
			}
			select {
			case <-p.entered:
			case <-time.After(time.Second):
				t.Fatal("parallel SoundCloud preparation did not begin")
			}
			if err := n.Server(t.Context(), websocket.MessageBinary, comparisonCloud("implicit", "Other", "Artist")); err != nil {
				t.Fatal(err)
			}
			if !timeout {
				close(release)
			}
			ds := finalSoundDirectives(t, down)
			n.wg.Wait()
			if len(ds) != 1 || calls.Load() != 1 || p.prepares.Load() != 1 || p.checks.Load() != 1 {
				t.Fatalf("directives=%d classifier=%d prepares=%d checks=%d", len(ds), calls.Load(), p.prepares.Load(), p.checks.Load())
			}
			wantCommit := int32(1)
			if timeout {
				wantCommit = 0
			}
			if p.committed.Load() != wantCommit {
				t.Fatal("implicit comparison selected wrong winner", p.committed.Load())
			}
		})
	}
}

func TestNativeImplicitMusicComparisonRealProviderHTTP(t *testing.T) {
	var searches, streams, classifier atomic.Int32
	hc := &http.Client{Transport: soundCloudHTTPTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		if strings.HasSuffix(r.URL.Path, "/streams") {
			streams.Add(1)
			body = `{"hls_aac_160_url":"https://cf-media.sndcdn.com/fixture.m3u8"}`
		} else {
			searches.Add(1)
			if !strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("q")), "нас нет") {
				t.Error("bare title search query changed", r.URL.Query().Get("q"))
			}
			body = `{"collection":[{"id":1,"title":"Нас нет","duration":120000,"metadata_artist":"svk"}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	cfg := soundcloud.Config{Enabled: true, CompareYandex: true, Default: true, API: "official", AccessToken: "fixture"}
	catalog, err := soundcloud.NewClient(cfg, hc)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := soundcloud.NewRelay("http://bridge.invalid", hc)
	if err != nil {
		t.Fatal(err)
	}
	provider := soundcloud.NewProvider(cfg, catalog, relay)
	e := &Engine{SoundCloud: provider, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		classifier.Add(1)
		return `{"route":"yandex"}`, nil
	})}
	n, up, down := soundSession(t, e)
	if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("bare-http", "Включи нас нет")); err != nil {
		t.Fatal(err)
	}
	if f := nextSoundFrame(t, up); f.Kind() != "TextInput" {
		t.Fatal("Yandex branch missing")
	}
	if err := n.Server(t.Context(), websocket.MessageBinary, comparisonCloud("bare-http", "Other", "Artist")); err != nil {
		t.Fatal(err)
	}
	ds := finalSoundDirectives(t, down)
	n.wg.Wait()
	if len(ds) != 2 || ds[0].GetName() != "audio_play" || ds[1].GetName() != nativeMusicGetNext || ds[0].GetPayload().GetFields()["provider_name"].GetStringValue() != "SoundCloud" || ds[0].GetPayload().GetFields()["metadata"].GetStructValue().GetFields()["title"].GetStringValue() != "Нас нет" {
		t.Fatal("exact bare title did not enter the native SoundCloud queue")
	}
	if classifier.Load() != 1 || searches.Load() != 1 || streams.Load() != 1 {
		t.Fatalf("unbounded/repeated work: classifier=%d searches=%d streams=%d", classifier.Load(), searches.Load(), streams.Load())
	}
}
