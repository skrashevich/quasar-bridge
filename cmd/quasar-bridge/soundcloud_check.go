package main

import (
	"bufio"
	"errors"
	"io"
	"strings"

	"quasarbridge/internal/soundcloud"
)

// Proxy credentials arrive on stdin, never as command-line arguments. The
// installer sends exactly one line and persists it only after a successful check.
func readSoundCloudProxy(input io.Reader) (string, error) {
	const limit = 4096
	raw, err := bufio.NewReader(io.LimitReader(input, limit+1)).ReadString('\n')
	if (err != nil && !errors.Is(err, io.EOF)) || len(raw) > limit {
		return "", errors.New("soundcloud: invalid proxy input")
	}
	raw = strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
	if raw == "" {
		return "", errors.New("soundcloud: proxy URL is required")
	}
	if err := (soundcloud.Config{ProxyURL: raw}).Validate(); err != nil {
		return "", err
	}
	return raw, nil
}
