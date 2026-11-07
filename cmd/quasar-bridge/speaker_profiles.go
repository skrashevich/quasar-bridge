package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"

	"quasarbridge/internal/speakerprofiles"
)

// speakerProfileNames persists names delivered by the enrollment protocol,
// indexed by the exact hashed enrollment handle.
type speakerProfileNames struct {
	directory *speakerprofiles.Directory
	changed   chan struct{}
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func startSpeakerProfileNames(parent context.Context, configPath string) *speakerProfileNames {
	ctx, cancel := context.WithCancel(parent)
	directory, err := speakerprofiles.OpenDirectory(filepath.Join(filepath.Dir(configPath), "speaker-profiles.json"))
	if err != nil {
		slog.Warn("speaker_name_directory_unavailable", "error", err)
	}
	s := &speakerProfileNames{
		directory: directory,
		changed:   make(chan struct{}, 1),
		cancel:    cancel,
	}
	s.wg.Go(func() {
		flush := func() {
			if err := s.directory.Flush(); err != nil {
				slog.Warn("speaker_name_directory_save_failed", "error", err)
			}
		}
		for {
			select {
			case <-ctx.Done():
				flush()
				return
			case <-s.changed:
				flush()
			}
		}
	})
	return s
}

func (s *speakerProfileNames) Lookup(id string) string {
	return s.directory.Lookup(id)
}

func (s *speakerProfileNames) Observe(id, name string) {
	if s.directory.Observe(id, name) {
		select {
		case s.changed <- struct{}{}:
		default:
		}
	}
}

func (s *speakerProfileNames) Close() {
	s.cancel()
	s.wg.Wait()
}
