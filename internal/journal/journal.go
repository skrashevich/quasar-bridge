// Package journal writes opt-in conversation text, never raw protocol payloads.
package journal

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type Config struct {
	Enabled      bool   `json:"enabled"`
	Path         string `json:"path"`
	MaxSizeMB    int    `json:"max_size_mb"`
	MaxBackups   int    `json:"max_backups"`
	MaxTextChars int    `json:"max_text_chars"`
}

func DefaultConfig() Config {
	return Config{Path: "runtime/conversations.jsonl", MaxSizeMB: 10, MaxBackups: 3, MaxTextChars: 16000}
}
func (c Config) Validate() error {
	if c.Path == "" || c.MaxSizeMB < 1 || c.MaxSizeMB > 1024 || c.MaxBackups < 0 || c.MaxBackups > 20 || c.MaxTextChars < 1 || c.MaxTextChars > 64000 {
		return errors.New("invalid conversation_log: require path, max_size_mb 1..1024, max_backups 0..20, max_text_chars 1..64000")
	}
	return nil
}

// Speaker contains request-bound recognition evidence. IDs are hashed upstream;
// profile names and credentials are deliberately absent from this log format.
type Speaker struct {
	Status                 string `json:"status"`
	ID                     string `json:"id,omitempty"`
	Source                 string `json:"source,omitempty"`
	Confidence             string `json:"confidence,omitempty"`
	NameAvailable          bool   `json:"name_available,omitzero"`
	CandidateID            string `json:"candidate_id,omitempty"`
	CandidateNameAvailable bool   `json:"candidate_name_available,omitzero"`
	Scoring                bool   `json:"scoring,omitzero"`
	Classification         bool   `json:"classification,omitzero"`
	CandidateCount         int    `json:"candidate_count,omitzero"`
}

type Event struct {
	Time      time.Time `json:"time"`
	Session   string    `json:"session,omitempty"`
	Request   string    `json:"request,omitempty"`
	Event     string    `json:"event"`
	Kind      string    `json:"kind,omitempty"`
	Route     string    `json:"route,omitempty"`
	Text      string    `json:"text,omitempty"`
	Speech    string    `json:"speech,omitempty"`
	Delivered bool      `json:"delivered"`
	ElapsedMS int64     `json:"elapsed_ms,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
	Speaker   *Speaker  `json:"speaker,omitempty"`
}
type Writer struct {
	mu       sync.Mutex
	config   Config
	file     *os.File
	size     int64
	enabled  atomic.Bool
	failures atomic.Uint64
}

func (w *Writer) Config() Config   { w.mu.Lock(); defer w.mu.Unlock(); return w.config }
func (w *Writer) Enabled() bool    { return w != nil && w.enabled.Load() }
func (w *Writer) Failures() uint64 { return w.failures.Load() }
func (w *Writer) Configure(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if c == w.config && (!c.Enabled || w.file != nil) {
		return nil
	}
	var f *os.File
	var size int64
	if c.Enabled {
		var err error
		f, size, err = openLog(c.Path)
		if err != nil {
			return err
		}
		// Files .1..20 are the writer's bounded backup namespace.
		for i := c.MaxBackups + 1; i <= 20; i++ {
			name := fmt.Sprintf("%s.%d", c.Path, i)
			info, statErr := os.Lstat(name)
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			if statErr != nil || !info.Mode().IsRegular() {
				f.Close()
				return errors.New("cannot prune conversation log backups")
			}
			if err := os.Remove(name); err != nil {
				f.Close()
				return errors.New("cannot prune conversation log backups")
			}
		}
	}
	if w.file != nil {
		w.file.Close()
	}
	w.file = f
	w.size = size
	w.config = c
	w.enabled.Store(c.Enabled)
	return nil
}
func openLog(path string) (*os.File, int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, 0, errors.New("cannot create conversation log directory")
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, 0, errors.New("conversation log must be a regular file")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, 0, errors.New("cannot open conversation log")
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, errors.New("cannot stat conversation log")
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, 0, errors.New("cannot protect conversation log")
	}
	return f, info.Size(), nil
}
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enabled.Store(false)
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}
func shorten(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	return string([]rune(s)[:n]), true
}
func (w *Writer) Record(e Event) {
	if !w.Enabled() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.enabled.Load() || !w.config.Enabled {
		return
	}
	if w.file == nil {
		var err error
		w.file, w.size, err = openLog(w.config.Path)
		if err != nil {
			w.noteFailure()
			return
		}
	}
	e.Time = time.Now().UTC()
	var a, b bool
	e.Text, a = shorten(e.Text, w.config.MaxTextChars)
	e.Speech, b = shorten(e.Speech, w.config.MaxTextChars)
	e.Truncated = a || b
	raw, err := json.Marshal(e)
	if err == nil {
		raw = append(raw, '\n')
		if w.size+int64(len(raw)) > int64(w.config.MaxSizeMB)<<20 {
			err = w.rotate()
		}
		if err == nil {
			var n int
			n, err = w.file.Write(raw)
			w.size += int64(n)
		}
	}
	if err != nil {
		// No payload or path in operational logs, and no failed logging error reaches the speaker.
		w.noteFailure()
	}
}
func (w *Writer) noteFailure() {
	if w.failures.Add(1) == 1 {
		slog.Warn("conversation_log_write_failed")
	}
}
func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	c := w.config
	for i := c.MaxBackups; i > 1; i-- {
		src := fmt.Sprintf("%s.%d", c.Path, i-1)
		dst := fmt.Sprintf("%s.%d", c.Path, i)
		if err := os.Rename(src, dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if c.MaxBackups > 0 {
		if err := os.Rename(c.Path, c.Path+".1"); err != nil {
			return err
		}
	} else if err := os.Remove(c.Path); err != nil {
		return err
	}
	f, err := os.OpenFile(c.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	w.file = f
	w.size = 0
	return nil
}
