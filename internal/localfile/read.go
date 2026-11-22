// Package localfile reads regular files from configured local paths.
package localfile

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// Symlinks controls whether a configured path may end in a symlink.
type Symlinks bool

const (
	NoFollow Symlinks = false
	Follow   Symlinks = true
)

// Open verifies the open descriptor, without blocking on a FIFO.
func Open(path string, links Symlinks) (*os.File, os.FileInfo, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if links == NoFollow {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, errors.New("expected a regular file")
	}
	return f, info, nil
}

// Read checks the size again while reading in case the open file grows.
func Read(path string, links Symlinks, limit int64) ([]byte, os.FileInfo, error) {
	f, info, err := Open(path, links)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	if info.Size() > limit {
		return nil, nil, errors.New("file exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, errors.New("file exceeds size limit")
	}
	return data, info, nil
}
