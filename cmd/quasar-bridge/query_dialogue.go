package main

import (
	"errors"
	"sync"
	"time"
	"uuid"

	"quasarbridge/internal/yandex"
)

var (
	errInvalidQueryDialog = errors.New("dialog_id must be a UUID returned by the query API")
	errMissingQueryDialog = errors.New("unknown or expired dialog_id; omit it to start a new dialog")
	errQueryDialogLimit   = errors.New("too many active query dialogs; retry after an idle dialog expires")
)

type queryDialog struct {
	id     string
	used   time.Time
	yandex *yandex.Client
}

// API dialog identifiers are local handles, never Yandex's session payloads.
type queryDialogs struct {
	mu      sync.Mutex
	entries map[string]*queryDialog
	ttl     time.Duration
	now     func() time.Time
}

func newQueryDialogs(ttl time.Duration) *queryDialogs {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &queryDialogs{entries: map[string]*queryDialog{}, ttl: ttl, now: time.Now}
}

func (s *queryDialogs) acquire(id string) (*queryDialog, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for key, d := range s.entries {
		if now.Sub(d.used) >= s.ttl {
			delete(s.entries, key)
		}
	}
	if id != "" {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return nil, false, errInvalidQueryDialog
		}
		d, ok := s.entries[parsed.String()]
		if !ok {
			return nil, false, errMissingQueryDialog
		}
		d.used = now
		return d, false, nil
	}
	if len(s.entries) >= 32 {
		return nil, false, errQueryDialogLimit
	}
	d := &queryDialog{id: uuid.NewV4().String(), used: now}
	s.entries[d.id] = d
	return d, true, nil
}

func (s *queryDialogs) finish(id string, discard bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if discard {
		delete(s.entries, id)
	} else if d := s.entries[id]; d != nil {
		d.used = s.now()
	}
}
