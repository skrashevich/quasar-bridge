package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/skrashevich/quasar-bridge/internal/bridge"
)

// The SSH installer records its client's IP in service-owned state. Opening
// diagnostics to the LAN alone must not grant permission to define programs.
func (c *configurationControl) trustedCommandClient(peer netip.Addr) bool {
	if !peer.IsValid() || peer.IsLoopback() {
		return false
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(c.path), "command-editor.clients"))
	if err != nil {
		return false
	}
	for line := range strings.Lines(string(data)) {
		if ip, err := netip.ParseAddr(strings.TrimSpace(line)); err == nil && ip.Unmap() == peer {
			return true
		}
	}
	return false
}

func (c *configurationControl) commandEditor() http.HandlerFunc {
	secret := []byte(rand.Text())
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		if c == nil {
			writeDiagnosticError(w, http.StatusServiceUnavailable, "Редактор команд недоступен.")
			return
		}
		peer, local := diagnosticClient(r)
		if !local && !c.trustedCommandClient(peer) {
			writeDiagnosticError(w, http.StatusForbidden, "Откройте редактор с компьютера, с которого установили Bridge по SSH, или через SSH-туннель.")
			return
		}
		sites := r.Header.Values("Sec-Fetch-Site")
		if len(sites) > 0 && (len(sites) != 1 || sites[0] != "same-origin") {
			writeDiagnosticError(w, http.StatusForbidden, "Откройте редактор из веб-панели Bridge.")
			return
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(peer.String() + "\x00" + diagnosticOrigin(r)))
		token := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		if r.Method != http.MethodPost {
			value, err := c.commandForPanel(r.URL.Query().Get("id"), token)
			writeDiagnosticResult(w, value, err)
			return
		}
		tokens := r.Header.Values("X-Quasar-CSRF")
		if r.Header.Get("Origin") != diagnosticOrigin(r) || len(tokens) != 1 || !hmac.Equal([]byte(tokens[0]), []byte(token)) {
			writeDiagnosticError(w, http.StatusForbidden, "Откройте редактор заново и повторите сохранение.")
			return
		}
		var edit commandEdit
		if readDiagnosticJSON(w, r, &edit) {
			value, err := c.editCommand(r.Context(), edit)
			writeDiagnosticResult(w, value, err)
		}
	}
}

type commandEdit struct {
	Action           string                 `json:"action"`
	ID               string                 `json:"id"`
	OriginalID       string                 `json:"original_id,omitempty"`
	Command          *bridge.RoutingCommand `json:"command,omitempty"`
	Description      *string                `json:"description,omitempty"`
	ExpectedRevision string                 `json:"expected_revision"`
}

func (c *configurationControl) commandForPanel(id, csrfToken string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.active.Routing
	view := struct {
		Revision    string                 `json:"revision"`
		CSRFToken   string                 `json:"csrf_token"`
		Command     *bridge.RoutingCommand `json:"command,omitempty"`
		Description string                 `json:"description,omitempty"`
	}{Revision: c.engine.RoutingStatus().Revision, CSRFToken: csrfToken}
	if id != "" {
		command, ok := r.Commands[id]
		if !ok {
			return nil, errors.New("Команда отсутствует. Обновите список.")
		}
		view.Command, view.Description = &command, r.Classifier.Commands[id]
	}
	return view, nil
}

func (c *configurationControl) editCommand(ctx context.Context, edit commandEdit) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg, raw, err := c.routingForEdit(ctx, edit.ExpectedRevision)
	if err != nil {
		return nil, err
	}
	r := &cfg.Routing
	if edit.ID == "" {
		return nil, errors.New("Укажите ID команды.")
	}
	_, exists := r.Commands[edit.ID]
	switch edit.Action {
	case "create":
		if edit.OriginalID != "" || exists {
			return nil, errors.New("Команда с таким ID уже есть. Укажите другой ID.")
		}
	case "update":
		if _, ok := r.Commands[edit.OriginalID]; !ok {
			return nil, errors.New("Исходная команда отсутствует. Обновите список.")
		}
		if edit.OriginalID != edit.ID && exists {
			return nil, errors.New("Новый ID уже занят другой командой.")
		}
	case "delete":
		if !exists || edit.Command != nil || edit.OriginalID != "" || edit.Description != nil {
			return nil, errors.New("Укажите существующую команду для удаления.")
		}
		for _, rule := range r.Rules {
			if rule.Command == edit.ID {
				return nil, fmt.Errorf("Сначала измените правило «%s», которое использует эту команду.", rule.Name)
			}
		}
	default:
		return nil, errors.New("Неизвестное действие с командой.")
	}
	changes := map[string]jsontext.Value{}
	if edit.Action == "delete" {
		delete(r.Commands, edit.ID)
		delete(r.Classifier.Commands, edit.ID)
	} else {
		if edit.Command == nil {
			return nil, errors.New("Укажите программу для запуска.")
		}
		if len(edit.Command.Args) == 0 {
			edit.Command.Args = nil
		}
		if r.Commands == nil {
			r.Commands = map[string]bridge.RoutingCommand{}
		}
		if edit.Action == "update" && edit.OriginalID != edit.ID {
			delete(r.Commands, edit.OriginalID)
			if description, ok := r.Classifier.Commands[edit.OriginalID]; ok {
				r.Classifier.Commands[edit.ID] = description
				delete(r.Classifier.Commands, edit.OriginalID)
			}
			rulesChanged := false
			for i, rule := range r.Rules {
				if rule.Command == edit.OriginalID {
					r.Rules[i].Command = edit.ID
					rulesChanged = true
				}
			}
			if rulesChanged {
				changes["routing.rules"], _ = json.Marshal(r.Rules)
			}
		}
		r.Commands[edit.ID] = *edit.Command
		if edit.Description != nil {
			if description := strings.TrimSpace(*edit.Description); description != "" {
				if r.Classifier.Commands == nil {
					r.Classifier.Commands = map[string]string{}
				}
				r.Classifier.Commands[edit.ID] = description
			} else {
				delete(r.Classifier.Commands, edit.ID)
			}
		}
	}
	if len(r.Commands) == 0 {
		r.Commands = nil
	}
	if len(r.Classifier.Commands) == 0 {
		r.Classifier.Commands = nil
	}
	changes["routing.commands"], _ = json.Marshal(r.Commands)
	if !maps.Equal(c.active.Routing.Classifier.Commands, r.Classifier.Commands) {
		changes["routing.classifier.commands"], _ = json.Marshal(r.Classifier.Commands)
	}
	return c.applyRoutingChangesLocked(ctx, cfg, raw, changes)
}
