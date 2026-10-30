package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type githubTransport struct{ base string }

func (tr githubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(tr.base, "http://")
	return http.DefaultTransport.RoundTrip(clone)
}

func releaseServer(t *testing.T, tag string, status int) *Updater {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/skrashevich/quasar-bridge/releases/latest" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"tag_name":%q,"draft":false,"prerelease":false}`, tag)
	}))
	t.Cleanup(server.Close)
	return &Updater{currentVersion: "1.2.3", client: &http.Client{Transport: githubTransport{server.URL}}}
}

func TestCheckComparesStableVersions(t *testing.T) {
	for _, tc := range []struct {
		tag, current string
		newer, known bool
	}{
		{"v1.2.4", "1.2.3", true, true}, {"v1.10.0", "1.9.9", true, true},
		{"v1.2.3", "v1.2.3", false, true}, {"v1.2.2", "1.2.3", false, true},
		{"v1.2.4", "dev", false, false},
	} {
		t.Run(tc.tag+"/"+tc.current, func(t *testing.T) {
			u := releaseServer(t, tc.tag, http.StatusOK)
			u.currentVersion = tc.current
			got, err := u.Check(t.Context())
			if err != nil || got.UpdateAvailable != tc.newer || got.CurrentVersionKnown != tc.known || got.LatestVersion != strings.TrimPrefix(tc.tag, "v") {
				t.Fatalf("Check = %+v, %v", got, err)
			}
		})
	}
}

