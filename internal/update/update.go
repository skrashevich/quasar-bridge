// Package update checks official releases and updates standalone speaker installs.
package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Version is set by the release build; unversioned binaries remain development builds.
var Version = "dev"

const repository = "https://github.com/skrashevich/quasar-bridge"
const latestURL = "https://api.github.com/repos/skrashevich/quasar-bridge/releases/latest"
const maxArchive = 32 << 20
const maxExtracted = 96 << 20

var stableVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type Status struct {
	CurrentVersion           string `json:"current_version"`
	CurrentVersionKnown      bool   `json:"current_version_known"`
	LatestVersion            string `json:"latest_version"`
	UpdateAvailable          bool   `json:"update_available"`
	ReleaseURL               string `json:"release_url"`
	InstallSupported         bool   `json:"install_supported"`
	InstallUnavailableReason string `json:"install_unavailable_reason,omitempty"`
}

type InstallResult struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	LogPath string `json:"log_path,omitempty"`
}

type Updater struct {
	currentVersion string
	executable     string
	configPath     string
	client         *http.Client
}

func New(configPath string) *Updater {
	executable, _ := os.Executable()
	executable, _ = filepath.EvalSymlinks(executable)
	configPath, _ = filepath.Abs(configPath)
	u := &Updater{currentVersion: Version, executable: executable, configPath: configPath, client: &http.Client{
		Timeout: 40 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || req.URL.Scheme != "https" || req.URL.User != nil {
				return errors.New("unsafe or excessive release redirect")
			}
			return nil
		},
	}}
	// Older releases already ship VERSION next to the launcher scripts.
	if u.currentVersion == "dev" && filepath.Base(filepath.Dir(executable)) == "bin" {
		if data, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(executable)), "VERSION")); err == nil {
			if version := strings.TrimSpace(string(data)); stableVersion.MatchString(version) {
				u.currentVersion = version
			}
		}
	}
	return u
}

func (u *Updater) Check(ctx context.Context) (Status, error) {
	data, err := u.fetch(ctx, latestURL, 1<<20)
	if err != nil {
		return Status{}, err
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if json.Unmarshal(data, &release) != nil || release.Draft || release.Prerelease || !strings.HasPrefix(release.Tag, "v") || !stableVersion.MatchString(release.Tag) {
		return Status{}, errors.New("GitHub did not return a valid stable release")
	}
	current := stableVersion.FindStringSubmatch(u.currentVersion)
	latest := stableVersion.FindStringSubmatch(release.Tag)
	status := Status{CurrentVersion: u.currentVersion, CurrentVersionKnown: current != nil, LatestVersion: strings.TrimPrefix(release.Tag, "v"), ReleaseURL: repository + "/releases/tag/" + release.Tag}
	if current != nil {
		for i := 1; i <= 3; i++ {
			if current[i] == latest[i] {
				continue
			}
			status.UpdateAvailable = len(latest[i]) > len(current[i]) || len(latest[i]) == len(current[i]) && latest[i] > current[i]
			break
		}
	}
	if _, err := u.installState(); err != nil {
		status.InstallUnavailableReason = err.Error()
	} else {
		status.InstallSupported = true
	}
	return status, nil
}

func (u *Updater) installState() (string, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" && runtime.GOARCH != "arm" || os.Geteuid() != 0 {
		return "", errors.New("automatic installation requires a standalone Linux ARM64/ARMv7 speaker running as root")
	}
	if !stableVersion.MatchString(u.currentVersion) {
		return "", errors.New("current version is unknown; update development builds manually")
	}
	return standaloneState(u.executable, u.configPath)
}

func standaloneState(executable, configPath string) (string, error) {
	app := filepath.Dir(filepath.Dir(executable))
	state := filepath.Dir(app)
	if filepath.Base(executable) != "quasar-bridge" || filepath.Base(filepath.Dir(executable)) != "bin" || filepath.Base(app) != "app" {
		return "", errors.New("automatic installation requires the standalone app/bin layout; update YAM/manual installs manually")
	}
	config, err := filepath.EvalSymlinks(configPath)
	expected, expectedErr := filepath.EvalSymlinks(filepath.Join(state, "config.json"))
	if err != nil || expectedErr != nil || config != expected {
		return "", errors.New("running configuration does not belong to this standalone installation")
	}
	pid, err := os.ReadFile(filepath.Join(state, "bridge.pid"))
	if err != nil || strings.TrimSpace(string(pid)) != fmt.Sprint(os.Getpid()) {
		return "", errors.New("this process is not managed by the standalone service")
	}
	for _, name := range []string{"install.sh", "control.sh", "VERSION"} {
		info, err := os.Stat(filepath.Join(app, name))
		if err != nil || !info.Mode().IsRegular() {
			return "", errors.New("standalone installation is incomplete")
		}
	}
	return state, nil
}

