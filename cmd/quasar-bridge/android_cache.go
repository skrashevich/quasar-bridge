package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	q "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
	"google.golang.org/protobuf/proto"
)

const androidNativeConfigLimit = 2 << 20

// Restore only the endpoint cached by this bridge. A configuration version may
// stay unchanged after unmounting firmware, so native sync need not resend it.
func restoreAndroidCachedEndpoint(configPath, storagePath, local, backupDir string) error {
	endpoint, err := url.Parse(local)
	if err != nil || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "/uni.ws" || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") {
		return errors.New("cached Android endpoint must be a loopback WebSocket /uni.ws URL")
	}
	ip, err := netip.ParseAddr(endpoint.Hostname())
	if err != nil || !ip.IsLoopback() {
		return errors.New("cached Android endpoint must use a loopback IP")
	}
	if err := ensureAndroidNativeWriterStopped(); err != nil {
		return err
	}
	firmware, _, err := readAndroidNativeConfigFile(configPath)
	if err != nil {
		return errors.New("cannot read restored Android firmware configuration")
	}
	var firmwareConfig struct {
		Aliced struct {
			URL string `json:"uniProxyUrl"`
		} `json:"aliced"`
	}
	if json.Unmarshal(firmware, &firmwareConfig) != nil || firmwareConfig.Aliced.URL != "wss://uniproxy.alice.yandex.net/uni.ws" {
		return errors.New("Android cache rollback requires the restored official firmware endpoint")
	}
	originalInfo, err := os.Lstat(storagePath)
	if err != nil || !originalInfo.Mode().IsRegular() {
		return errors.New("Android native configuration storage must be a regular file")
	}
	raw, info, err := readAndroidNativeConfigFile(storagePath)
	if err != nil || !os.SameFile(originalInfo, info) {
		return errors.New("cannot read Android native configuration storage")
	}
	var storage q.ConfigStorage
	if proto.Unmarshal(raw, &storage) != nil {
		return errors.New("invalid Android native configuration storage")
	}
	changed := false
	for _, user := range storage.GetUserConfig() {
		if user.Config == nil {
			continue
		}
		var config map[string]jsontext.Value
		if json.Unmarshal([]byte(user.GetConfig()), &config) != nil || config == nil {
			return errors.New("invalid cached Android configuration JSON")
		}
		value, present := config["system_config"]
		if !present {
			continue
		}
		var system map[string]jsontext.Value
		if json.Unmarshal(value, &system) != nil || system == nil {
			return errors.New("invalid cached Android system configuration")
		}
		value, present = system["uniProxyUrl"]
		if !present {
			continue
		}
		var current string
		if json.Unmarshal(value, &current) != nil {
			return errors.New("invalid cached Android endpoint")
		}
		if current != local {
			continue
		}
		system["uniProxyUrl"], _ = json.Marshal(firmwareConfig.Aliced.URL)
		config["system_config"], err = json.Marshal(system)
		if err != nil {
			return errors.New("cannot encode cached Android system configuration")
		}
		encoded, err := json.Marshal(config)
		if err != nil {
			return errors.New("cannot encode cached Android configuration")
		}
		user.Config = new(string(encoded))
		changed = true
	}
	if !changed {
		return nil
	}
	updated, err := proto.Marshal(&storage)
	if err != nil || len(updated) > androidNativeConfigLimit {
		return errors.New("cannot encode Android native configuration storage")
	}
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		return errors.New("cannot create private Android cache backup directory")
	}
	backupInfo, err := os.Lstat(backupDir)
	if err != nil || !backupInfo.IsDir() {
		return errors.New("Android cache backup directory must be a private directory")
	}
	if err := os.Chmod(backupDir, 0700); err != nil {
		return errors.New("cannot protect Android cache backup directory")
	}
	backup, err := os.CreateTemp(backupDir, "android-user-config-*.dat")
	if err != nil {
		return errors.New("cannot create Android cache backup")
	}
	if _, err = backup.Write(raw); err == nil {
		err = backup.Sync()
	}
	closeErr := backup.Close()
	if err != nil || closeErr != nil {
		os.Remove(backup.Name())
		return errors.New("cannot save Android cache backup")
	}
	if err := syncAndroidCacheDirectory(backupDir); err != nil {
		return err
	}
	stage, err := os.CreateTemp(filepath.Dir(storagePath), ".quasar-android-cache-*")
	if err != nil {
		return errors.New("cannot stage Android native configuration storage")
	}
	defer os.Remove(stage.Name())
	if err = stage.Chmod(info.Mode().Perm()); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			err = stage.Chown(int(stat.Uid), int(stat.Gid))
		}
	}
	if err == nil {
		_, err = stage.Write(updated)
	}
	if err == nil {
		err = stage.Sync()
	}
	closeErr = stage.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot save Android native configuration storage")
	}
	// Do not replace a newer native update observed since the initial snapshot.
	currentInfo, err := os.Lstat(storagePath)
	if err != nil || !currentInfo.Mode().IsRegular() || !os.SameFile(info, currentInfo) {
		return errors.New("Android native configuration storage changed during rollback")
	}
	current, currentOpenedInfo, err := readAndroidNativeConfigFile(storagePath)
	if err != nil || !os.SameFile(info, currentOpenedInfo) || !bytes.Equal(current, raw) {
		return errors.New("Android native configuration storage changed during rollback")
	}
	if err := ensureAndroidNativeWriterStopped(); err != nil {
		return err
	}
	if err := os.Rename(stage.Name(), storagePath); err != nil {
		return errors.New("cannot replace Android native configuration storage")
	}
	return syncAndroidCacheDirectory(filepath.Dir(storagePath))
}

func readAndroidNativeConfigFile(path string) ([]byte, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > androidNativeConfigLimit {
		return nil, nil, errors.New("invalid native file")
	}
	data, err := io.ReadAll(io.LimitReader(f, androidNativeConfigLimit+1))
	if err != nil || len(data) > androidNativeConfigLimit {
		return nil, nil, errors.New("invalid native file")
	}
	return data, info, nil
}

func syncAndroidCacheDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return errors.New("cannot sync Android native configuration directory")
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return errors.New("cannot sync Android native configuration directory")
	}
	return nil
}

// The native supervisor must also stay quiesced across rollback. These two
// checks catch an active writer; they cannot reserve its process lifecycle.
func ensureAndroidNativeWriterStopped() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	return inspectAndroidNativeWriterStopped("/proc")
}

func inspectAndroidNativeWriterStopped(procRoot string) error {
	stopped := errors.New("Stop the native yiod service and its supervisor, and keep them stopped until rollback completes")
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return stopped
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		command, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "cmdline"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return stopped
		}
		args := bytes.Split(command, []byte{0})
		if len(args) < 2 || filepath.Base(string(args[0])) != "maind" {
			continue
		}
		for _, arg := range args[1:] {
			if bytes.Equal(arg, []byte("--yiod")) {
				return stopped
			}
		}
	}
	return nil
}
