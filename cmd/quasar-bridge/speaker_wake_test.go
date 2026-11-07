package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const (
	wakeCompactConfig = `{"common":{"statisticSampeRate":0.14999999999999999,"big":1e3},"aliced":{"vadEnabled":false,"uniProxyUrl":"wss://x.example.invalid/uni.ws"},"zeta":{"aliced_like":1}}`
	// Same data as the factory firmware writes it: indented, spaces around colons.
	wakePrettyConfig = "{\n   \"common\" : {\n      \"statisticSampeRate\" : 0.14999999999999999,\n      \"big\" : 1e3\n   },\n   \"aliced\" : {\n      \"vadEnabled\" : false,\n      \"uniProxyUrl\" : \"wss://x.example.invalid/uni.ws\"\n   },\n   \"zeta\" : { \"aliced_like\" : 1 }\n}\n"
)

type wakeFixture struct {
	t     *testing.T
	paths speakerWakePaths
}

func newWakeFixture(t *testing.T, config string) *wakeFixture {
	t.Helper()
	root := t.TempDir()
	f := &wakeFixture{t: t, paths: speakerWakePaths{
		Config:     filepath.Join(root, "quasar.cfg"),
		Models:     filepath.Join(root, "spotter_models"),
		Maind:      filepath.Join(root, "maind"),
		UserConfig: filepath.Join(root, "user_config.dat"),
		State:      filepath.Join(root, "state"),
	}}
	f.write(f.paths.Config, config)
	f.write(f.paths.Maind, "ELF....padding customActivation forceEnable....")
	f.write(f.paths.UserConfig, `xx{"account_config":{"spotter":"alisa","jingle":false}}`)
	for _, word := range []string{"alisa", "yandex"} {
		f.write(filepath.Join(f.paths.Models, "activation", word, "acoustic_model.nnet"), word+word)
		f.write(filepath.Join(f.paths.Models, "activation", word, "tts-timings-info", "t.json"), "{}")
	}
	return f
}

func (f *wakeFixture) write(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *wakeFixture) read(path string) string {
	f.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *wakeFixture) exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (f *wakeFixture) run(action, word string) (string, error) {
	f.t.Helper()
	return setSpeakerWake(f.paths, action, word)
}

func (f *wakeFixture) mustRun(action, word string) string {
	f.t.Helper()
	out, err := f.run(action, word)
	if err != nil {
		f.t.Fatalf("%s %s: %v", action, word, err)
	}
	return out
}

func TestSpeakerWakeAppliesToCompactAndFactoryFormatting(t *testing.T) {
	for name, config := range map[string]string{"compact": wakeCompactConfig, "factory": wakePrettyConfig} {
		t.Run(name, func(t *testing.T) {
			f := newWakeFixture(t, config)
			f.mustRun("apply", "")
			patched := f.read(f.paths.Config)
			if !strings.Contains(patched, `"customActivation":{"forceEnable":true,"forcePhrase":"yandex"}`) {
				t.Fatalf("block missing: %s", patched)
			}
			// Other members keep their text, in particular numbers.
			for _, keep := range []string{`0.14999999999999999`, `1e3`, `"aliced_like":1`, `"vadEnabled":false`, `wss://x.example.invalid/uni.ws`} {
				if !strings.Contains(strings.ReplaceAll(patched, " ", ""), keep) {
					t.Errorf("%s lost: %s", keep, patched)
				}
			}
			slot := f.paths.slotPath()
			if f.read(filepath.Join(slot, "acoustic_model.nnet")) != "yandexyandex" || !f.exists(filepath.Join(slot, "tts-timings-info", "t.json")) || !f.exists(filepath.Join(slot, wakeSlotMarker)) {
				t.Fatal("model not copied")
			}
			if f.read(filepath.Join(f.paths.Models, "activation", "yandex", "acoustic_model.nnet")) != "yandexyandex" || f.exists(filepath.Join(f.paths.Models, "activation", "yandex", wakeSlotMarker)) {
				t.Fatal("firmware model was touched")
			}
			f.mustRun("rollback", "")
			if f.read(f.paths.Config) != config {
				t.Fatalf("config not restored byte for byte:\n%s", f.read(f.paths.Config))
			}
			if f.exists(f.paths.slotPath()) || f.exists(f.paths.statePath()) || f.exists(f.paths.originalPath()) {
				t.Fatal("rollback left files")
			}
		})
	}
}

