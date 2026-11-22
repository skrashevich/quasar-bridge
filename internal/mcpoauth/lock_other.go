//go:build !linux && !darwin

package mcpoauth

import (
	"context"
	"errors"
	"os"
)

var errStorageUnsupported = errors.New("MCP OAuth credential storage requires Linux or macOS")

func openPrivate(string, bool) (*os.File, error) {
	return nil, errStorageUnsupported
}

func lockCredential(context.Context, string) (func(), error) {
	return nil, errStorageUnsupported
}
