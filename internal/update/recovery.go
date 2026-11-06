package update

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type recoveryFile struct {
	path  string
	raw   []byte
	info  os.FileInfo
	next  []byte
	saved os.FileInfo
}

// saveObserve deliberately does not validate unrelated backend settings: a
// broken LLM configuration must not prevent this recovery operation. start.sh
// sources runtime.env on restart, so its mode override must be persisted too.
func saveObserve(path string) (restore func() error, err error) {
	config, err := readRecoveryFile(path, false)
	if err != nil {
		return nil, err
	}
	var doc map[string]jsontext.Value
	if json.Unmarshal(config.raw, &doc) != nil || doc == nil {
		return nil, errors.New("recovery configuration must be a JSON object")
	}
	doc["mode"] = jsontext.Value(`"observe"`)
	data, err := json.Marshal(doc, json.Deterministic(true))
	if err != nil {
		return nil, errors.New("cannot encode recovery configuration")
	}
	config.next = append(data, '\n')
	environment, err := readRecoveryFile(filepath.Join(filepath.Dir(path), "runtime.env"), true)
	if err != nil {
		return nil, err
	}
	// Append a fixed assignment instead of interpreting or rewriting shell code.
	environment.next = append(bytes.Clone(environment.raw), []byte("\n# Emergency observe recovery\nQUASAR_MODE='observe'\n")...)
	files := []*recoveryFile{config, environment}
	for _, file := range files {
		if err := recoveryFileUnchanged(file.path, file.info, file.raw); err != nil {
			return nil, err
		}
		if file.info != nil {
			if _, err := writeRecoveryConfig(file.path+".recovery-backup", file.raw, 0600); err != nil {
				return nil, fmt.Errorf("cannot save %s recovery backup", filepath.Base(file.path))
			}
		}
	}
	restore = func() error {
		var errs []error
		for _, file := range files {
			if file.saved == nil {
				continue
			}
			if err := recoveryFileUnchanged(file.path, file.saved, file.next); err != nil {
				errs = append(errs, err)
				continue
			}
			if file.info == nil {
				errs = append(errs, os.Remove(file.path))
			} else {
				_, err := writeRecoveryConfig(file.path, file.raw, file.info.Mode().Perm())
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	for _, file := range files {
		if err := recoveryFileUnchanged(file.path, file.info, file.raw); err != nil {
			return nil, errors.Join(err, restore())
		}
		file.saved, err = writeRecoveryConfig(file.path, file.next, 0600)
		if err != nil {
			return nil, fmt.Errorf("cannot save observe recovery files: %w", errors.Join(err, restore()))
		}
	}
	return restore, nil
}

func readRecoveryFile(path string, optional bool) (*recoveryFile, error) {
	file := &recoveryFile{path: path}
	info, err := os.Lstat(path)
	if optional && errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular recovery file, not a symlink", filepath.Base(path))
	}
	if info.Size() > 1<<20 {
		return nil, errors.New("file exceeds recovery size limit")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read recovery file")
	}
	if len(raw) > 1<<20 {
		return nil, errors.New("file exceeds recovery size limit")
	}
	file.info, file.raw = info, raw
	return file, nil
}

func recoveryFileUnchanged(path string, expected os.FileInfo, raw []byte) error {
	info, err := os.Lstat(path)
	if expected == nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || expected == nil || !info.Mode().IsRegular() || !os.SameFile(expected, info) {
		return errors.New("configuration or runtime environment changed externally during recovery")
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, current) {
		return errors.New("configuration or runtime environment changed externally during recovery")
	}
	return nil
}

func writeRecoveryConfig(path string, data []byte, mode os.FileMode) (os.FileInfo, error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".quasar-recovery-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if err != nil || statErr != nil || closeErr != nil {
		return nil, errors.Join(err, statErr, closeErr)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return nil, err
	}
	return info, nil
}
