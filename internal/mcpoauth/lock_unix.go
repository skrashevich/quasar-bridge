//go:build linux || darwin

package mcpoauth

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

func openPrivate(path string, create bool) (*os.File, error) {
	flags := os.O_RDONLY
	if create {
		flags = os.O_RDWR | os.O_CREATE
	}
	return os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
}

func lockCredential(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := openPrivate(path+".lock", true)
	if err != nil {
		return nil, errors.New("cannot open MCP OAuth credential lock")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		_ = f.Close()
		return nil, errors.New("invalid MCP OAuth credential lock")
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, errors.New("cannot lock MCP OAuth credentials")
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
