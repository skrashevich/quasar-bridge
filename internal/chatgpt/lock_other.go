//go:build !linux && !darwin

package chatgpt

import (
	"context"
	"errors"
	"os"
)

func openPrivate(path string, create bool) (*os.File, error) {
	return nil, errors.New("ChatGPT credential storage requires Linux or macOS")
}

func (s *Store) lock(ctx context.Context) (func(), error) {
	return nil, errors.New("ChatGPT credential storage requires Linux or macOS")
}
