package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"quasarbridge/internal/wire"
)

func TestDiagnosticLogBoundsAndIncrementalReads(t *testing.T) {
	l := newDiagnosticLog(16)
	_, _ = l.Write([]byte("one\n"))
	first := l.snapshot(0)
	if first.Text != "one\n" || first.Cursor != 4 {
		t.Fatalf("first snapshot: %+v", first)
	}
	_, _ = l.Write([]byte("two\n"))
	if got := l.snapshot(first.Cursor); got.Text != "two\n" || got.Reset {
		t.Fatalf("incremental snapshot: %+v", got)
	}
	_, _ = l.Write([]byte("three\nfour\nfive\n"))
	if got := l.snapshot(0); got.Text != "three\nfour\nfive\n" || !got.Reset || got.Cursor != 24 {
		t.Fatalf("bounded snapshot: %+v", got)
	}
	if got := l.snapshot(25); !got.Reset || got.Text != "three\nfour\nfive\n" {
		t.Fatalf("stale cursor after restart: %+v", got)
	}
	_, _ = l.Write([]byte("a line longer than the entire buffer\nlast\n"))
	if got := l.snapshot(0); len(got.Text) > 16 || !strings.HasSuffix(got.Text, "last\n") {
		t.Fatalf("oversized write escaped bound: %+v", got)
	}
}

func TestDiagnosticPacketEviction(t *testing.T) {
	var p diagnosticPackets
	raw := bytes.Repeat([]byte{'x'}, packetBytesLimit/2)
	first := p.add(raw, wire.ClientToServer)
	raw[0] = 'y'
	second := p.add(raw, wire.ServerToClient)
	if pkt, ok := p.get(first); !ok || pkt.data[0] != 'x' {
		t.Fatal("retained packet aliases the caller's buffer")
	}
	if id := p.add(make([]byte, packetBytesLimit+1), wire.ClientToServer); id != 0 {
		t.Fatal("oversized packet retained")
	}
	p.add([]byte("new"), wire.ClientToServer)
	if _, ok := p.get(first); ok {
		t.Fatal("byte limit did not evict the oldest packet")
	}
	if pkt, ok := p.get(second); !ok || pkt.direction != wire.ServerToClient || pkt.data[0] != 'y' {
		t.Fatal("incorrect packet after byte eviction")
	}
	for range len(p.entries) {
		p.add([]byte{1}, wire.ServerToClient)
	}
	if _, ok := p.get(second); ok || p.size != len(p.entries) {
		t.Fatal("entry limit failed to evict small packets")
	}
}

