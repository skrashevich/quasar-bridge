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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

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
		_, local := diagnosticClient(r)
		if r.Method != http.MethodPost {
			value, err := c.commandForPanel(r.URL.Query().Get("id"), token, local)
			writeDiagnosticResult(w, value, err)
			return
		}
		tokens := r.Header.Values("X-Quasar-CSRF")
		if r.Header.Get("Origin") != diagnosticOrigin(r) || token == "" || len(tokens) != 1 || !hmac.Equal([]byte(tokens[0]), []byte(token)) {
			writeDiagnosticError(w, http.StatusForbidden, "Откройте редактор заново и повторите сохранение.")
			return
		}
		var edit commandEdit
		if readDiagnosticJSON(w, r, &edit) {
			value, err := c.editCommand(r.Context(), edit, local)
			writeDiagnosticResult(w, value, err)
		}
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

func (c *configurationControl) commandDirectory() (string, error) {
	return filepath.Abs(filepath.Join(filepath.Dir(c.path), "commands"))
}

// Local installation grants a helper its web-callable interface. For programs
// elsewhere, the panel can reuse an invocation already present in the configuration.
func (c *configurationControl) validatePanelProgram(cmd bridge.RoutingCommand) error {
	dir, err := c.commandDirectory()
	if err != nil {
		return err
	}
	if filepath.Dir(cmd.Path) == dir && filepath.Clean(cmd.Path) == cmd.Path {
		for _, path := range []string{filepath.Dir(dir), dir, cmd.Path} {
			info, err := os.Lstat(path)
			if err != nil || info.Mode().Perm()&0022 != 0 {
				return fmt.Errorf("Проверьте, что программа установлена в %s и только владелец может её изменять.", dir)
			}
			owner := info.Sys().(*syscall.Stat_t).Uid
			if owner != 0 && owner != uint32(os.Geteuid()) {
				return errors.New("Программа и её каталог должны принадлежать владельцу службы или root.")
			}
			if path == cmd.Path {
				if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
					return errors.New("Выберите обычный исполняемый файл в каталоге программ.")
				}
			} else if !info.IsDir() {
				return errors.New("Каталог программ должен быть обычным каталогом.")
			}
		}
		return nil
	}
	for _, configured := range c.active.Routing.Commands {
		if configured.Path == cmd.Path && slices.Equal(configured.Args, cmd.Args) {
			return nil
		}
	}
	return fmt.Errorf("Установите программу в %s или зарегистрируйте этот путь и аргументы через SSH-туннель.", dir)
}

func (c *configurationControl) commandForPanel(id, csrfToken string, local bool) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.active.Routing
	view := struct {
		Revision    string                 `json:"revision"`
		CSRFToken   string                 `json:"csrf_token"`
		Directory   string                 `json:"command_directory,omitempty"`
		Programs    []string               `json:"programs,omitempty"`
		Command     *bridge.RoutingCommand `json:"command,omitempty"`
		Description string                 `json:"description,omitempty"`
	}{Revision: c.engine.RoutingStatus().Revision, CSRFToken: csrfToken}
	if !local {
		dir, err := c.commandDirectory()
		if err != nil {
			return nil, err
		}
		view.Directory = dir
		for _, command := range r.Commands {
			view.Programs = append(view.Programs, command.Path)
		}
		if info, err := os.Lstat(dir); err == nil && info.IsDir() {
			files, _ := os.ReadDir(dir)
			for _, file := range files {
				program := bridge.RoutingCommand{Path: filepath.Join(dir, file.Name())}
				if c.validatePanelProgram(program) == nil {
					view.Programs = append(view.Programs, program.Path)
				}
			}
		}
		slices.Sort(view.Programs)
		view.Programs = slices.Compact(view.Programs)
	}
	if id != "" {
		command, ok := r.Commands[id]
		if !ok {
			return nil, errors.New("Команда отсутствует. Обновите список.")
		}
		view.Command, view.Description = &command, r.Classifier.Commands[id]
	}
	return view, nil
}

func (c *configurationControl) editCommand(ctx context.Context, edit commandEdit, local bool) (any, error) {
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
		if !local {
			if err := c.validatePanelProgram(*edit.Command); err != nil {
				return nil, err
			}
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
