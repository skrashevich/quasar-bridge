package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// The firmware keeps one spotter component for the word chosen in the app and
// an optional second one, "custom_activation". Filling the second slot with a
// model that ships in the firmware makes the speaker answer to both words.
const (
	wakeSlotName     = "custom_activation"
	wakeSlotMarker   = ".second-wake-word"
	wakeConfigKey    = "customActivation"
	wakeFirmwareHint = "customActivation"
)

var (
	wakeWordPattern    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	selectedWordRegexp = regexp.MustCompile(`"spotter"\s*:\s*"([a-z0-9_]+)"`)
)

// speakerWakePaths are the firmware and state locations used by the second wake word.
type speakerWakePaths struct {
	Config     string // quasar.cfg
	Models     string // spotter_models directory
	Maind      string // voice daemon binary, checked for second-slot support
	UserConfig string // account settings with the word chosen in the app
	State      string // persistent directory for the backup and state file
}

type speakerWakeState struct {
	Config        string `json:"config"`
	Slot          string `json:"slot"`
	Word          string `json:"word"`
	AppliedSHA256 string `json:"applied_sha256"`
}

func (p speakerWakePaths) statePath() string    { return filepath.Join(p.State, "state.json") }
func (p speakerWakePaths) originalPath() string { return filepath.Join(p.State, "quasar.cfg.orig") }
func (p speakerWakePaths) slotPath() string     { return filepath.Join(p.Models, wakeSlotName) }

// setSpeakerWake enables, disables or reports the second wake word. Like
// setSpeakerEndpoint it edits only its own member of quasar.cfg, so the rest of
// the firmware configuration keeps its values whatever the file's formatting.
func setSpeakerWake(p speakerWakePaths, action, word string) (string, error) {
	switch action {
	case "apply":
		return applySpeakerWake(p, word)
	case "rollback":
		return rollbackSpeakerWake(p)
	case "status":
		return statusSpeakerWake(p), nil
	}
	return "", errors.New("speaker-wake must be apply, rollback or status")
}

func readSpeakerConfig(path string) (map[string]jsontext.Value, map[string]jsontext.Value, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, errors.New("cannot read speaker config")
	}
	return parseSpeakerConfig(raw)
}

func parseSpeakerConfig(raw []byte) (doc, alice map[string]jsontext.Value, err error) {
	if json.Unmarshal(raw, &doc) != nil {
		return nil, nil, errors.New("invalid speaker JSON")
	}
	if json.Unmarshal(doc["aliced"], &alice) != nil || alice == nil {
		return nil, nil, errors.New("missing aliced configuration; this firmware is laid out differently, nothing changed")
	}
	return doc, alice, nil
}

func marshalSpeakerConfig(doc, alice map[string]jsontext.Value) ([]byte, error) {
	var err error
	if doc["aliced"], err = json.Marshal(alice, json.Deterministic(true)); err != nil {
		return nil, err
	}
	return json.Marshal(doc, json.Deterministic(true))
}

