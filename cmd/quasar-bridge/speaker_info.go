package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"

	"github.com/skrashevich/quasar-bridge/internal/bridge"
	"github.com/skrashevich/quasar-bridge/internal/localfile"
)

// The Quasar firmware stores its full build and the bootloader's serial
// separately. androidboot.deviceid is an account-facing ID, not that serial.
func readSpeakerInfo(configPath, cmdlinePath string) bridge.SpeakerInfo {
	var cfg struct {
		Common struct {
			DeviceType      string `json:"deviceType"`
			SoftwareVersion string `json:"softwareVersion"`
		} `json:"common"`
	}
	var info bridge.SpeakerInfo
	if raw, _, err := localfile.Read(configPath, localfile.Follow, 1<<20); err == nil && json.Unmarshal(raw, &cfg) == nil {
		info.Model = strings.TrimSpace(cfg.Common.DeviceType)
		info.FirmwareVersion = strings.TrimSpace(cfg.Common.SoftwareVersion)
	}
	if raw, _, err := localfile.Read(filepath.Join(filepath.Dir(configPath), "version_full"), localfile.Follow, 4096); err == nil {
		if version := strings.TrimSpace(string(raw)); version != "" {
			info.FirmwareVersion = version
		}
	}
	if info == (bridge.SpeakerInfo{}) {
		return info
	}
	cmdline, _ := os.ReadFile(cmdlinePath)
	for field := range strings.FieldsSeq(string(cmdline)) {
		if serial, ok := strings.CutPrefix(field, "androidboot.serialno="); ok {
			info.SerialNumber = strings.Trim(serial, `"`)
			break
		}
	}
	return info
}
