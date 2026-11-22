package main

import (
	"os"
	"path/filepath"
	"testing"

	"quasarbridge/internal/bridge"
)

func TestReadSpeakerInfo(t *testing.T) {
	dir := t.TempDir()
	cfg, boot := filepath.Join(dir, "quasar.cfg"), filepath.Join(dir, "cmdline")
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(boot, "console=tty0 androidboot.deviceid=unrelated androidboot.serialno=DEMO0000000001\n")
	if got := readSpeakerInfo(cfg, boot); got != (bridge.SpeakerInfo{}) {
		t.Fatalf("reported the external host as a speaker: %+v", got)
	}
	write(cfg, `{"common":{"deviceType":"yandexmidi","softwareVersion":"1.2"}}`)
	want := bridge.SpeakerInfo{Model: "yandexmidi", FirmwareVersion: "1.2", SerialNumber: "DEMO0000000001"}
	if got := readSpeakerInfo(cfg, boot); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	write(filepath.Join(dir, "version_full"), "1.2.3.4\n")
	write(boot, "androidboot.deviceid=unrelated")
	want.FirmwareVersion, want.SerialNumber = "1.2.3.4", ""
	if got := readSpeakerInfo(cfg, boot); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