func TestDiagnosticPacketEndpoint(t *testing.T) {
	l := newDiagnosticLog(1024)
	// Server StreamControl with MessageId "private" in an AAPI envelope.
	raw := append([]byte("AAPI"), []byte{0x12, 9, 0x12, 7, 'p', 'r', 'i', 'v', 'a', 't', 'e'}...)
	id := l.packets.add(raw, wire.ServerToClient)
	slog.New(slog.NewJSONHandler(l, nil)).Info("frame", "event", "StreamControl", "packet_id", id)
	mux := http.NewServeMux()
	registerDiagnostics(mux, l, func() map[string]any { return nil })
	for _, path := range []string{"/diagnostics/logs", "/diagnostics/download"} {
		res := diagnosticRequest(mux, path)
		if strings.Contains(res.Body.String(), "private") || !strings.Contains(res.Body.String(), "packet_id") {
			t.Fatalf("bad summary for %s: %s", path, res.Body)
		}
	}
	path := "/diagnostics/logs/" + strconv.FormatUint(id, 10)
	res := diagnosticRequest(mux, path+"?run="+l.run)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"StreamControl":{"MessageId":"private"}`) || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("bad details: %d %s", res.Code, res.Body)
	}
	for _, old := range []string{path, path + "?run=previous", "/diagnostics/logs/99?run=" + l.run} {
		if res := diagnosticRequest(mux, old); res.Code != http.StatusGone {
			t.Fatalf("%s: got %d, want 410", old, res.Code)
		}
	}
}

func TestDiagnosticLogConcurrentWriters(t *testing.T) {
	l := newDiagnosticLog(1024)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				_, _ = l.Write([]byte("entry\n"))
				_ = l.snapshot(0)
			}
		})
	}
	wg.Wait()
	got := l.snapshot(0)
	if len(got.Text) > 1024 || got.Cursor != 4800 || !got.Reset {
		t.Fatalf("concurrent snapshot: %+v", got)
	}
}

func diagnosticRequest(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://localhost:18443"+path, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestDiagnosticsExposeLoggerAndLiveStateWithoutReadingConfig(t *testing.T) {
	l := newDiagnosticLog(1024)
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(io.Discard, l), nil))
	logger.Info("RTSP SETUP parameters timing=PTP")
	connected := false
	mux := http.NewServeMux()
	registerDiagnostics(mux, l, func() map[string]any {
		return map[string]any{"connected": connected, "state": "running"}
	})
	response := diagnosticRequest(mux, "/diagnostics/logs")
	var first diagnosticSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil || response.Code != 200 {
		t.Fatalf("response=%s error=%v", response.Body, err)
	}
	if !strings.Contains(first.Text, "RTSP SETUP parameters") || first.AirPlay["connected"] != false || first.Run == "" {
		t.Fatalf("missing log or live state: %+v", first)
	}
	connected = true
	logger.Info("RTSP response status=200")
	response = diagnosticRequest(mux, "/diagnostics/logs?run="+first.Run+"&after="+strconv.FormatInt(first.Cursor, 10))
	var next diagnosticSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(next.Text, "SETUP parameters") || !strings.Contains(next.Text, "response status=200") || next.AirPlay["connected"] != true {
		t.Fatalf("incremental response or live status lost: %+v", next)
	}
	response = diagnosticRequest(mux, "/diagnostics/logs?run=previous-process&after="+strconv.FormatInt(next.Cursor, 10))
	if !strings.Contains(response.Body.String(), "SETUP parameters") {
		t.Fatalf("new process did not reset cursor: %s", response.Body)
	}
	response = diagnosticRequest(mux, "/diagnostics/download")
	if response.Code != 200 || !strings.Contains(response.Header().Get("Content-Disposition"), ".txt") || !strings.Contains(response.Body.String(), "RTSP SETUP parameters") || !strings.Contains(response.Body.String(), `"connected":true`) {
		t.Fatalf("download is missing logs or metadata: %s %s", response.Header(), response.Body)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("diagnostics may be cached")
	}
}

func TestDiagnosticsRequireLoopbackAndLocalBrowserHost(t *testing.T) {
	mux := http.NewServeMux()
	registerDiagnostics(mux, newDiagnosticLog(1024), func() map[string]any { return nil })
	for _, path := range []string{"/diagnostics/", "/diagnostics/logs", "/diagnostics/logs/1?run=test", "/diagnostics/download", "/diagnostics/app.js"} {
		for _, tc := range []struct{ remote, host, fetchSite string }{
			{"192.0.2.2:12345", "localhost:18443", ""},
			{"127.0.0.1:12345", "attacker.example:18443", ""},
			{"127.0.0.1:12345", "localhost:18443", "cross-site"},
		} {
			req := httptest.NewRequest("GET", "http://"+tc.host+path, nil)
			req.RemoteAddr = tc.remote
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != 403 {
				t.Errorf("%s %+v: got %d", path, tc, w.Code)
			}
		}
	}
	for _, host := range []string{"localhost:18443", "127.0.0.1:18443", "[::1]:18443"} {
		req := httptest.NewRequest("GET", "http://"+host+"/diagnostics/", nil)
		req.RemoteAddr = "[::1]:12345"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("loopback %s rejected: %d", host, w.Code)
		}
	}
}