func TestSpeakerWakeChoosesWordNotSelectedInApp(t *testing.T) {
	f := newWakeFixture(t, wakeCompactConfig)
	f.write(f.paths.UserConfig, `{"spotter":"yandex"}`)
	f.mustRun("apply", "")
	if !strings.Contains(f.read(f.paths.Config), `"forcePhrase":"alisa"`) {
		t.Fatal("expected alisa when the app has yandex")
	}
	f.mustRun("rollback", "")
	f.mustRun("apply", "yandex")
	if !strings.Contains(f.read(f.paths.Config), `"forcePhrase":"yandex"`) {
		t.Fatal("explicit word ignored")
	}
	g := newWakeFixture(t, wakeCompactConfig)
	g.write(g.paths.UserConfig, `{"spotter":"yasmina"}`)
	if _, err := g.run("apply", ""); err == nil {
		t.Fatal("unknown app word must require an explicit second word")
	}
}

func TestSpeakerWakeRefusesWithoutChangingAnything(t *testing.T) {
	cases := map[string]func(f *wakeFixture, word *string){
		"no aliced section":     func(f *wakeFixture, _ *string) { f.write(f.paths.Config, `{"common":{}}`) },
		"invalid JSON":          func(f *wakeFixture, _ *string) { f.write(f.paths.Config, `{"aliced":`) },
		"firmware without slot": func(f *wakeFixture, _ *string) { f.write(f.paths.Maind, "ELF no such feature") },
		"model missing":         func(f *wakeFixture, w *string) { *w = "yasmina" },
		"invalid model name":    func(f *wakeFixture, w *string) { *w = "../activation/alisa" },
		"foreign slot": func(f *wakeFixture, _ *string) {
			f.write(filepath.Join(f.paths.slotPath(), "custom.nnet"), "mine")
		},
		"foreign customActivation": func(f *wakeFixture, _ *string) {
			f.write(f.paths.Config, `{"aliced":{"customActivation":{"forceEnable":false}}}`)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newWakeFixture(t, wakeCompactConfig)
			word := "yandex"
			setup(f, &word)
			before := f.read(f.paths.Config)
			if _, err := f.run("apply", word); err == nil {
				t.Fatal("apply must refuse")
			}
			if f.read(f.paths.Config) != before || f.exists(f.paths.statePath()) {
				t.Fatal("refused apply changed files")
			}
			if name == "foreign slot" && f.read(filepath.Join(f.paths.slotPath(), "custom.nnet")) != "mine" {
				t.Fatal("foreign slot touched")
			}
		})
	}
}

func TestSpeakerWakeApplyTwiceIsRefused(t *testing.T) {
	f := newWakeFixture(t, wakeCompactConfig)
	f.mustRun("apply", "yandex")
	patched := f.read(f.paths.Config)
	if _, err := f.run("apply", "alisa"); err == nil {
		t.Fatal("second apply must be refused")
	}
	if f.read(f.paths.Config) != patched || !strings.Contains(patched, `"forcePhrase":"yandex"`) {
		t.Fatal("second apply changed the first result")
	}
}

func TestSpeakerWakeRollbackKeepsUnrelatedChanges(t *testing.T) {
	f := newWakeFixture(t, wakeCompactConfig)
	f.mustRun("apply", "yandex")
	// The firmware or the cloud changed something else in the meantime.
	f.write(f.paths.Config, strings.Replace(f.read(f.paths.Config), `"vadEnabled":false`, `"vadEnabled":true`, 1))
	f.mustRun("rollback", "")
	got := f.read(f.paths.Config)
	if strings.Contains(got, "customActivation") || !strings.Contains(got, `"vadEnabled":true`) {
		t.Fatalf("rollback must remove only its own member: %s", got)
	}
	if f.exists(f.paths.slotPath()) || f.exists(f.paths.statePath()) {
		t.Fatal("rollback left files")
	}
}

