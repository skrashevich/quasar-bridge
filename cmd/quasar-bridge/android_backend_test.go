package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json/jsontext"
	"flag"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAndroidBackendProxyOptIn(t *testing.T) {
	for _, value := range []string{"", "1", "0", "true", "private-setting-secret"} {
		t.Setenv("QUASAR_ANDROID_BACKEND_PROXY", value)
		enabled, err := androidBackendProxyEnabled()
		if value == "" || value == "1" {
			if err != nil || enabled != (value == "1") {
				t.Fatalf("opt-in %q: enabled=%v error=%v", value, enabled, err)
			}
		} else if err == nil || strings.Contains(err.Error(), value) {
			t.Fatalf("invalid opt-in accepted or leaked: %v", err)
		}
	}
}

// Losing the relay or mutating another field breaks native cloud synchronization.
func TestAndroidBackendRewritesOnlySpeechEndpoint(t *testing.T) {
	const original = `{"status":"ok","config":{"account_config":{"contentAccess":"medium","spotter":"alisa"},"system_config":{"uniProxyUrl":"wss://uniproxy.alice.yandex.net/uni.ws","other":{"number":900719925474099312345}},"unknown":[1,true,null]},"group":{"keep":"yes"},"extension":{"raw":900719925474099312345}}`
	var authorized atomic.Bool
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorized.Store(r.Header.Get("Authorization") == "OAuth fixture-secret" && r.URL.RequestURI() == "/get_sync_info?device_id=fixture" && r.Header.Get("Accept-Encoding") == "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		for _, name := range []string{"ETag", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest"} {
			w.Header().Set(name, "stale-validator")
		}
		w.Header().Set("Trailer", "Digest, Content-Digest")
		compressor := gzip.NewWriter(w)
		_, _ = io.WriteString(compressor, original)
		_ = compressor.Close()
		w.Header().Set("Digest", "stale-trailer-validator")
		w.Header().Set("Content-Digest", "stale-trailer-validator")
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	relay := newAndroidBackendProxy(target, upstream.Client().Transport, "ws://127.0.0.1:8443/uni.ws")
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8443/android-backend/get_sync_info?device_id=fixture", nil)
	request.RemoteAddr = "127.0.0.1:50000"
	request.Header.Set("Authorization", "OAuth fixture-secret")
	request.Header.Set("Accept-Encoding", "untrusted-encoding")
	response := httptest.NewRecorder()
	relay.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !authorized.Load() {
		t.Fatalf("sync relay failed: HTTP%d authorized=%v body=%s", response.Code, authorized.Load(), response.Body)
	}
	wantJSON, err := jsontext.AppendFormat(nil, strings.Replace(original, "wss://uniproxy.alice.yandex.net/uni.ws", "ws://127.0.0.1:8443/uni.ws", 1), jsontext.ReorderRawObjects(true))
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := jsontext.AppendFormat(nil, response.Body.Bytes(), jsontext.ReorderRawObjects(true))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatalf("relay changed fields outside the speech endpoint: %s", response.Body)
	}
	for _, name := range []string{"ETag", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Content-Encoding"} {
		if response.Header().Get(name) != "" || response.Result().Trailer.Get(name) != "" {
			t.Errorf("stale %s retained", name)
		}
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("rewritten configuration can be cached")
	}
}

func TestAndroidBackendPassesOtherNativeRequests(t *testing.T) {
	const body = "opaque\x00native request"
	var forwarded atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		forwarded.Store(r.Method == http.MethodPost && r.URL.RequestURI() == "/set_device_state?device_id=x%2Fy" && string(data) == body && r.Header.Get("Authorization") == "OAuth fixture-secret")
		w.Header().Set("ETag", "unchanged")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/android-backend/set_device_state?device_id=x%2Fy", strings.NewReader(body))
	request.RemoteAddr = "[::1]:50000"
	request.Header.Set("Authorization", "OAuth fixture-secret")
	response := httptest.NewRecorder()
	newAndroidBackendProxy(target, upstream.Client().Transport, "ws://127.0.0.1:8443/uni.ws").ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Body.String() != body || response.Header().Get("ETag") != "unchanged" || !forwarded.Load() {
		t.Fatalf("other native route changed: HTTP%d body=%q forwarded=%v", response.Code, response.Body.String(), forwarded.Load())
	}
}

