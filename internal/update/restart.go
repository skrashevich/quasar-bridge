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
	return u.scheduleServiceAction(ctx, "restart")
}

// Recover schedules a local recovery operation without consulting any backend.
// observe persists the passthrough mode; rollback restores the speaker endpoint
// using the standalone controller and reboots only if restoration succeeds.
func (u *Updater) Recover(ctx context.Context, action string) (RestartResult, error) {
	if action != "observe" && action != "rollback" {
		return RestartResult{}, errors.New("recovery action must be observe or rollback")
	}
	return u.scheduleServiceAction(ctx, action)
}

func (u *Updater) scheduleServiceAction(ctx context.Context, action string) (RestartResult, error) {
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
	handedOff := false
	defer func() {
		if !handedOff {
			_ = os.Remove(lock)
		}
	}()
	var restore func() error
	serviceAction := action
	switch action {
	case "observe":
		restore, err = saveObserve(u.configPath)
		if err != nil {
			return RestartResult{}, err
		}
		serviceAction = "restart"
	case "rollback":
		info, err := os.Lstat(filepath.Join(state, "endpoint-backup.json"))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return RestartResult{}, errors.New("rollback requires a regular, nonempty endpoint backup")
		}
	}
	done, err := launchServiceAction(ctx, state, lock, serviceAction)
	if err != nil {
		if restore != nil {
			if restoreErr := restore(); restoreErr != nil {
				return RestartResult{}, fmt.Errorf("observe configuration was saved but restart could not be scheduled; restoring the previous configuration failed: %w", errors.Join(err, restoreErr))
			}
		}
		return RestartResult{}, err
	}
	handedOff = true
	message := "Bridge restart scheduled after this answer; successful startup is not yet confirmed"
	if action == "observe" {
		message = "Observe mode saved and Bridge restart scheduled after this answer; successful startup is not yet confirmed"
	} else if action == "rollback" {
		message = "Speaker endpoint rollback and reboot scheduled after this answer; successful recovery is not yet confirmed"
	}
	return RestartResult{Status: "scheduled", Message: message, Done: done}, nil
}

func launchServiceAction(ctx context.Context, state, lock, action string) (<-chan struct{}, error) {
	if action != "restart" && action != "rollback" {
		return nil, errors.New("invalid service action")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	logName := "restart.log"
	if action == "rollback" {
		logName = "recovery.log"
	}
	log, err := os.OpenFile(filepath.Join(state, logName), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, errors.New("cannot open service operation log")
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
export QUASAR_DATA_DIR="$1"
case "$3" in
restart) sh "$1/app/control.sh" restart ;;
rollback) sh "$1/app/control.sh" rollback && reboot ;;
esac
`
	cmd := exec.Command("sh", "-c", script, "quasar-bridge-service", state, lock, action)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{reader}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		return nil, errors.New("cannot schedule service operation")
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
