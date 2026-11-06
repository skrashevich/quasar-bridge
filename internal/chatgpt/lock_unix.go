//go:build linux || darwin

package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func openPrivate(path string, create bool) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NOFOLLOW
	if create {
		flags = os.O_RDWR | os.O_CREATE | syscall.O_NOFOLLOW
	}
	return os.OpenFile(path, flags, 0600)
}

func (s *Store) lock(ctx context.Context) (func(), error) {
	if ctx.Err() != nil {
		return nil, fmt.Errorf("ChatGPT credential lock: %w", ctx.Err())
	}
	path, err := s.filename()
	if err != nil {
		return nil, err
	}
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return nil, errors.New("cannot create ChatGPT credential directory")
	}
	f, err := openPrivate(path+".lock", true)
	if err != nil {
		return nil, errors.New("cannot open ChatGPT credential lock")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || f.Chmod(0600) != nil {
		f.Close()
		return nil, errors.New("cannot protect ChatGPT credential lock")
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, errors.New("cannot lock ChatGPT credentials")
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("ChatGPT credential lock: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}