func (u *Updater) Install(ctx context.Context) (InstallResult, error) {
	state, err := u.installState()
	if err != nil {
		return InstallResult{}, err
	}
	// /tmp is cleared on reboot, so a power loss cannot leave a persistent update lock.
	key := sha256.Sum256([]byte(state))
	lock := filepath.Join(os.TempDir(), fmt.Sprintf("quasar-bridge-update-%x.lock", key[:8]))
	if err := os.Mkdir(lock, 0700); err != nil {
		return InstallResult{}, errors.New("another update is in progress; inspect update.log")
	}
	handedOff := false
	stage := ""
	defer func() {
		if !handedOff {
			if stage != "" {
				_ = os.RemoveAll(stage)
			}
			_ = os.Remove(lock)
		}
	}()
	status, err := u.Check(ctx)
	if err != nil {
		return InstallResult{}, err
	}
	if !status.UpdateAvailable {
		return InstallResult{Status: "up_to_date", Version: status.CurrentVersion}, nil
	}
	stage, err = os.MkdirTemp("", "quasar-bridge-update-")
	if err != nil {
		return InstallResult{}, err
	}
	arch := "arm64"
	if runtime.GOARCH == "arm" {
		arch = "armv7"
	}
	if err := u.prepare(ctx, stage, "v"+status.LatestVersion, arch); err != nil {
		return InstallResult{}, err
	}
	if err := launchInstall(ctx, stage, state, lock); err != nil {
		return InstallResult{}, err
	}
	handedOff = true
	return InstallResult{Status: "scheduled", Version: status.LatestVersion, LogPath: filepath.Join(state, "update.log")}, nil
}

func (u *Updater) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "quasar-bridge-updater")
	if url == latestURL {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, errors.New("cannot reach GitHub release server")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 404 {
			return nil, errors.New("GitHub release or asset not found")
		}
		return nil, fmt.Errorf("GitHub release request failed (HTTP %d)", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, errors.New("cannot read GitHub release response")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("GitHub release response exceeds size limit")
	}
	return data, nil
}

func (u *Updater) prepare(ctx context.Context, stage, tag, arch string) error {
	if !strings.HasPrefix(tag, "v") || !stableVersion.MatchString(tag) || arch != "arm64" && arch != "armv7" {
		return errors.New("invalid release or architecture")
	}
	name := "quasar-bridge-linux-" + arch + ".tar.gz"
	base := repository + "/releases/download/" + tag + "/"
	checksum, err := u.fetch(ctx, base+name+".sha256", 4096)
	if err != nil {
		return err
	}
	fields := strings.Fields(string(checksum))
	if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
		return errors.New("invalid release checksum file")
	}
	expected, err := hex.DecodeString(fields[0])
	if err != nil || len(expected) != sha256.Size {
		return errors.New("invalid release SHA-256")
	}
	// Stream the archive to /tmp: speakers have a small memory budget.
	archive := filepath.Join(stage, name)
	file, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+name, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "quasar-bridge-updater")
	resp, err := u.client.Do(req)
	if err != nil {
		return errors.New("cannot download release archive")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("release archive download failed (HTTP %d)", resp.StatusCode)
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(resp.Body, maxArchive+1))
	if err != nil {
		return errors.New("release archive download interrupted")
	}
	if n > maxArchive {
		return errors.New("release archive exceeds size limit")
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), fields[0]) {
		return errors.New("release SHA-256 mismatch; installation cancelled")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	dest := filepath.Join(stage, "package")
	if err := os.Mkdir(dest, 0700); err != nil {
		return err
	}
	if err := extract(file, dest); err != nil {
		return err
	}
	for name, want := range map[string]string{"ARCH": arch, "VERSION": strings.TrimPrefix(tag, "v")} {
		data, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || strings.TrimSpace(string(data)) != want {
			return errors.New("release archive version or architecture mismatch")
		}
	}
	return ctx.Err()
}

func extract(reader io.Reader, dest string) error {
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return errors.New("invalid release gzip archive")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	entries := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("invalid release tar archive")
		}
		entries++
		if entries > 256 || !filepath.IsLocal(h.Name) || strings.Contains(h.Name, "\\") {
			return errors.New("unsafe release archive path or too many entries")
		}
		path := filepath.Join(dest, h.Name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0700); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if h.Size < 0 || total > maxExtracted {
				return errors.New("extracted release exceeds size limit")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode)&0700)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		default:
			return errors.New("release archive contains links or special files")
		}
	}
	// Drain gzip to verify its trailer as well as the tar payload.
	_, err = io.Copy(io.Discard, io.LimitReader(gz, maxExtracted+1))
	return err
}

// The installer stops this process. Run it in its own session, with no inherited
// pipes, and let it own cleanup; request cancellation must not kill it afterwards.
func launchInstall(ctx context.Context, stage, state, lock string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(state, "update.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	const script = `trap 'rm -rf "$1"; rmdir "$3"' EXIT
trap 'exit 1' HUP INT TERM
# Give the current voice reply time to leave the bridge before restarting.
sleep 5
if QUASAR_DATA_DIR="$2" sh "$1/package/install.sh"; then result=0; else result=$?; fi
printf '\nUpdate installer exit status: %s\n' "$result"
exit "$result"
`
	cmd := exec.Command("sh", "-c", script, "quasar-bridge-update", stage, state, lock)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