func TestAndroidBackendRejectsUnsafeSyncReplies(t *testing.T) {
	for _, body := range []string{`{private-payload-secret`, `[]`, `{"status":"ok","config":[]}`, `{"status":"ok","config":{"system_config":[]}}`, `{"status":"ok","config":{"system_config":null}}`, strings.Repeat("x", (2<<20)+1)} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		target, _ := url.Parse(upstream.URL)
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/android-backend/get_sync_info", nil)
		request.RemoteAddr = "127.0.0.1:50000"
		newAndroidBackendProxy(target, upstream.Client().Transport, "ws://127.0.0.1:8443/uni.ws").ServeHTTP(response, request)
		upstream.Close()
		if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "private-payload-secret") || response.Body.Len() > 200 {
			t.Fatalf("unsafe sync response was accepted or exposed: HTTP%d body=%s", response.Code, response.Body)
		}
	}
}

func TestAndroidBackendPreservesUnchangedSyncReplies(t *testing.T) {
	for _, body := range []string{`{"status":"ok"}`, `{"status":"ok","config":null}`, `{"status":"ok","config":{}}`, `{"status":"ok","config":{"system_config":{}}}`, `{"status":"error","message":"native error"}`} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		target, _ := url.Parse(upstream.URL)
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/android-backend/get_sync_info", nil)
		request.RemoteAddr = "127.0.0.1:50000"
		newAndroidBackendProxy(target, upstream.Client().Transport, "ws://127.0.0.1:8443/uni.ws").ServeHTTP(response, request)
		upstream.Close()
		if response.Code != http.StatusOK || response.Body.String() != body {
			t.Fatalf("unchanged sync reply rejected or mutated: HTTP%d body=%s", response.Code, response.Body)
		}
	}
}

func TestAndroidBackendRejectsLANAndRedirects(t *testing.T) {
	var relayCalls, escapedCalls atomic.Uint32
	escape := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { escapedCalls.Add(1) }))
	defer escape.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayCalls.Add(1)
		w.Header().Set("Location", escape.URL+"/private-redirect-secret")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	relay := newAndroidBackendProxy(target, upstream.Client().Transport, "ws://127.0.0.1:8443/uni.ws")
	for _, peer := range []string{"192.0.2.20:50000", "localhost:50000", "malformed"} {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/android-backend/get_sync_info", nil)
		request.RemoteAddr = peer
		response := httptest.NewRecorder()
		relay.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("non-loopback peer accepted: %q HTTP%d", peer, response.Code)
		}
	}
	if relayCalls.Load() != 0 {
		t.Fatal("denied requests reached the backend")
	}
	server := httptest.NewServer(relay)
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/android-backend/get_sync_info", nil)
	request.Header.Set("Authorization", "OAuth fixture-secret")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadGateway || response.Header.Get("Location") != "" || escapedCalls.Load() != 0 || strings.Contains(string(data), "private-redirect-secret") {
		t.Fatalf("redirect escaped pinned origin: HTTP%d escaped=%d body=%s", response.StatusCode, escapedCalls.Load(), data)
	}
}

func TestAndroidBackendKeepsTLSVerification(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "private-tls-secret") }))
	upstream.Config.ErrorLog = log.New(io.Discard, "", 0)
	upstream.StartTLS()
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/android-backend/get_sync_info", nil)
	request.RemoteAddr = "127.0.0.1:50000"
	response := httptest.NewRecorder()
	newAndroidBackendProxy(target, http.DefaultTransport, "ws://127.0.0.1:8443/uni.ws").ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "private-tls-secret") {
		t.Fatalf("untrusted TLS was accepted or exposed: HTTP%d body=%s", response.Code, response.Body)
	}
}

func TestAndroidBackendInvalidOptInProcess(t *testing.T) {
	if path := os.Getenv("QUASAR_TEST_ANDROID_BACKEND_CONFIG"); path != "" {
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", path, "-check-config"}
		main()
		os.Exit(0)
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAndroidBackendInvalidOptInProcess$")
	command.Env = append(os.Environ(), "QUASAR_TEST_ANDROID_BACKEND_CONFIG="+configFile(t, `{}`), "QUASAR_ANDROID_BACKEND_PROXY=private-optin-secret")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "QUASAR_ANDROID_BACKEND_PROXY") || strings.Contains(string(output), "private-optin-secret") {
		t.Fatalf("invalid opt-in accepted or leaked: %v %s", err, output)
	}
}
