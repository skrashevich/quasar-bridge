package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type chatGPTModelState struct {
	Active   string `json:"active"`
	Saved    string `json:"saved"`
	Writable bool   `json:"writable"`
}

var errModelChanged = errors.New("Настройки модели изменились. Обновите список и повторите выбор.")

func (c *configurationControl) activeModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active.LLMModel
}

func (c *configurationControl) chatGPTModel() (chatGPTModelState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, _, err := c.chatGPTModelLocked()
	return state, err
}

func (c *configurationControl) chatGPTModelLocked() (chatGPTModelState, *installerModelEnv, error) {
	next, err := readConfig(c.path)
	if err != nil {
		return chatGPTModelState{}, nil, err
	}
	if c.active.LLMAPI != "chatgpt" || next.LLMAPI != "chatgpt" {
		return chatGPTModelState{}, nil, errors.New("Для выбора модели включите провайдер ChatGPT и примените настройки.")
	}
	model, overridden := os.LookupEnv("QUASAR_LLM_MODEL")
	var env *installerModelEnv
	if overridden {
		env = readInstallerModelEnv(c.path, model)
	}
	return chatGPTModelState{c.active.LLMModel, next.LLMModel, !overridden || env != nil}, env, nil
}

func (c *configurationControl) saveChatGPTModel(ctx context.Context, expected, model string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, env, err := c.chatGPTModelLocked()
	if err != nil {
		return nil, err
	}
	if state.Saved != expected {
		return nil, errModelChanged
	}
	if !state.Writable {
		return nil, errors.New("Модель задана окружением службы. Измените QUASAR_LLM_MODEL в настройках запуска.")
	}
	if c.restartPending() {
		return nil, errors.New("restart already scheduled; wait for the new process")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if model == expected {
		c.active.LLMModel = model
		state.Active = model
		return state, nil
	}
	if env == nil {
		raw, err := os.ReadFile(c.path)
		if err != nil {
			return nil, err
		}
		previous, err := parseConfig(raw, false)
		if err != nil || previous.LLMModel != expected || previous.LLMAPI != "chatgpt" {
			return nil, errModelChanged
		}
		encoded, err := json.Marshal(model)
		if err != nil {
			return nil, err
		}
		if _, err := c.setLocked(ctx, map[string]jsontext.Value{"llm_model": encoded}, raw); err != nil {
			return nil, err
		}
	} else if err := env.save(model); err != nil {
		return nil, err
	}
	c.active.LLMModel = model
	state.Active, state.Saved = model, model
	return state, nil
}

// setup.sh owns exactly one quoted assignment in its marked block. Leave
// arbitrary shell programs, manual overrides and other service environments read-only.
type installerModelEnv struct {
	path  string
	raw   []byte
	lines []string
	index int
}

var simpleAssignment = regexp.MustCompile("^(?:export[ \\t]+)?([A-Za-z_][A-Za-z0-9_]*)=(?:[A-Za-z0-9_./:@%,+=?#-]*|'[^']*'|\"(?:[^\"\\\\$`]|\\\\.)*\")$")

func modelAssignment(model string) string {
	// Use the same escaping for double-quoted values as packaging/speaker/setup.sh.
	escape := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	return `QUASAR_LLM_MODEL="` + escape.Replace(model) + `"`
}

func readInstallerModelEnv(configPath, model string) *installerModelEnv {
	dir := os.Getenv("QUASAR_DATA_DIR")
	path, err := filepath.Abs(configPath)
	if err != nil || dir == "" || path != filepath.Join(dir, "config.json") {
		return nil
	}
	path = filepath.Join(dir, "runtime.env")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 1<<20 {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	index := -1
	inside, seen := false, false
	for i, line := range lines {
		switch line {
		case "# BEGIN QUASAR SETUP":
			if seen {
				return nil
			}
			inside, seen = true, true
		case "# END QUASAR SETUP":
			if !inside {
				return nil
			}
			inside = false
		default:
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			assignment := simpleAssignment.FindStringSubmatch(line)
			if assignment == nil {
				return nil
			}
			if assignment[1] == "QUASAR_LLM_MODEL" {
				if !inside || index != -1 || lines[i] != modelAssignment(model) {
					return nil
				}
				index = i
			}
		}
	}
	if inside || index == -1 {
		return nil
	}
	return &installerModelEnv{path, raw, lines, index}
}

func (e *installerModelEnv) save(model string) error {
	current, err := os.ReadFile(e.path)
	if err != nil || !bytes.Equal(current, e.raw) {
		return errModelChanged
	}
	e.lines[e.index] = modelAssignment(model)
	if err := writeConfiguration(e.path, []byte(strings.Join(e.lines, "\n")), 0600); err != nil {
		return errors.New("Не удалось сохранить модель в настройках установщика.")
	}
	// Subsequent config reads in this process must see the persisted override.
	return os.Setenv("QUASAR_LLM_MODEL", model)
}
