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
	"slices"
	"strings"

	"github.com/skrashevich/quasar-bridge/internal/bridge"
)

func (c *configurationControl) commandEditor() http.HandlerFunc {
	secret := []byte(rand.Text())
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		if c == nil {
			writeDiagnosticError(w, http.StatusServiceUnavailable, "Редактор команд недоступен.")
			return
		}
		sites := r.Header.Values("Sec-Fetch-Site")
		if len(sites) > 0 && (len(sites) != 1 || sites[0] != "same-origin") {
			writeDiagnosticError(w, http.StatusForbidden, "Откройте редактор из веб-панели Bridge.")
			return
		}
		token := commandFormToken(w, r, secret)
		if r.Method != http.MethodPost {
			value, err := c.commandForPanel(r.URL.Query().Get("id"), token)
			writeDiagnosticResult(w, value, err)
			return
		}
		tokens := r.Header.Values("X-Quasar-CSRF")
		if r.Header.Get("Origin") != diagnosticOrigin(r) || token == "" || len(tokens) != 1 || !hmac.Equal([]byte(tokens[0]), []byte(token)) {
			writeDiagnosticError(w, http.StatusForbidden, "Откройте редактор заново и повторите сохранение.")
			return
		}
		var form struct {
			commandEdit
			ChallengeID string `json:"challenge_id,omitempty"`
			Code        string `json:"code,omitempty"`
		}
		if !readDiagnosticJSON(w, r, &form) {
			return
		}
		if form.Action == "confirm" || form.Action == "cancel" {
			if form.commandEdit != (commandEdit{Action: form.Action}) || form.ChallengeID == "" || (form.Action == "cancel" && form.Code != "") {
				writeDiagnosticError(w, http.StatusBadRequest, "При подтверждении можно передать только номер запроса и код.")
				return
			}
			edit, err := c.approvals.finish(c.path, token, form.ChallengeID, form.Code, form.Action == "cancel")
			if err != nil {
				writeApprovalError(w, err)
				return
			}
			if form.Action == "cancel" {
				writeDiagnosticJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
				return
			}
			value, err := c.editCommand(r.Context(), edit, true)
			writeDiagnosticResult(w, value, err)
			return
		}
		if form.Code != "" || form.ChallengeID != "" {
			writeDiagnosticError(w, http.StatusBadRequest, "Подтвердите изменение отдельным запросом.")
			return
		}
		value, err := c.editCommand(r.Context(), form.commandEdit, false)
		if errors.Is(err, errCommandApprovalRequired) {
			challenge, err := c.approvals.begin(r.Context(), c.path, token, form.commandEdit)
			if err != nil {
				writeApprovalError(w, err)
				return
			}
			writeDiagnosticJSON(w, http.StatusAccepted, challenge)
			return
		}
		writeDiagnosticResult(w, value, err)
	}
}

// The cookie identifies a browser session only for CSRF. Its value stays out
// of JavaScript and the form token is valid only on the origin that issued it.
func commandFormToken(w http.ResponseWriter, r *http.Request, secret []byte) string {
	const name = "quasar_command_session"
	var session string
	cookies := r.CookiesNamed(name)
	if len(cookies) == 1 {
		session = cookies[0].Value
	}
	if session == "" {
		if r.Method == http.MethodPost {
			return ""
		}
		session = rand.Text()
		http.SetCookie(w, &http.Cookie{Name: name, Value: session, Path: "/diagnostics/api/routing/command",
			HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(diagnosticOrigin(r) + "\x00" + session))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
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
		Programs    []string               `json:"programs,omitempty"`
		Command     *bridge.RoutingCommand `json:"command,omitempty"`
		Description string                 `json:"description,omitempty"`
	}{Revision: c.engine.RoutingStatus().Revision, CSRFToken: csrfToken}
	for _, command := range r.Commands {
		view.Programs = append(view.Programs, command.Path)
	}
	slices.Sort(view.Programs)
	view.Programs = slices.Compact(view.Programs)
	if id != "" {
		command, ok := r.Commands[id]
		if !ok {
			return nil, errors.New("Команда отсутствует. Обновите список.")
		}
		view.Command, view.Description = &command, r.Classifier.Commands[id]
	}
	return view, nil
}

func (c *configurationControl) editCommand(ctx context.Context, edit commandEdit, confirmed bool) (any, error) {
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
	if edit.Command != nil && !confirmed {
		previous := c.active.Routing.Commands[edit.OriginalID]
		if edit.Action == "create" || previous.Path != edit.Command.Path || !slices.Equal(previous.Args, edit.Command.Args) {
			if _, err := bridge.CompileRouting(*r); err != nil {
				return nil, err
			}
			return nil, errCommandApprovalRequired
		}
	}
	return c.applyRoutingChangesLocked(ctx, cfg, raw, changes)
}
