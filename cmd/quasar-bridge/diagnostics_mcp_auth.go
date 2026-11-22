package main

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/mcpoauth"
)

var errPanelMCPAttemptGone = errors.New("MCP OAuth attempt is no longer current")

const maxPanelMCPAttempts = 8

type panelMCPAuth struct {
	ctx     context.Context
	cancel  context.CancelFunc
	store   *mcpoauth.Store
	ttl     time.Duration
	names   []string
	servers map[string]*panelMCPServer
	life    sync.Mutex
	jobs    sync.WaitGroup
	pending atomic.Int32
}

type panelMCPServer struct {
	cfg     assistant.MCPServerConfig
	mu      sync.Mutex
	current *panelMCPAttempt
}

type panelMCPAttempt struct {
	id      string
	ctx     context.Context
	cancel  context.CancelFunc
	attempt *mcpoauth.Attempt
	done    chan struct{}
	err     error // written before done closes
}

func newPanelMCPAuth(ctx context.Context, configs []assistant.MCPServerConfig, store *mcpoauth.Store) *panelMCPAuth {
	ctx, cancel := context.WithCancel(ctx)
	p := &panelMCPAuth{ctx: ctx, cancel: cancel, store: store, ttl: 10 * time.Minute, servers: make(map[string]*panelMCPServer)}
	for _, c := range configs {
		if c.OAuth != nil {
			p.servers[c.Name] = &panelMCPServer{cfg: c}
			p.names = append(p.names, c.Name)
		}
	}
	return p
}

func (p *panelMCPAuth) close() {
	p.life.Lock()
	p.cancel()
	p.life.Unlock()
	p.jobs.Wait()
}

func (p *panelMCPAuth) register(register func(string, http.HandlerFunc)) {
	const base = "/diagnostics/api/mcp-auth"
	register("GET "+base, p.list)
	register("POST "+base+"/start", p.start)
	register("POST "+base+"/complete", p.complete)
	register("POST "+base+"/cancel", p.cancelAttempt)
	register("POST "+base+"/logout", p.logout)
}

func (p *panelMCPAuth) status(s *panelMCPServer) string {
	if p.store != nil {
		if status, err := assistant.MCPOAuthStatus(p.store, s.cfg); err == nil {
			return status
		}
	}
	return "unavailable"
}

func (p *panelMCPAuth) list(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Name     string `json:"name"`
		Disabled bool   `json:"disabled"`
		Status   string `json:"status"`
	}
	out := make([]entry, 0, len(p.names))
	for _, name := range p.names {
		s := p.servers[name]
		out = append(out, entry{name, s.cfg.Disabled, p.status(s)})
	}
	writeDiagnosticJSON(w, http.StatusOK, map[string]any{"servers": out})
}

func (p *panelMCPAuth) server(w http.ResponseWriter, name string) *panelMCPServer {
	s := p.servers[name]
	if s == nil {
		writeDiagnosticError(w, http.StatusNotFound, "OAuth MCP-сервер не найден.")
	}
	return s
}

func (p *panelMCPAuth) start(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Server string `json:"server"`
	}
	if !readDiagnosticJSON(w, r, &in) {
		return
	}
	s := p.server(w, in.Server)
	if s == nil {
		return
	}
	if s.cfg.Disabled {
		writeDiagnosticError(w, http.StatusConflict, "MCP-сервер отключён.")
		return
	}
	if p.store == nil {
		writeDiagnosticError(w, http.StatusServiceUnavailable, "Хранилище MCP OAuth недоступно.")
		return
	}
	links := make(chan string, 1)
	attempt, err := assistant.NewMCPPanelAttempt(s.cfg, func(ctx context.Context, link string) error {
		select {
		case links <- link:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		writeDiagnosticError(w, http.StatusUnprocessableEntity, "Не удалось подготовить вход в MCP.")
		return
	}
	s.mu.Lock()
	p.life.Lock()
	if p.ctx.Err() != nil {
		p.life.Unlock()
		s.mu.Unlock()
		writeDiagnosticError(w, http.StatusServiceUnavailable, "Bridge завершает работу.")
		return
	}
	if s.current == nil {
		if p.pending.Load() >= maxPanelMCPAttempts {
			p.life.Unlock()
			s.mu.Unlock()
			writeDiagnosticError(w, http.StatusTooManyRequests, "Слишком много попыток входа. Отмените одну из них или дождитесь её завершения.")
			return
		}
		p.pending.Add(1)
	} else {
		s.current.cancel()
	}
	p.jobs.Add(1)
	p.life.Unlock()
	ctx, cancel := context.WithTimeout(p.ctx, p.ttl)
	a := &panelMCPAttempt{id: rand.Text(), ctx: ctx, cancel: cancel, attempt: attempt, done: make(chan struct{})}
	s.current = a
	s.mu.Unlock()
	go p.exchange(s, a)
	wait, stop := context.WithTimeout(r.Context(), 15*time.Second)
	defer stop()
	select {
	case link := <-links:
		if wait.Err() != nil {
			p.invalidate(s, a)
			writeDiagnosticError(w, http.StatusGatewayTimeout, "Bridge не успел подготовить вход. Начните заново.")
			return
		}
		s.mu.Lock()
		current := s.current == a && a.ctx.Err() == nil
		s.mu.Unlock()
		if !current {
			writePanelMCPAttemptGone(w)
			return
		}
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(3 * time.Second))
		defer controller.SetWriteDeadline(time.Time{})
		writeErr := writeDiagnosticJSON(w, http.StatusOK, map[string]string{
			"attempt_id": a.id, "authorization_url": link, "redirect_uri": mcpoauth.ManualRedirect,
		})
		if writeErr != nil || controller.Flush() != nil || r.Context().Err() != nil {
			p.invalidate(s, a)
		}
	case <-a.done:
		writePanelMCPResult(w, a.err, "")
	case <-wait.Done():
		p.invalidate(s, a)
		writeDiagnosticError(w, http.StatusGatewayTimeout, "Bridge не успел подготовить вход. Начните заново.")
	}
}

