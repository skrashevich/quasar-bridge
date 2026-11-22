// Package commandprompt plays a confirmation number directly on the local speaker.
// The prompt never goes through the network, assistant history or packet journal.
package commandprompt

import (
	"archive/zip"
	"bytes"
	"context"
	_ "embed"
	"io"
	"os/exec"
	"strconv"
	"time"
)

//go:embed russian.bank
var vocabulary []byte

// Play speaks a three-digit number twice. Only PCM is passed to the player.
func Play(ctx context.Context, code int) error {
	bank, err := zip.NewReader(bytes.NewReader(vocabulary), int64(len(vocabulary)))
	if err != nil {
		return err
	}
	var intro, number bytes.Buffer
	word := func(dst *bytes.Buffer, name string) error {
		f, err := bank.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(dst, f)
		return err
	}
	if err := word(&intro, "intro"); err != nil {
		return err
	}
	parts := []int{code / 100 * 100}
	if rest := code % 100; rest >= 20 {
		parts = append(parts, rest/10*10, rest%10)
	} else {
		parts = append(parts, rest)
	}
	for _, part := range parts {
		if part != 0 {
			if err := word(&number, strconv.Itoa(part)); err != nil {
				return err
			}
		}
	}
	// 0.6 seconds of silence between the two readings at 22,050 Hz (S16_LE mono).
	pause := bytes.NewReader(make([]byte, 26460))
	cmd := exec.CommandContext(ctx, "aplay", "-q", "-D", "default", "-t", "raw", "-f", "S16_LE", "-r", "22050", "-c", "1")
	cmd.Stdin = io.MultiReader(&intro, bytes.NewReader(number.Bytes()), pause, &number)
	cmd.WaitDelay = time.Second
	return cmd.Run()
}
