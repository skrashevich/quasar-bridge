package update

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"quasarbridge/internal/reply"
)

type RestartResult struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Done    <-chan struct{} `json:"-"`
}

// Restart only controls the verified standalone service owning this process and
// configuration. Development builds may restart too; no release lookup occurs.
func (u *Updater) Restart(ctx context.Context) (RestartResult, error) {
	if err := ctx.Err(); err != nil {
		return RestartResult{}, err
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return RestartResult{}, errors.New("voice restart requires the standalone Linux speaker service running as root")
	}
	state, err := standaloneState(u.executable, u.configPath)
	if err != nil {
		return RestartResult{}, err
	}
	key := sha256.Sum256([]byte(state))
	lock := filepath.Join(os.TempDir(), fmt.Sprintf("quasar-bridge-update-%x.lock", key[:8]))
	if err := os.Mkdir(lock, 0700); err != nil {
		return RestartResult{}, errors.New("another update or restart is in progress")
	}
	done, err := launchRestart(ctx, state, lock)
	if err != nil {
		_ = os.Remove(lock)
		return RestartResult{}, err
	}
	return RestartResult{Status: "scheduled", Message: "Bridge restart scheduled after this answer; successful startup is not yet confirmed", Done: done}, nil
}

func launchRestart(ctx context.Context, state, lock string) (<-chan struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(state, "restart.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, errors.New("cannot open restart log")
	}
	defer log.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	// Wait for final speech delivery (or the bounded native request's end), then
	// allow another five seconds for buffered playback before stopping Bridge.
	const script = `trap 'rmdir "$2"' EXIT
trap 'exit 1' HUP INT TERM
read -r ready <&3 || :
exec 3<&-
sleep 5
QUASAR_DATA_DIR="$1" sh "$1/app/control.sh" restart
`
	cmd := exec.Command("sh", "-c", script, "quasar-bridge-restart", state, lock)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{reader}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		return nil, errors.New("cannot schedule service restart")
	}
	stop := context.AfterFunc(reply.DeliveryContext(ctx), func() { _ = writer.Close() })
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		stop()
		_ = writer.Close()
		close(done)
	}()
	return done, nil
}
