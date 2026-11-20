package main

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestDiagnosticsListenerRebindsWithoutClosingActiveRequests(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(finish) })
	defer release()
	m := newDiagnosticsListener(0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-finish
		_, _ = w.Write([]byte("applied"))
	}))
	if err := m.SetNetworkAccess(false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = m.Shutdown(ctx)
	})
	if host, _, _ := net.SplitHostPort(m.Address()); host != "127.0.0.1" {
		t.Fatalf("default listener is not loopback: %s", m.Address())
	}
	result := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://" + m.Address())
		if err == nil {
			_ = resp.Body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("applying request did not reach the listener")
	}
	if err := m.SetNetworkAccess(true); err != nil {
		t.Fatal(err)
	}
	if host, _, _ := net.SplitHostPort(m.Address()); host != "0.0.0.0" {
		t.Fatalf("network listener not enabled: %s", m.Address())
	}
	release()
	if err := <-result; err != nil {
		t.Fatalf("enabling network access interrupted the applying request: %v", err)
	}
	if err := m.SetNetworkAccess(false); err != nil {
		t.Fatal(err)
	}
	if host, _, _ := net.SplitHostPort(m.Address()); host != "127.0.0.1" {
		t.Fatalf("loopback access not restored: %s", m.Address())
	}
}

func TestDiagnosticsListenerReportsPortConflict(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	m := newDiagnosticsListener(occupied.Addr().(*net.TCPAddr).Port, http.NotFoundHandler())
	if err := m.SetNetworkAccess(false); err == nil {
		t.Fatal("port conflict was silently ignored")
	}
	if m.Address() != "" {
		t.Fatalf("failed listener advertised an address: %s", m.Address())
	}
}

func TestDiagnosticsPortNeverPreemptsBridgeListener(t *testing.T) {
	available, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := available.Addr().(*net.TCPAddr).Port
	_ = available.Close()
	server := &http.Server{Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })}
	panel := newDiagnosticsListener(port, http.NotFoundHandler())
	result := make(chan error, 1)
	go func() { result <- serveBridgeHTTPWithDiagnostics(server, "", "", func() {}, panel, false) }()
	t.Cleanup(func() { _ = server.Close() })
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, requestErr := client.Get("http://" + server.Addr + "/healthz")
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode != 204 {
				t.Fatalf("diagnostics stole the main port: %d", response.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("primary listener never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if panel.Address() != "" {
		t.Fatalf("panel claimed the main listener address: %s", panel.Address())
	}
	_ = server.Close()
	if err := <-result; err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
