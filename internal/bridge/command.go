package bridge

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
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
	logTag    string
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
	total    int64
	overflow bool
	cancel   context.CancelFunc
}

func (out *commandOutput) Write(p []byte) (int, error) {
	out.total += int64(len(p))
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
	// For web and CLI requests there is no native message ID. Give each call
	// its own tag so concurrent runs of the same registration stay distinguishable.
	invocation := origin.logTag
	if invocation == "" {
		invocation = requestTag(newID())
	}
	timeoutMS := cmp.Or(c.TimeoutMS, 10000)
	log := slog.With("request", invocation, "command", decision.Command, "source", decision.Source, "program", c.Path, "argc", len(c.Args), "timeout_ms", timeoutMS)
	if decision.Rule != "" {
		log = log.With("rule", decision.Rule)
	}
	start := time.Now()
	tag, _ := ctx.Value(wakewordTagKey{}).(string)
	input, err := json.Marshal(commandInput{
		Version: 1, Text: text, Command: decision.Command, Source: decision.Source, Rule: decision.Rule,
		SpeakerKey: dialogue.SpeakerKey(ctx), WakeWordTag: tag, commandOrigin: origin,
	})
	if err != nil {
		log.Error("routing_command_failed", "stage", "stdin", "error", "cannot encode routing command input")
		return "", errors.New("cannot encode routing command input")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
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
	var stderr commandStderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cause := ctx.Err()
		if cause == nil {
			cause = errors.New("could not start process")
			if errno, ok := errors.AsType[syscall.Errno](err); ok {
				cause = fmt.Errorf("cannot start process: %w", errno)
			}
		}
		log.Error("routing_command_failed", "stage", "start", "elapsed_ms", time.Since(start).Milliseconds(), "error", cause)
		return "", cause
	}
	log = log.With("pid", cmd.Process.Pid)
	log.Info("routing_command_started")
	err = cmd.Wait()
	log = log.With("elapsed_ms", time.Since(start).Milliseconds(), "stdout_bytes", out.total, "stderr_bytes", stderr.total)
	if state := cmd.ProcessState; state != nil {
		if code := state.ExitCode(); code >= 0 {
			log = log.With("exit_code", code)
		} else if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			log = log.With("signal", status.Signal().String())
		}
	}
	answer, stage, err := commandResult(ctx, &out, err)
	if err != nil {
		if stderr.total != 0 {
			log = log.With("stderr", stderr.excerpt(text, c.Args, origin.RequestID, dialogue.SpeakerKey(ctx), tag), "stderr_truncated", stderr.total > int64(len(stderr.data)))
		}
		log.Error("routing_command_failed", "stage", stage, "error", err)
		return "", err
	}
	log.Info("routing_command_finished")
	return answer, nil
}

// The error may reach the web chat. Child output belongs only in diagnostics.
func commandResult(ctx context.Context, out *commandOutput, err error) (answer, stage string, cause error) {
	switch {
	case out.overflow:
		return "", "stdout", errCommandOutputLimit
	case ctx.Err() != nil:
		return "", "wait", ctx.Err()
	case err != nil:
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return "", "wait", fmt.Errorf("routing command terminated by signal %s", status.Signal())
			}
			return "", "wait", fmt.Errorf("routing command exited with status %d", exit.ExitCode())
		}
		return "", "wait", fmt.Errorf("waiting for routing command: %w", err)
	case !utf8.Valid(out.data) || bytes.IndexByte(out.data, 0) >= 0:
		return "", "stdout", errors.New("routing command stdout must be UTF-8 text without NUL")
	}
	answer = string(bytes.TrimSpace(out.data))
	if answer == "" {
		return "", "stdout", errors.New("routing command returned empty stdout")
	}
	return answer, "", nil
}