func TestCheckRejectsUnavailableOrInvalidRelease(t *testing.T) {
	for _, tc := range []struct {
		tag    string
		status int
	}{{"v1.2.4", 404}, {"v1.2.4", 403}, {"v1.2.4-rc1", 200}, {"../../evil", 200}} {
		u := releaseServer(t, tc.tag, tc.status)
		if _, err := u.Check(t.Context()); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}

func fixtureArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPrepareVerifiesChecksumBeforeExtracting(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			archive := fixtureArchive(t, map[string]string{"VERSION": "1.2.4\n", "ARCH": "arm64\n", "install.sh": "#!/bin/sh\nexit 0\n", "bin/quasar-bridge": "binary"})
			checksum := fmt.Sprintf("%x  quasar-bridge-linux-arm64.tar.gz\n", sha256.Sum256(archive))
			if bad {
				checksum = strings.Repeat("0", 64) + "  quasar-bridge-linux-arm64.tar.gz\n"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/skrashevich/quasar-bridge/releases/download/v1.2.4/quasar-bridge-linux-arm64.tar.gz":
					w.Write(archive)
				case "/skrashevich/quasar-bridge/releases/download/v1.2.4/quasar-bridge-linux-arm64.tar.gz.sha256":
					io.WriteString(w, checksum)
				default:
					t.Errorf("unexpected URL %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			u := &Updater{client: &http.Client{Transport: githubTransport{server.URL}}}
			dir := t.TempDir()
			err := u.prepare(t.Context(), dir, "v1.2.4", "arm64")
			if bad {
				if err == nil {
					t.Fatal("accepted corrupt archive")
				}
				if _, err := os.Stat(filepath.Join(dir, "package")); !os.IsNotExist(err) {
					t.Fatal("extracted before checksum validation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(dir, "package", "VERSION"))
			if err != nil || string(content) != "1.2.4\n" {
				t.Fatalf("VERSION = %q, %v", content, err)
			}
		})
	}
}

func TestExtractRejectsUnsafeArchive(t *testing.T) {
	for _, name := range []string{"../escaped", "/absolute", "safe/../../escaped"} {
		dir := t.TempDir()
		archive := fixtureArchive(t, map[string]string{name: "bad"})
		if err := extract(bytes.NewReader(archive), dir); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "link", Linkname: "/tmp", Typeflag: tar.TypeSymlink})
	tw.Close()
	gz.Close()
	if err := extract(&buf, t.TempDir()); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestStandaloneRequiresMatchingProcessAndConfig(t *testing.T) {
	state := t.TempDir()
	exe := filepath.Join(state, "app", "bin", "quasar-bridge")
	if err := os.MkdirAll(filepath.Dir(exe), 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"app/bin/quasar-bridge": "binary", "app/install.sh": "installer", "app/control.sh": "control", "app/VERSION": "1.2.3", "bridge.pid": fmt.Sprint(os.Getpid()), "config.json": "{}"} {
		if err := os.WriteFile(filepath.Join(state, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := standaloneState(exe, filepath.Join(state, "config.json")); err != nil || got != state {
		t.Fatalf("state=%q, %v", got, err)
	}
	if _, err := standaloneState(exe, filepath.Join(state, "other.json")); err == nil {
		t.Fatal("accepted unrelated config")
	}
	os.WriteFile(filepath.Join(state, "bridge.pid"), []byte(fmt.Sprint(os.Getpid()+1)), 0600)
	if _, err := standaloneState(exe, filepath.Join(state, "config.json")); err == nil {
		t.Fatal("accepted another service PID")
	}
}

func TestDetachedInstallerRunsAndCleansUp(t *testing.T) {
	if stage := os.Getenv("QUASAR_UPDATE_HELPER_STAGE"); stage != "" {
		if err := launchInstall(t.Context(), stage, os.Getenv("QUASAR_UPDATE_HELPER_STATE"), os.Getenv("QUASAR_UPDATE_HELPER_LOCK")); err != nil {
			t.Fatal(err)
		}
		return
	}
	state := t.TempDir()
	stage, err := os.MkdirTemp("", "quasar-update-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	lock := filepath.Join(state, "lock")
	os.Mkdir(lock, 0700)
	os.Mkdir(filepath.Join(stage, "package"), 0700)
	os.WriteFile(filepath.Join(stage, "package", "install.sh"), []byte("#!/bin/sh\nprintf installed > \"$QUASAR_DATA_DIR/result\"\n"), 0700)
	// The launching process exits before installation begins, just as the old
	// bridge exits when its service is stopped during the update.
	helper := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDetachedInstallerRunsAndCleansUp$")
	helper.Env = append(os.Environ(), "QUASAR_UPDATE_HELPER_STAGE="+stage, "QUASAR_UPDATE_HELPER_STATE="+state, "QUASAR_UPDATE_HELPER_LOCK="+lock)
	if output, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("launcher failed: %v %s", err, output)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, stageErr := os.Stat(stage)
		_, lockErr := os.Stat(lock)
		if os.IsNotExist(stageErr) && os.IsNotExist(lockErr) {
			body, err := os.ReadFile(filepath.Join(state, "result"))
			if err != nil || string(body) != "installed" {
				t.Fatalf("result=%q, %v", body, err)
			}
			if _, err := os.Stat(lock); !os.IsNotExist(err) {
				t.Fatal("lock not removed")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("detached installation did not finish")
}

func TestStatusJSONDoesNotClaimUnknownVersionIsOutdated(t *testing.T) {
	u := releaseServer(t, "v1.2.4", 200)
	u.currentVersion = "dev"
	status, err := u.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"current_version_known":false`)) || !bytes.Contains(raw, []byte(`"update_available":false`)) {
		t.Fatal(string(raw))
	}
}

func TestInstallSchedulesVerifiedReleaseAndRejectsConcurrentUpdate(t *testing.T) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "arm64" && runtime.GOARCH != "arm") || os.Geteuid() != 0 {
		t.Skip("requires a Linux ARM speaker environment; also run in the ARM64 BusyBox container")
	}
	state := t.TempDir()
	exe := filepath.Join(state, "app", "bin", "quasar-bridge")
	if err := os.MkdirAll(filepath.Dir(exe), 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"app/bin/quasar-bridge": "old binary", "app/install.sh": "old installer", "app/control.sh": "control", "app/VERSION": "1.2.3", "bridge.pid": fmt.Sprint(os.Getpid()), "config.json": "existing config", "runtime.env": "existing environment"} {
		if err := os.WriteFile(filepath.Join(state, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	arch := "arm64"
	if runtime.GOARCH == "arm" {
		arch = "armv7"
	}
	archive := fixtureArchive(t, map[string]string{"VERSION": "1.2.4\n", "ARCH": arch, "bin/quasar-bridge": "new binary", "install.sh": "#!/bin/sh\nprintf installed > \"$QUASAR_DATA_DIR/result\"\n"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/skrashevich/quasar-bridge/releases/latest":
			io.WriteString(w, `{"tag_name":"v1.2.4"}`)
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			fmt.Fprintf(w, "%x  quasar-bridge-linux-%s.tar.gz\n", sha256.Sum256(archive), arch)
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	u := &Updater{currentVersion: "1.2.3", executable: exe, configPath: filepath.Join(state, "config.json"), client: &http.Client{Transport: githubTransport{server.URL}}}
	result, err := u.Install(t.Context())
	if err != nil || result.Status != "scheduled" || result.Version != "1.2.4" {
		t.Fatalf("Install = %+v, %v", result, err)
	}
	if _, err := u.Install(t.Context()); err == nil {
		t.Fatal("scheduled a concurrent update")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		log, _ := os.ReadFile(result.LogPath)
		if strings.Contains(string(log), "Update installer exit status: 0") {
			got, err := os.ReadFile(filepath.Join(state, "result"))
			if err != nil || string(got) != "installed" {
				t.Fatalf("installation result=%q, %v", got, err)
			}
			for name, want := range map[string]string{"config.json": "existing config", "runtime.env": "existing environment"} {
				got, err := os.ReadFile(filepath.Join(state, name))
				if err != nil || string(got) != want {
					t.Fatalf("%s changed", name)
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("scheduled installer did not complete")
}
