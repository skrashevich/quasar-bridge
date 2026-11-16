package backendcompat

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

const fixture = `{"status":"ok","config":{"system_config":{"voiceDialogSettings":{"uniProxyPreferQuic":true,"uniProxyQuicUrl":"https://quic.example.invalid:8443","recognizer":{"recordingTimeout":50},"wsUseZstd":true},"other":9007199254740993123},"device_config":{"volume":7},"account_config":{"keep":true}},"subscription":{"ttl":3600},"unknown":[1,null,"keep"]}`

func TestPatchPreservesAllOtherSettings(t *testing.T) {
	result, ok := patchSync([]byte(fixture))
	if !ok {
		t.Fatal("expected a transport rewrite")
	}
	var before, after map[string]any
	if err := json.Unmarshal([]byte(fixture), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(result, &after); err != nil {
		t.Fatal(err)
	}
	voice := after["config"].(map[string]any)["system_config"].(map[string]any)["voiceDialogSettings"].(map[string]any)
	if voice["uniProxyPreferQuic"] != false || voice["uniProxyQuicUrl"] != "" {
		t.Fatal("QUIC remained enabled")
	}
	voice["uniProxyPreferQuic"] = true
	voice["uniProxyQuicUrl"] = "https://quic.example.invalid:8443"
	if !reflect.DeepEqual(before, after) {
		t.Fatal("unrelated settings changed")
	}
	if !bytes.Contains(result, []byte("9007199254740993123")) {
		t.Fatal("large integer lost precision")
	}
}

func TestPatchEncodedLayers(t *testing.T) {
	for _, level := range []string{"config", "system_config", "voiceDialogSettings"} {
		t.Run(level, func(t *testing.T) {
			var doc map[string]jsontext.Value
			json.Unmarshal([]byte(fixture), &doc)
			var config, system map[string]jsontext.Value
			json.Unmarshal(doc["config"], &config)
			json.Unmarshal(config["system_config"], &system)
			switch level {
			case "voiceDialogSettings":
				system[level], _ = json.Marshal(string(system[level]))
				config["system_config"], _ = json.Marshal(system)
				doc["config"], _ = json.Marshal(config)
			case "system_config":
				config[level], _ = json.Marshal(string(config[level]))
				doc["config"], _ = json.Marshal(config)
			case "config":
				doc[level], _ = json.Marshal(string(doc[level]))
			}
			raw, _ := json.Marshal(doc)
			got, ok := patchSync(raw)
			if !ok || bytes.Contains(got, []byte("quic.example.invalid")) {
				t.Fatal("encoded layer not patched")
			}
			// Encoding form must survive; decode the edited path explicitly.
			json.Unmarshal(got, &doc)
			value := doc["config"]
			for _, key := range []string{"system_config", "voiceDialogSettings", "uniProxyPreferQuic"} {
				var text string
				if json.Unmarshal(value, &text) == nil {
					value = jsontext.Value(text)
				}
				var obj map[string]jsontext.Value
				if json.Unmarshal(value, &obj) != nil {
					t.Fatal("invalid encoded layer")
				}
				if key == level {
					var encoded string
					if json.Unmarshal(obj[key], &encoded) != nil {
						t.Fatal("string encoding lost")
					}
				}
				value = obj[key]
			}
			if string(value) != "false" {
				t.Fatal("wrong final setting")
			}
		})
	}
}

func TestUnexpectedSchemasPassThroughExactly(t *testing.T) {
	for _, input := range []string{
		`not json`, `[]`, `null`, `{"status":"error","config":{}}`,
		`{"status":"ok","config":{}}`,
		`{"status":"ok","config":{"system_config":null}}`,
		`{"status":"ok","config":{"system_config":{"voiceDialogSettings":[]}}}`,
		`{"status":"ok","status":"ok","config":{}}`,
	} {
		got, changed := patchSync([]byte(input))
		if changed || string(got) != input {
			t.Fatal("unexpected schema was modified")
		}
	}
}

func TestProxyForwardsOnlyToFixedUpstream(t *testing.T) {
	seen := make(chan *http.Request, 1)
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"original"`)
		w.Header().Set("Content-MD5", "original")
		io.WriteString(w, fixture)
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)
	p := newProxy(target, up.Client().Transport)
	r := httptest.NewRequest("GET", "https://untrusted.invalid"+Prefix+"/get_sync_info?device_id=fixture", nil)
	r.Header.Set("Authorization", "OAuth fixture-secret")
	r.Header.Set("Cookie", "fixture=cookie")
	r.Header.Set("X-Forwarded-Host", "untrusted.invalid")
	r.Header.Set("If-None-Match", `"original"`)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte("quic.example.invalid")) {
		t.Fatal("proxy did not patch")
	}
	u := <-seen
	if u.Host != target.Host || u.URL.Path != "/get_sync_info" || u.URL.RawQuery != "device_id=fixture" {
		t.Fatal("upstream routing changed")
	}
	if u.Header.Get("Authorization") != "OAuth fixture-secret" || u.Header.Get("Cookie") != "fixture=cookie" {
		t.Fatal("native credentials not forwarded")
	}
	if u.Header.Get("If-None-Match") != "" || u.Header.Get("X-Forwarded-Host") != "" {
		t.Fatal("unsafe or conditional headers forwarded")
	}
	if w.Header().Get("ETag") != "" || w.Header().Get("Content-MD5") != "" || w.Result().ContentLength != int64(w.Body.Len()) {
		t.Fatal("stale body metadata")
	}
	if s := p.Snapshot(); s.Requests != 1 || s.Rewritten != 1 || s.Failures != 0 {
		t.Fatal("wrong health counters")
	}
}

func TestNativeRequestsAndNonSuccessPassThrough(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/other", 200}, {"POST", "/get_sync_info", 200}, {"GET", "/get_sync_info", 401},
	} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.method == "POST" {
				b, _ := io.ReadAll(r.Body)
				if string(b) != "fixture-body" {
					t.Error("body changed")
				}
			}
			w.WriteHeader(tc.status)
			io.WriteString(w, fixture)
		}))
		target, _ := url.Parse(up.URL)
		p := newProxy(target, up.Client().Transport)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest(tc.method, Prefix+tc.path, strings.NewReader("fixture-body")))
		up.Close()
		if w.Code != tc.status || w.Body.String() != fixture {
			t.Fatal("unrelated backend response changed")
		}
	}
}

func TestGzipAndUnchangedResponses(t *testing.T) {
	for _, input := range []string{fixture, `{"status":"ok","config":{}}`} {
		var compressed bytes.Buffer
		z := gzip.NewWriter(&compressed)
		io.WriteString(z, input)
		z.Close()
		p := New()
		r := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(compressed.Bytes())), Request: httptest.NewRequest("GET", "/get_sync_info", nil)}
		r.Header.Set("Content-Encoding", "gzip")
		if err := p.modifyResponse(r); err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if input == fixture {
			if r.Header.Get("Content-Encoding") != "" || !jsontext.Value(got).IsValid() {
				t.Fatal("gzip rewrite invalid")
			}
		} else if !bytes.Equal(got, compressed.Bytes()) || r.Header.Get("Content-Encoding") != "gzip" {
			t.Fatal("unmatched body not preserved")
		}
	}
}

func TestResponseLimitAndTLSVerification(t *testing.T) {
	if _, err := readBounded(strings.NewReader(strings.Repeat("x", maxResponse+1))); err == nil {
		t.Fatal("unbounded response")
	}
	p := New()
	transport := p.proxy.Transport.(*http.Transport)
	if transport.Proxy != nil || (transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify) {
		t.Fatal("unsafe production transport")
	}
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Error("untrusted TLS reached handler") }))
	up.Config.ErrorLog = log.New(io.Discard, "", 0)
	up.StartTLS()
	defer up.Close()
	target, _ := url.Parse(up.URL)
	p = newProxy(target, transport)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", Prefix+"/get_sync_info?token=fixture-secret", nil))
	if w.Code != 502 || strings.Contains(w.Body.String(), "fixture-secret") {
		t.Fatal("TLS bypass or diagnostic leakage")
	}
}

func TestCancellationAndCapacity(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)
	p := newProxy(target, up.Client().Transport)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", Prefix+"/get_sync_info", nil).WithContext(ctx))
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request not started")
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
		t.Fatal("proxy did not return")
	}
	for i := 0; i < cap(p.active); i++ {
		p.active <- struct{}{}
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", Prefix+"/get_sync_info", nil))
	if w.Code != 503 {
		t.Fatal("capacity not bounded")
	}
}

func TestRejectsPathsOutsidePrefix(t *testing.T) {
	p := New()
	for _, path := range []string{"/get_sync_info", Prefix + "-other/get_sync_info"} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatal("unexpected route accepted")
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpstreamHasFiniteBudget(t *testing.T) {
	target, _ := url.Parse("https://quasar.yandex.net")
	p := newProxy(target, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 60*time.Second {
			t.Fatal("missing or excessive upstream budget")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("native")), Request: r}, nil
	}))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", Prefix+"/other", nil))
	if w.Code != 200 || w.Body.String() != "native" {
		t.Fatal("bounded request failed")
	}
}

func TestCompressedResponseLimit(t *testing.T) {
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	io.CopyN(z, strings.NewReader(strings.Repeat("x", maxResponse+1)), maxResponse+1)
	z.Close()
	p := New()
	r := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b.Bytes())), Request: httptest.NewRequest("GET", "/get_sync_info", nil)}
	r.Header.Set("Content-Encoding", "gzip")
	if p.modifyResponse(r) == nil {
		t.Fatal("compressed response bypassed limit")
	}
}
