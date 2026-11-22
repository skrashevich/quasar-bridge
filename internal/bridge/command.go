package bridge

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/skrashevich/quasar-bridge/internal/dialogue"
)

// RoutingCommand is a deployment setting. Rules and classifier replies refer
// to its registered ID and cannot supply an executable or arguments.
type RoutingCommand struct {
	Path      string   `json:"path"`
	Args      []string `json:"args,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitzero"`
}

var commandID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

func (p *RoutingPolicy) compileCommands(cfg RoutingConfig) error {
	p.commands = make(map[string]*RoutingCommand, len(cfg.Commands))
	for id, cmd := range cfg.Commands {
		if !commandID.MatchString(id) {
			return errors.New("invalid routing command ID")
		}
		if !filepath.IsAbs(cmd.Path) || strings.ContainsRune(cmd.Path, 0) {
			return fmt.Errorf("routing command %s requires an absolute executable path", id)
		}
		if cmd.TimeoutMS < 0 || cmd.TimeoutMS > 60000 {
			return fmt.Errorf("routing command %s timeout_ms must be between 0 and 60000", id)
		}
		for _, arg := range cmd.Args {
			if strings.ContainsRune(arg, 0) {
				return fmt.Errorf("invalid argument for routing command %s", id)
			}
		}
		cmd.Args = slices.Clone(cmd.Args)
		p.commands[id] = &cmd
	}
	if len(cfg.Classifier.Commands) == 0 {
		p.classifier.Commands = nil
		return nil
	}
	for id, description := range cfg.Classifier.Commands {
		if p.commands[id] == nil || strings.TrimSpace(description) == "" {
			return errors.New("classifier.commands requires registered command IDs with nonempty descriptions")
		}
	}
	p.classifier.Commands = maps.Clone(cfg.Classifier.Commands)
	catalog, err := json.Marshal(p.classifier.Commands, json.Deterministic(true))
	if err != nil {
		return err
	}
	p.classifier.Prompt += "\nДополнительный допустимый маршрут — внешняя команда. Если запрос соответствует описанию команды, верни только {\"route\":\"command\",\"command\":\"ID\"}, подставив точный ID из списка. В остальных случаях действуй по основной инструкции. Доступные ID и их назначение: " + string(catalog)
	return nil
}

type commandOriginKey struct{}

type commandOrigin struct {
	RequestID string `json:"request_id,omitempty"`
	InputType string `json:"input_type,omitempty"`
}

type commandInput struct {
	Version     int    `json:"version"`
	Text        string `json:"text"`
	Command     string `json:"command"`
	Source      string `json:"source"`
	Rule        string `json:"rule,omitempty"`
	SpeakerKey  string `json:"speaker_key,omitempty"`
	WakeWordTag string `json:"wake_word_tag,omitempty"`
	commandOrigin
}

const maxCommandOutput = 16 << 10

var errCommandOutputLimit = errors.New("routing command stdout exceeds 16 KiB")

type commandOutput struct {
	data     []byte
	overflow bool
	cancel   context.CancelFunc
}

func (out *commandOutput) Write(p []byte) (int, error) {
	if len(p) > maxCommandOutput-len(out.data) {
		out.overflow = true
		out.cancel()
		return 0, errCommandOutputLimit
	}
	out.data = append(out.data, p...)
	return len(p), nil
}

func (c *RoutingCommand) run(ctx context.Context, text string, decision RouteDecision) (string, error) {
	origin, _ := ctx.Value(commandOriginKey{}).(commandOrigin)
	tag, _ := ctx.Value(wakewordTagKey{}).(string)
	input, err := json.Marshal(commandInput{
		Version: 1, Text: text, Command: decision.Command, Source: decision.Source, Rule: decision.Rule,
		SpeakerKey: dialogue.SpeakerKey(ctx), WakeWordTag: tag, commandOrigin: origin,
	})
	if err != nil {
		return "", errors.New("cannot encode routing command input")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cmp.Or(c.TimeoutMS, 10000))*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Env = []string{}
	for _, key := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TZ", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 250 * time.Millisecond
	cmd.Stdin = bytes.NewReader(append(input, '\n'))
	out := commandOutput{cancel: cancel}
	cmd.Stdout = &out
	err = cmd.Run()
	switch {
	case out.overflow:
		return "", errCommandOutputLimit
	case ctx.Err() != nil:
		return "", ctx.Err()
	case err != nil:
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("routing command exited with status %d", exit.ExitCode())
		}
		return "", errors.New("routing command could not complete")
	case !utf8.Valid(out.data) || bytes.IndexByte(out.data, 0) >= 0:
		return "", errors.New("routing command stdout must be UTF-8 text")
	}
	answer := string(bytes.TrimSpace(out.data))
	if answer == "" {
		return "", errors.New("routing command returned empty stdout")
	}
	return answer, nil
}
