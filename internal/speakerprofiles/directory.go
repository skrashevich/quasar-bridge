// Package speakerprofiles stores names learned from explicit protocol profiles.
package speakerprofiles

import (
	"encoding/json/v2"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	maxPeople         = 64
	maxDirectoryBytes = 64 << 10
)

var errDirectory = errors.New("speaker profile catalogue unavailable")

type nameEntry struct {
	Name     string `json:"name,omitempty"`
	Conflict bool   `json:"conflict,omitzero"`
}
type catalogue struct {
	Version int                  `json:"version"`
	Entries map[string]nameEntry `json:"entries"`
}

// Directory stores names learned from explicit server enrollment directives.
// It contains only hashed profile IDs and names, never credentials or biometrics.
// Observe updates memory; the caller schedules Flush outside the proxy lock.
type Directory struct {
	path                string
	flushMu             sync.Mutex
	mu                  sync.RWMutex
	entries             map[string]nameEntry
	generation, flushed uint64
}

// OpenDirectory returns an empty usable directory alongside a safe error if its
// previous cache is unreadable or invalid. Missing files are normal first use.
func OpenDirectory(path string) (*Directory, error) {
	d := &Directory{path: path, entries: map[string]nameEntry{}}
	body, err := readBounded(path, maxDirectoryBytes)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, errDirectory
	}
	var stored catalogue
	if json.Unmarshal(body, &stored) != nil || stored.Version != 1 || len(stored.Entries) > maxPeople {
		return d, errDirectory
	}
	for id, entry := range stored.Entries {
		if !validID(id) || entry.Conflict && entry.Name != "" || !entry.Conflict && (entry.Name == "" || validName(entry.Name) != entry.Name) {
			return d, errDirectory
		}
	}
	d.entries = stored.Entries
	if d.entries == nil {
		d.entries = map[string]nameEntry{}
	}
	return d, nil
}

func validID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'f') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// Observe returns whether a valid new observation changed the catalogue. A
// conflict is retained as an explicit unknown, including across restarts.
func (d *Directory) Observe(id, name string) bool {
	if !validID(id) {
		return false
	}
	name = validName(name)
	if name == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, exists := d.entries[id]
	if exists && (entry.Conflict || entry.Name == name) {
		return false
	}
	if !exists && len(d.entries) >= maxPeople {
		return false
	}
	if exists {
		d.entries[id] = nameEntry{Conflict: true}
	} else {
		d.entries[id] = nameEntry{Name: name}
	}
	d.generation++
	return true
}

// Name distinguishes absent entries from known conflicts (empty name, true).
// A caller must not fall back to a different source for a conflicted profile.
func (d *Directory) Name(id string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	entry, ok := d.entries[id]
	return entry.Name, ok
}
func (d *Directory) Lookup(id string) string { name, _ := d.Name(id); return name }

// Flush writes a bounded atomic private cache without holding the read lock
// during disk I/O. Calls are serialized; observations during a write stay dirty.
func (d *Directory) Flush() error {
	d.flushMu.Lock()
	defer d.flushMu.Unlock()
	d.mu.RLock()
	generation := d.generation
	if generation == d.flushed {
		d.mu.RUnlock()
		return nil
	}
	stored := catalogue{Version: 1, Entries: maps.Clone(d.entries)}
	d.mu.RUnlock()
	body, err := json.Marshal(stored)
	if err != nil || len(body) > maxDirectoryBytes {
		return errDirectory
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0700); err != nil {
		return errDirectory
	}
	temp, err := os.CreateTemp(filepath.Dir(d.path), ".speaker-profiles-*")
	if err != nil {
		return errDirectory
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return errDirectory
	}
	if _, err := temp.Write(body); err != nil {
		temp.Close()
		return errDirectory
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return errDirectory
	}
	if err := temp.Close(); err != nil {
		return errDirectory
	}
	if err := os.Rename(tempName, d.path); err != nil {
		return errDirectory
	}
	d.mu.Lock()
	d.flushed = generation
	d.mu.Unlock()
	return nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errDirectory
	}
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errDirectory
	}
	return body, nil
}

func validName(value string) string {
	for _, r := range value {
		if unicode.IsControl(r) {
			return ""
		}
	}
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) || len(value) > 320 || utf8.RuneCountInString(value) > 80 {
		return ""
	}
	return strings.Clone(value)
}
