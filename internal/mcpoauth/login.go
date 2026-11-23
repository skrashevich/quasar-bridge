package mcpoauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type Login struct {
	*Attempt
	server   *http.Server
	listener net.Listener
}

func NewLogin(b Binding, listen string, out io.Writer, upstream http.RoundTripper) (*Login, error) {
	address, err := loopbackListen(listen)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, errors.New("cannot bind the MCP OAuth callback port")
	}
	attempt, err := NewAttempt(b, "http://"+listener.Addr().String()+callbackPath, func(_ context.Context, link string) error {
		_, err := fmt.Fprintf(out, "Open this link in your browser:\n%s\nWaiting for MCP OAuth sign-in…\n", link)
		return err
	}, upstream)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	l := &Login{Attempt: attempt, listener: listener}
	l.server = &http.Server{
		Handler: http.HandlerFunc(l.callback), ReadHeaderTimeout: 3 * time.Second,
		WriteTimeout: 3 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192,
	}
	go func() { _ = l.server.Serve(listener) }()
	return l, nil
}

func (l *Login) Close() error {
	// Submitting a return wakes the exchange goroutine before the callback
	// handler finishes its response. Let that response flush before closing.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := l.server.Shutdown(ctx)
	_ = l.listener.Close()
	if err != nil {
		_ = l.server.Close()
	}
	return err
}

func (l *Login) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodGet || r.URL.EscapedPath() != callbackPath || r.Host != l.listener.Addr().String() {
		http.NotFound(w, r)
		return
	}
	outcome, err := l.Submit(l.redirect + "?" + r.URL.RawQuery)
	switch {
	case errors.Is(err, ErrCallbackUsed):
		http.Error(w, "OAuth callback already received", http.StatusConflict)
	case err != nil:
		http.Error(w, "Invalid OAuth callback", http.StatusBadRequest)
	case outcome == CallbackDenied:
		http.Error(w, "MCP OAuth sign-in was not completed", http.StatusBadRequest)
	default:
		_, _ = io.WriteString(w, "Sign-in response received. Check the Quasar Bridge terminal.")
	}
}
