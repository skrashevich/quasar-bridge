package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	q "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func androidCacheFixture(t *testing.T, storage *q.ConfigStorage) (string, string, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	config, path, backups := filepath.Join(dir, "firmware.json"), filepath.Join(dir, "user_config.dat"), filepath.Join(dir, "backups")
	if err := os.WriteFile(config, []byte(`{"aliced":{"uniProxyUrl":"wss://uniproxy.alice.yandex.net/uni.ws"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	data, err := proto.Marshal(storage)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
	return config, path, backups, data
}

func TestAndroidCachedEndpointRollbackPreservesNativeData(t *testing.T) {
	local := "ws://127.0.0.1:8443/uni.ws"
	configJSON := `{"system_config":{"uniProxyUrl":"ws://127.0.0.1:8443/uni.ws","large":900719925474099312345,"other":{"list":[1,true,null]}},"account_config":{"secret":"private-marker","keep":true},"unknown":900719925474099312346}`
	storage := &q.ConfigStorage{CurrentPassportUid: new("owner"), UserConfig: []*q.UserConfig{
		{PassportUid: new("owner"), Config: new(configJSON), GroupConfig: new(`{"group":"keep"}`)},
		{PassportUid: new("other"), Config: new(`{"system_config":{"uniProxyUrl":"wss://other.invalid/uni.ws"},"keep":1}`)},
		{PassportUid: new("absent")},
	}, SubscriptionState: []*q.SubscriptionState{{PassportUid: new("owner"), SubscriptionInfo: new(`{"state":"keep"}`), LastUpdateTime: new(int64(17))}}}
	storage.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 123))
	storage.UserConfig[0].ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 77, protowire.BytesType), []byte("unknown-user")))
	config, path, backups, original := androidCacheFixture(t, storage)
	if err := restoreAndroidCachedEndpoint(config, path, local, backups); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var restored q.ConfigStorage
	if err := proto.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	changed := restored.UserConfig[0].GetConfig()
	var before, after map[string]jsontext.Value
	if json.Unmarshal([]byte(configJSON), &before) != nil || json.Unmarshal([]byte(changed), &after) != nil {
		t.Fatal("invalid configuration JSON")
	}
	for _, key := range []string{"account_config", "unknown"} {
		if !bytes.Equal(before[key], after[key]) {
			t.Fatalf("changed sibling %s", key)
		}
	}
	var beforeSystem, afterSystem map[string]jsontext.Value
	json.Unmarshal(before["system_config"], &beforeSystem)
	json.Unmarshal(after["system_config"], &afterSystem)
	if string(afterSystem["uniProxyUrl"]) != `"wss://uniproxy.alice.yandex.net/uni.ws"` {
		t.Fatal("cached local endpoint not restored")
	}
	for _, key := range []string{"large", "other"} {
		if !bytes.Equal(beforeSystem[key], afterSystem[key]) {
			t.Fatalf("changed system sibling %s", key)
		}
	}
	expected := proto.Clone(storage).(*q.ConfigStorage)
	expected.UserConfig[0].Config = new(changed)
	if !proto.Equal(expected, &restored) {
		t.Fatal("other protobuf fields changed")
	}
	entries, err := os.ReadDir(backups)
	if err != nil || len(entries) != 1 {
		t.Fatal("missing unique native backup", err)
	}
	backupData, err := os.ReadFile(filepath.Join(backups, entries[0].Name()))
	if err != nil || !bytes.Equal(backupData, original) {
		t.Fatal("backup is not byte exact", err)
	}
	if info, err := os.Stat(backups); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("backup directory is not private", err)
	}
	if info, err := entries[0].Info(); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("backup file is not private", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0640 {
		t.Fatal("native mode changed", err)
	}
	if err := restoreAndroidCachedEndpoint(config, path, local, backups); err != nil {
		t.Fatal(err)
	}
	repeated, _ := os.ReadFile(path)
	if !bytes.Equal(repeated, data) {
		t.Fatal("repeat rollback rewrote native data")
	}
	repeatedEntries, _ := os.ReadDir(backups)
	if len(repeatedEntries) != 1 {
		t.Fatal("repeat rollback created extra backup")
	}
}

func TestAndroidCachedEndpointRollbackLeavesOtherEndpointsUntouched(t *testing.T) {
	storage := &q.ConfigStorage{UserConfig: []*q.UserConfig{{Config: new(`{"system_config":{"uniProxyUrl":"ws://127.0.0.1:9999/uni.ws"},"large":900719925474099312345}`)}, {Config: new(`{"account_config":{}}`)}}}
	config, path, backups, original := androidCacheFixture(t, storage)
	if err := restoreAndroidCachedEndpoint(config, path, "ws://127.0.0.1:8443/uni.ws", backups); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, original) {
		t.Fatal("unrelated endpoint changed")
	}
	if _, err := os.Stat(backups); !os.IsNotExist(err) {
		t.Fatal("unnecessary backup created")
	}
}

func TestAndroidCachedEndpointRollbackRejectsInvalidStorageWithoutWriting(t *testing.T) {
	for _, value := range []string{`{private-marker`, `[]`, `null`, `{"system_config":null}`, `{"system_config":{"uniProxyUrl":4}}`} {
		t.Run(value, func(t *testing.T) {
			storage := &q.ConfigStorage{UserConfig: []*q.UserConfig{{Config: new(`{"system_config":{"uniProxyUrl":"ws://127.0.0.1:8443/uni.ws"}}`)}, {Config: new(value)}}}
			config, path, backups, original := androidCacheFixture(t, storage)
			err := restoreAndroidCachedEndpoint(config, path, "ws://127.0.0.1:8443/uni.ws", backups)
			if err == nil {
				t.Fatal("invalid JSON accepted")
			}
			if strings.Contains(err.Error(), "private-marker") {
				t.Fatal("private data in error")
			}
			data, _ := os.ReadFile(path)
			if !bytes.Equal(data, original) {
				t.Fatal("invalid file rewritten")
			}
			if _, err := os.Stat(backups); !os.IsNotExist(err) {
				t.Fatal("backup created before complete validation")
			}
		})
	}
	for _, data := range [][]byte{{0xff}, bytes.Repeat([]byte("x"), (2<<20)+1)} {
		t.Run("invalid-protobuf-or-size", func(t *testing.T) {
			config, path, backups, _ := androidCacheFixture(t, &q.ConfigStorage{})
			os.WriteFile(path, data, 0600)
			if err := restoreAndroidCachedEndpoint(config, path, "ws://127.0.0.1:8443/uni.ws", backups); err == nil {
				t.Fatal("invalid storage accepted")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(after, data) {
				t.Fatal("invalid storage changed")
			}
			if _, err := os.Stat(backups); !os.IsNotExist(err) {
				t.Fatal("backup created for invalid storage")
			}
		})
	}
}

func TestAndroidCachedEndpointRollbackRejectsSymlinksAndUnofficialFirmware(t *testing.T) {
	storage := &q.ConfigStorage{UserConfig: []*q.UserConfig{{Config: new(`{"system_config":{"uniProxyUrl":"ws://127.0.0.1:8443/uni.ws"}}`)}}}
	config, path, backups, original := androidCacheFixture(t, storage)
	link := filepath.Join(filepath.Dir(path), "symlink.dat")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := restoreAndroidCachedEndpoint(config, link, "ws://127.0.0.1:8443/uni.ws", backups); err == nil {
		t.Fatal("symlink storage accepted")
	}
	os.WriteFile(config, []byte(`{"aliced":{"uniProxyUrl":"ws://127.0.0.1:8443/uni.ws"}}`), 0644)
	if err := restoreAndroidCachedEndpoint(config, path, "ws://127.0.0.1:8443/uni.ws", backups); err == nil {
		t.Fatal("unrestored firmware accepted")
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, original) {
		t.Fatal("storage changed after rejection")
	}
	if _, err := os.Stat(backups); !os.IsNotExist(err) {
		t.Fatal("backup created after rejection")
	}
}

func TestAndroidNativeWriterGuardDetectsExactNativeCommand(t *testing.T) {
	for _, command := range []string{"maind\x00--yiod\x00", "/system/vendor/quasar/maind\x00--yiod\x00", "/vendor/quasar/maind\x00--config\x00path\x00--yiod\x00"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			process := filepath.Join(root, "123")
			if err := os.Mkdir(process, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(process, "cmdline"), []byte(command), 0600); err != nil {
				t.Fatal(err)
			}
			err := inspectAndroidNativeWriterStopped(root)
			if err == nil || !strings.Contains(err.Error(), "keep them stopped until rollback completes") {
				t.Fatal("native writer accepted", err)
			}
			if strings.Contains(err.Error(), command) {
				t.Fatal("raw command in error")
			}
		})
	}
}

func TestAndroidNativeWriterGuardAllowsOtherCommandsAndDepartedProcesses(t *testing.T) {
	root := t.TempDir()
	for index, command := range []string{"maind\x00--mediad\x00", "unrelated\x00--yiod\x00", "maind-helper\x00--yiod\x00", "maind\x00--yiod-extra\x00", "", "sh\x00-c\x00maind --yiod\x00"} {
		process := filepath.Join(root, fmt.Sprint(index+1))
		if err := os.Mkdir(process, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(process, "cmdline"), []byte(command), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "99"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "not-a-process"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := inspectAndroidNativeWriterStopped(root); err != nil {
		t.Fatal(err)
	}
}

func TestAndroidNativeWriterGuardFailsClosedOnInspectionError(t *testing.T) {
	root := t.TempDir()
	if err := inspectAndroidNativeWriterStopped(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing proc table accepted")
	}
	process := filepath.Join(root, "123")
	if err := os.MkdirAll(filepath.Join(process, "cmdline"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := inspectAndroidNativeWriterStopped(root); err == nil {
		t.Fatal("unreadable command accepted")
	}
}