func wakeBlock(word string) jsontext.Value {
	data, _ := json.Marshal(map[string]any{"forceEnable": true, "forcePhrase": word}, json.Deterministic(true))
	return data
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// selectedWakeWord returns the word chosen in the app ("alisa", "yandex") or "".
func selectedWakeWord(userConfig string) string {
	data, err := os.ReadFile(userConfig)
	if err != nil {
		return ""
	}
	if m := selectedWordRegexp.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

// fileContains reports whether the file holds needle, without loading it whole:
// maind is tens of megabytes and the bridge runs under a small memory limit.
func fileContains(path string, needle []byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	keep := 0
	for {
		n, err := f.Read(buf[keep:])
		end := keep + n
		if bytes.Contains(buf[:end], needle) {
			return true, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err
		}
		keep = min(len(needle)-1, end)
		copy(buf, buf[end-keep:end])
	}
}

func firmwareSupportsSecondWake(maind string) error {
	found, err := fileContains(maind, []byte(wakeFirmwareHint))
	if err != nil {
		return fmt.Errorf("cannot inspect the voice daemon (%s): %w", maind, err)
	}
	if !found {
		return errors.New("this firmware has no second wake word support (the voice daemon does not know customActivation); nothing changed")
	}
	return nil
}

func applySpeakerWake(p speakerWakePaths, word string) (string, error) {
	if _, err := os.Stat(p.statePath()); err == nil {
		return "", errors.New("second wake word is already enabled; run rollback first")
	}
	if word == "" {
		switch chosen := selectedWakeWord(p.UserConfig); chosen {
		case "alisa":
			word = "yandex"
		case "yandex":
			word = "alisa"
		default:
			return "", errors.New("cannot tell which word is chosen in the app; pass the second word explicitly")
		}
	}
	if !wakeWordPattern.MatchString(word) {
		return "", errors.New("invalid wake word model name")
	}
	if err := firmwareSupportsSecondWake(p.Maind); err != nil {
		return "", err
	}
	source := filepath.Join(p.Models, "activation", word)
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		return "", fmt.Errorf("this firmware has no %q wake word model (%s)", word, source)
	}
	if _, err := os.Lstat(p.slotPath()); err == nil {
		return "", fmt.Errorf("%s already exists; if an earlier run left it, run rollback first", p.slotPath())
	}
	raw, err := os.ReadFile(p.Config)
	if err != nil {
		return "", errors.New("cannot read speaker config")
	}
	doc, alice, err := parseSpeakerConfig(raw)
	if err != nil {
		return "", err
	}
	if _, ok := alice[wakeConfigKey]; ok {
		return "", errors.New("aliced already has customActivation that this tool did not write; nothing changed")
	}
	alice[wakeConfigKey] = wakeBlock(word)
	patched, err := marshalSpeakerConfig(doc, alice)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(p.Config)
	if err != nil {
		return "", err
	}
	// State first: a crash after this point still leaves a way back.
	if err := os.MkdirAll(p.State, 0700); err != nil {
		return "", errors.New("cannot create state directory")
	}
	_ = os.Remove(p.originalPath()) // a leftover of an interrupted run must never be restored
	if err := writeNewFile(p.originalPath(), raw, 0600); err != nil {
		return "", errors.New("cannot save the original speaker config")
	}
	state, _ := json.Marshal(speakerWakeState{Config: p.Config, Slot: p.slotPath(), Word: word, AppliedSHA256: sha256Hex(patched)})
	if err := writeNewFile(p.statePath(), state, 0600); err != nil {
		return "", errors.New("cannot save state")
	}
	undo := func() {
		_ = os.RemoveAll(p.slotPath())
		_ = os.Remove(p.statePath())
	}
	if err := copyWakeModel(source, p.slotPath()); err != nil {
		undo()
		return "", err
	}
	if err := replaceFile(p.Config, patched, info.Mode().Perm()); err != nil {
		undo()
		return "", err
	}
	return fmt.Sprintf("Second wake word %q enabled; reboot the speaker to apply it", word), nil
}

// copyWakeModel copies a firmware model directory into the second slot and
// marks it, so that rollback never removes a directory this tool did not make.
func copyWakeModel(source, slot string) error {
	staging := slot + ".new"
	_ = os.RemoveAll(staging)
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(source, path)
		target := filepath.Join(staging, rel)
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0755)
		case entry.Type().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0644)
		}
		return fmt.Errorf("unexpected file type in %s", path)
	})
	if err == nil {
		err = os.WriteFile(filepath.Join(staging, wakeSlotMarker), nil, 0644)
	}
	if err == nil {
		err = os.Rename(staging, slot)
	}
	if err != nil {
		_ = os.RemoveAll(staging)
		return errors.New("cannot create the second wake word model (is the firmware writable?)")
	}
	return nil
}

