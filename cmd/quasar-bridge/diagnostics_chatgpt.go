package main

import (
	_ "embed"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"quasarbridge/internal/chatgpt"
)

// Both panel listeners share pending attempts. Only the browser that received
// the authorization URL knows the state. Rejected links cannot consume a login.
type panelChatGPT struct {
	store   chatgpt.Store
	mu      sync.Mutex
	pending []panelLogin
}

type panelLogin struct {
	attempt *chatgpt.LoginAttempt
	expires time.Time
}

//go:embed diagnostics_callback.html
var callbackHTML string

//go:embed diagnostics_callback.js
var callbackJS string

func panelCallbackPort(r *http.Request) (uint16, bool) {
	host := &url.URL{Host: r.Host}
	if r.TLS == nil && (host.Hostname() == "127.0.0.1" || host.Hostname() == "localhost") {
		if host.Port() == "" {
			return 80, true
		}
		if port, err := strconv.ParseUint(host.Port(), 10, 16); err == nil && port > 0 {
			return uint16(port), true
		}
	}
	// OpenAI returns to the user's computer. A remote panel can accept the
	// resulting address using its same-origin JSON endpoint.
	return 1455, false
}

func (p *panelChatGPT) register(mux *http.ServeMux, register func(string, http.HandlerFunc)) {
	register("GET /diagnostics/api/chatgpt", func(w http.ResponseWriter, r *http.Request) {
		status, err := p.store.Status(r.Context())
		if !status.SignedIn {
			status.Email = ""
		}
		writeDiagnosticResult(w, map[string]any{"signed_in": status.SignedIn, "email": status.Email}, err)
	})
	register("POST /diagnostics/api/chatgpt/start", p.start)
	register("POST /diagnostics/api/chatgpt/complete", p.complete)
	register("POST /diagnostics/api/chatgpt/logout", func(w http.ResponseWriter, r *http.Request) {
		if !readDiagnosticJSON(w, r, &struct{}{}) {
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		p.pending = nil
		writeDiagnosticResult(w, struct{}{}, p.store.Logout(r.Context()))
	})
	// The provider navigates across origins to this static landing page.
	// It reads no state and performs no mutation. The actual exchange uses
	// the regular guarded, same-origin POST endpoint.
	mux.HandleFunc("GET /auth/callback", func(w http.ResponseWriter, r *http.Request) {
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !peer.Addr().Unmap().IsLoopback() || (&url.URL{Host: r.Host}).Hostname() != "127.0.0.1" {
			http.Error(w, "Loopback callback required", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(callbackHTML))
	})
}

func (p *panelChatGPT) start(w http.ResponseWriter, r *http.Request) {
	if !readDiagnosticJSON(w, r, &struct{}{}) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.pending = slices.DeleteFunc(p.pending, func(login panelLogin) bool { return !now.Before(login.expires) })
	if len(p.pending) >= 8 {
		writeDiagnosticError(w, http.StatusTooManyRequests, "Слишком много попыток входа. Повторите через 10 минут.")
		return
	}
	port, automatic := panelCallbackPort(r)
	attempt, err := p.store.BeginLogin(r.Context(), port)
	if err != nil {
		writeDiagnosticResult(w, nil, err)
		return
	}
	p.pending = append(p.pending, panelLogin{attempt, now.Add(10 * time.Minute)})
	writeDiagnosticJSON(w, http.StatusOK, map[string]any{"authorization_url": attempt.AuthorizationURL(), "automatic_return": automatic})
}

func (p *panelChatGPT) complete(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Callback string `json:"callback"`
	}
	if !readDiagnosticJSON(w, r, &request) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, login := range p.pending {
		if time.Now().Before(login.expires) && login.attempt.Matches(request.Callback) {
			p.pending = slices.Delete(p.pending, i, i+1)
			err := login.attempt.Complete(r.Context(), request.Callback)
			if err != nil {
				reason := err.Error()
				switch {
				case errors.Is(err, chatgpt.ErrLoginDenied):
					reason = "Вход отменён в ChatGPT."
				case errors.Is(err, chatgpt.ErrPlanPermission):
					reason = "Разрешите использование подписки в настройках ChatGPT."
				}
				writeDiagnosticError(w, http.StatusUnprocessableEntity, reason+" Чтобы повторить, начните вход заново.")
				return
			}
			p.pending = nil
			writeDiagnosticJSON(w, http.StatusOK, struct{}{})
			return
		}
	}
	writeDiagnosticError(w, http.StatusBadRequest, "Ссылка не соответствует активной попытке входа. Проверьте адрес или начните вход заново.")
}
