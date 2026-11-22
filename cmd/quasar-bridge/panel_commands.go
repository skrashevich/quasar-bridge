package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/skrashevich/quasar-bridge/internal/bridge"
)

// LAN access to the ordinary diagnostics panel does not grant shell privileges.
// A local operator can use an SSH tunnel or supply this installation's editor key.
func (c *configurationControl) commandEditorAccess(w http.ResponseWriter, r *http.Request) bool {
	if c == nil {
		writeDiagnosticError(w, http.StatusServiceUnavailable, "Редактор команд недоступен.")
		return false
	}
	if _, local := diagnosticAddresses(r); local {
		return true
	}
	c.mu.Lock()
	key, err := commandEditorKey(filepath.Join(filepath.Dir(c.path), "command-editor.key"))
	c.mu.Unlock()
	if err != nil {
		writeDiagnosticError(w, http.StatusInternalServerError, "Не удалось прочитать ключ редактора рядом с config.json.")
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(key)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="Quasar Bridge command editor"`)
		writeDiagnosticError(w, http.StatusUnauthorized, "Для редактирования команд по сети введите ключ редактора.")
		return false
	}
	return true
}

func commandEditorKey(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		data, err := os.ReadFile(path)
		key := strings.TrimSpace(string(data))
		if err == nil && len(key) < 26 {
			err = errors.New("invalid command editor key")
		}
		return key, err
	}
	if err != nil {
		return "", err
	}
	key := rand.Text()
	_, err = f.WriteString(key + "\n")
	if err = errors.Join(err, f.Close()); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return key, nil
}

type commandEdit struct {
	Action           string                 `json:"action"`
	ID               string                 `json:"id"`
	OriginalID       string                 `json:"original_id,omitempty"`
	Command          *bridge.RoutingCommand `json:"command,omitempty"`
	Description      *string                `json:"description,omitempty"`
	ExpectedRevision string                 `json:"expected_revision"`
}

func (c *configurationControl) commandForPanel(id string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.active.Routing
	view := struct {
		Revision    string                 `json:"revision"`
		Command     *bridge.RoutingCommand `json:"command,omitempty"`
		Description string                 `json:"description,omitempty"`
	}{Revision: c.engine.RoutingStatus().Revision}
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