func rollbackSpeakerWake(p speakerWakePaths) (string, error) {
	slotMine := false
	if _, err := os.Stat(filepath.Join(p.slotPath(), wakeSlotMarker)); err == nil {
		slotMine = true
	}
	stateRaw, err := os.ReadFile(p.statePath())
	if errors.Is(err, fs.ErrNotExist) {
		if !slotMine {
			return "", errors.New("second wake word is not enabled; nothing to roll back")
		}
		// Leftover of an interrupted run: only the marked model directory remains.
		if err := os.RemoveAll(p.slotPath()); err != nil {
			return "", errors.New("cannot remove the second wake word model")
		}
		return "Removed a leftover second wake word model; reboot the speaker", nil
	}
	if err != nil {
		return "", errors.New("cannot read state")
	}
	var state speakerWakeState
	if json.Unmarshal(stateRaw, &state) != nil || state.Config != p.Config {
		return "", errors.New("state does not match this configuration")
	}
	current, err := os.ReadFile(p.Config)
	if err != nil {
		return "", errors.New("cannot read speaker config")
	}
	info, err := os.Stat(p.Config)
	if err != nil {
		return "", err
	}
	if sha256Hex(current) == state.AppliedSHA256 {
		// Untouched since apply: bring back the original file exactly.
		original, err := os.ReadFile(p.originalPath())
		if err != nil {
			return "", errors.New("cannot read the saved original speaker config")
		}
		if err := replaceFile(p.Config, original, info.Mode().Perm()); err != nil {
			return "", err
		}
	} else {
		// Changed since (firmware or cloud update): remove only our member.
		doc, alice, err := parseSpeakerConfig(current)
		if err != nil {
			return "", err
		}
		if block, ok := alice[wakeConfigKey]; ok {
			if !sameJSON(block, wakeBlock(state.Word)) {
				return "", errors.New("customActivation was changed externally; refusing rollback, inspect the saved original")
			}
			delete(alice, wakeConfigKey)
			data, err := marshalSpeakerConfig(doc, alice)
			if err != nil {
				return "", err
			}
			if err := replaceFile(p.Config, data, info.Mode().Perm()); err != nil {
				return "", err
			}
		}
	}
	note := ""
	if slotMine {
		if err := os.RemoveAll(p.slotPath()); err != nil {
			return "", errors.New("cannot remove the second wake word model")
		}
	} else if _, err := os.Lstat(p.slotPath()); err == nil {
		note = fmt.Sprintf(" (%s was not made by this tool and was left in place)", p.slotPath())
	}
	_ = os.Remove(p.statePath())
	_ = os.Remove(p.originalPath())
	return "Second wake word disabled" + note + "; reboot the speaker to apply it", nil
}

func sameJSON(a, b jsontext.Value) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && fmt.Sprint(x) == fmt.Sprint(y)
}

func statusSpeakerWake(p speakerWakePaths) string {
	var b strings.Builder
	if raw, err := os.ReadFile(p.statePath()); err == nil {
		var state speakerWakeState
		_ = json.Unmarshal(raw, &state)
		fmt.Fprintf(&b, "enabled: second word %q (made by this tool)\n", state.Word)
	} else {
		b.WriteString("enabled: no\n")
	}
	if _, alice, err := readSpeakerConfig(p.Config); err != nil {
		fmt.Fprintf(&b, "quasar.cfg: %v\n", err)
	} else if block, ok := alice[wakeConfigKey]; ok {
		fmt.Fprintf(&b, "quasar.cfg: customActivation present: %s\n", block)
	} else {
		b.WriteString("quasar.cfg: customActivation absent\n")
	}
	switch _, err := os.Stat(filepath.Join(p.slotPath(), wakeSlotMarker)); {
	case err == nil:
		fmt.Fprintf(&b, "model: %s made by this tool\n", p.slotPath())
	default:
		if _, err := os.Lstat(p.slotPath()); err == nil {
			fmt.Fprintf(&b, "model: %s exists, not made by this tool\n", p.slotPath())
		} else {
			fmt.Fprintf(&b, "model: %s absent\n", p.slotPath())
		}
	}
	if err := firmwareSupportsSecondWake(p.Maind); err != nil {
		fmt.Fprintf(&b, "firmware support: no (%v)\n", err)
	} else {
		b.WriteString("firmware support: yes (the voice daemon knows customActivation)\n")
	}
	fmt.Fprintf(&b, "word chosen in the app: %s\n", selectedWakeWord(p.UserConfig))
	return strings.TrimRight(b.String(), "\n")
}

var renameFile = os.Rename

func writeInPlace(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// writeNewFile creates a file that must not exist yet and syncs it.
func writeNewFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// replaceFile swaps a file for new content through a temporary file in the same
// directory, keeping the original permissions.
func replaceFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".quasar-bridge-*")
	if err != nil {
		return errors.New("cannot stage speaker config; remount the filesystem writable")
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("cannot save speaker config: %w", err)
	}
	err = renameFile(tmp, path)
	if errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EXDEV) {
		// The file is a mount point (for example a bind-mounted copy), so it cannot
		// be replaced, only rewritten. Not atomic, but the original is saved.
		err = writeInPlace(path, data)
	}
	if err != nil {
		return fmt.Errorf("cannot replace speaker config: %w", err)
	}
	return nil
}
