package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// A separate listener keeps panel access independent of the speaker's local
// UniProxy listener and its allowed_ips. Closing a listening socket leaves the
// HTTP request applying the change alive long enough to receive its result.
type diagnosticsListener struct {
	mu       sync.Mutex
	port     int
	network  bool
	listener net.Listener
	server   *http.Server
	closed   bool
}

func newDiagnosticsListener(port int, handler http.Handler) *diagnosticsListener {
	return &diagnosticsListener{port: port, server: &http.Server{
		Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
	}}
}

func (d *diagnosticsListener) Address() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listener == nil {
		return ""
	}
	return d.listener.Addr().String()
}

func (d *diagnosticsListener) bind(network bool) (net.Listener, error) {
	host := "127.0.0.1"
	if network {
		host = "0.0.0.0"
	}
	return net.Listen("tcp4", net.JoinHostPort(host, strconv.Itoa(d.port)))
}

func (d *diagnosticsListener) serve(listener net.Listener) {
	go func() {
		if err := d.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			slog.Error("diagnostics listener failed")
		}
	}()
}

func (d *diagnosticsListener) SetNetworkAccess(network bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errors.New("diagnostics listener is shut down")
	}
	if d.listener != nil && d.network == network {
		return nil
	}
	hadListener := d.listener != nil
	if hadListener {
		_ = d.listener.Close()
		d.listener = nil
	}
	listener, err := d.bind(network)
	if err != nil {
		if hadListener {
			previous, restoreErr := d.bind(d.network)
			if restoreErr != nil {
				return errors.New("cannot bind diagnostics listener or restore previous address; restart Bridge")
			}
			d.listener = previous
			d.serve(previous)
		}
		return errors.New("cannot bind diagnostics port; check whether it is already in use")
	}
	d.listener, d.network = listener, network
	// Port zero is used by integration tests; retain the allocated port on rebind.
	d.port = listener.Addr().(*net.TCPAddr).Port
	d.serve(listener)
	return nil
}

func (d *diagnosticsListener) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	d.closed = true
	if d.listener != nil {
		_ = d.listener.Close()
		d.listener = nil
	}
	d.mu.Unlock()
	return d.server.Shutdown(ctx)
}