func (p *panelMCPAuth) exchange(s *panelMCPServer, a *panelMCPAttempt) {
	defer p.jobs.Done()
	defer close(a.done)
	defer a.cancel()
	record, err := assistant.ExchangeMCPLogin(a.ctx, s.cfg, a.attempt)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != a || a.ctx.Err() != nil {
		if s.current == a {
			p.clearLocked(s)
		}
		a.err = errPanelMCPAttemptGone
		return
	}
	if err == nil {
		save, stop := context.WithTimeout(a.ctx, 5*time.Second)
		defer stop()
		err = p.store.Save(save, record)
		if err != nil && a.ctx.Err() != nil {
			err = errPanelMCPAttemptGone
		}
	}
	p.clearLocked(s)
	a.err = err
}

func (p *panelMCPAuth) clearLocked(s *panelMCPServer) {
	if s.current != nil {
		s.current.cancel()
		s.current = nil
		p.pending.Add(-1)
	}
}

func (p *panelMCPAuth) invalidate(s *panelMCPServer, a *panelMCPAttempt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == a {
		p.clearLocked(s)
	}
}

func (p *panelMCPAuth) complete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Server   string `json:"server"`
		ID       string `json:"attempt_id"`
		Callback string `json:"callback"`
	}
	if !readDiagnosticJSON(w, r, &in) {
		return
	}
	s := p.server(w, in.Server)
	if s == nil {
		return
	}
	s.mu.Lock()
	a := s.current
	if a == nil || a.id != in.ID || a.ctx.Err() != nil {
		s.mu.Unlock()
		writePanelMCPAttemptGone(w)
		return
	}
	_, err := a.attempt.Submit(in.Callback)
	s.mu.Unlock()
	switch {
	case errors.Is(err, mcpoauth.ErrCallbackUsed):
		writePanelMCPAttemptGone(w)
		return
	case err != nil:
		writeDiagnosticError(w, http.StatusBadRequest, "Адрес возврата не соответствует этой попытке. Проверьте его и повторите.")
		return
	}
	wait, stop := context.WithTimeout(r.Context(), 40*time.Second)
	defer stop()
	select {
	case <-a.done:
		status := ""
		if a.err == nil {
			status = p.status(s)
		}
		writePanelMCPResult(w, a.err, status)
	case <-wait.Done():
		writeDiagnosticError(w, http.StatusGatewayTimeout, "Не удалось дождаться результата. Обновите статус, не отправляйте адрес повторно.")
	}
}

func (p *panelMCPAuth) cancelAttempt(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Server string `json:"server"`
		ID     string `json:"attempt_id"`
	}
	if !readDiagnosticJSON(w, r, &in) {
		return
	}
	s := p.server(w, in.Server)
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.current == nil || s.current.id != in.ID || s.current.ctx.Err() != nil {
		s.mu.Unlock()
		writePanelMCPAttemptGone(w)
		return
	}
	p.clearLocked(s)
	s.mu.Unlock()
	writeDiagnosticJSON(w, http.StatusOK, struct{}{})
}

func (p *panelMCPAuth) logout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Server string `json:"server"`
	}
	if !readDiagnosticJSON(w, r, &in) {
		return
	}
	s := p.server(w, in.Server)
	if s == nil {
		return
	}
	if p.store == nil {
		writeDiagnosticError(w, http.StatusServiceUnavailable, "Хранилище MCP OAuth недоступно.")
		return
	}
	s.mu.Lock()
	p.clearLocked(s)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	err := p.store.Delete(ctx, in.Server)
	cancel()
	s.mu.Unlock()
	if err != nil {
		writeDiagnosticError(w, http.StatusServiceUnavailable, "Не удалось удалить локальный доступ.")
		return
	}
	writeDiagnosticJSON(w, http.StatusOK, map[string]string{"status": "not signed in"})
}

func writePanelMCPAttemptGone(w http.ResponseWriter) {
	writeDiagnosticError(w, http.StatusConflict, "Попытка входа больше не действует. Начните вход заново.")
}

func writePanelMCPResult(w http.ResponseWriter, err error, status string) {
	switch {
	case errors.Is(err, errPanelMCPAttemptGone):
		writePanelMCPAttemptGone(w)
	case errors.Is(err, mcpoauth.ErrLoginDenied):
		writeDiagnosticError(w, http.StatusUnprocessableEntity, "Вход у провайдера отменён.")
	case err != nil || status == "":
		writeDiagnosticError(w, http.StatusUnprocessableEntity, "Не удалось завершить OAuth-вход или подключиться к MCP.")
	default:
		writeDiagnosticJSON(w, http.StatusOK, map[string]string{"status": status})
	}
}