func TestSpeakerWakeRollbackRefusesExternallyChangedBlock(t *testing.T) {
	f := newWakeFixture(t, wakeCompactConfig)
	f.mustRun("apply", "yandex")
	f.write(f.paths.Config, strings.Replace(f.read(f.paths.Config), `"forcePhrase":"yandex"`, `"forcePhrase":"other"`, 1))
	changed := f.read(f.paths.Config)
	if _, err := f.run("rollback", ""); err == nil {
		t.Fatal("rollback must refuse a block changed by someone else")
	}
	if f.read(f.paths.Config) != changed || !f.exists(f.paths.statePath()) {
		t.Fatal("refused rollback changed files")
	}
}

func TestSpeakerWakeRollbackLeavesForeignSlotAndCleansLeftovers(t *testing.T) {
	// A model directory somebody else created after apply stays in place.
	f := newWakeFixture(t, wakeCompactConfig)
	f.mustRun("apply", "yandex")
	if err := os.RemoveAll(f.paths.slotPath()); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.paths.slotPath(), "theirs.nnet"), "theirs")
	f.mustRun("rollback", "")
	if f.read(filepath.Join(f.paths.slotPath(), "theirs.nnet")) != "theirs" {
		t.Fatal("foreign slot removed")
	}
	// An interrupted apply leaves only a marked model: rollback cleans it.
	g := newWakeFixture(t, wakeCompactConfig)
	g.write(filepath.Join(g.paths.slotPath(), wakeSlotMarker), "")
	g.mustRun("rollback", "")
	if g.exists(g.paths.slotPath()) {
		t.Fatal("leftover not removed")
	}
	// Nothing to roll back at all.
	if _, err := newWakeFixture(t, wakeCompactConfig).run("rollback", ""); err == nil {
		t.Fatal("rollback of nothing must fail")
	}
}

func TestSpeakerWakeStatusReportsState(t *testing.T) {
	f := newWakeFixture(t, wakePrettyConfig)
	if out := f.mustRun("status", ""); !strings.Contains(out, "enabled: no") || !strings.Contains(out, "firmware support: yes") || !strings.Contains(out, "chosen in the app: alisa") {
		t.Fatalf("status: %s", out)
	}
	f.mustRun("apply", "")
	if out := f.mustRun("status", ""); !strings.Contains(out, `second word "yandex"`) || !strings.Contains(out, "made by this tool") {
		t.Fatalf("status: %s", out)
	}
	if _, err := f.run("bogus", ""); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestFileContainsFindsNeedleAcrossReadBoundary(t *testing.T) {
	dir := t.TempDir()
	needle := []byte("customActivation")
	for _, offset := range []int{0, 1<<20 - 8, 1<<20 - 1, 1 << 20, 3<<20 - 5} {
		data := bytes.Repeat([]byte{'x'}, 3<<20+100)
		copy(data[offset:], needle)
		path := filepath.Join(dir, "maind")
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
		if ok, err := fileContains(path, needle); err != nil || !ok {
			t.Fatalf("needle at %d not found: %v %v", offset, ok, err)
		}
	}
	path := filepath.Join(dir, "other")
	os.WriteFile(path, bytes.Repeat([]byte{'x'}, 2<<20), 0644)
	if ok, err := fileContains(path, needle); err != nil || ok {
		t.Fatalf("false positive: %v %v", ok, err)
	}
}

// A bind-mounted quasar.cfg cannot be replaced by rename (EBUSY); it is rewritten in place.
func TestSpeakerWakeRewritesInPlaceWhenConfigCannotBeReplaced(t *testing.T) {
	f := newWakeFixture(t, wakePrettyConfig)
	original := renameFile
	t.Cleanup(func() { renameFile = original })
	renameFile = func(string, string) error { return &os.LinkError{Op: "rename", Err: syscall.EBUSY} }
	f.mustRun("apply", "yandex")
	if !strings.Contains(f.read(f.paths.Config), `"forcePhrase":"yandex"`) {
		t.Fatal("config not rewritten in place")
	}
	f.mustRun("rollback", "")
	if f.read(f.paths.Config) != wakePrettyConfig {
		t.Fatal("rollback did not restore the config in place")
	}
	// Other failures stay failures.
	renameFile = func(string, string) error { return &os.LinkError{Op: "rename", Err: syscall.EACCES} }
	if _, err := f.run("apply", "yandex"); err == nil {
		t.Fatal("unrelated rename error must fail")
	}
	if f.read(f.paths.Config) != wakePrettyConfig || f.exists(f.paths.slotPath()) {
		t.Fatal("failed apply left changes")
	}
}
