package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

type speakerBackup struct {
	Config       string         `json:"config"`
	Local        string         `json:"local"`
	URL          jsontext.Value `json:"url"`
	QUIC         jsontext.Value `json:"quic,omitempty"`
	Backend      jsontext.Value `json:"backend,omitempty"`
	BackendLocal string         `json:"backend_local,omitempty"`
}

// Patch only the two endpoints. All firmware-specific members remain intact.
func setSpeakerEndpoint(path, backupPath, local, action string) error {
	return setSpeakerEndpointForPlatform(path, backupPath, local, action, false)
}

// Android firmware receives an authoritative uniProxyUrl through backend sync.
// Its opt-in relay changes only that cloud field while retaining native sync.
func setSpeakerEndpointForPlatform(path, backupPath, local, action string, android bool) error {
	if action != "apply" && action != "rollback" {
		return errors.New("speaker-endpoint must be apply or rollback")
	}
	u, err := url.Parse(local)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/uni.ws" || (u.Scheme != "ws" && u.Scheme != "wss") {
		return errors.New("speaker URL must be a loopback WebSocket /uni.ws endpoint")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.IsLoopback() {
		return errors.New("speaker URL must use a loopback IP")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read speaker config")
	}
	var doc map[string]jsontext.Value
	if json.Unmarshal(raw, &doc) != nil {
		return errors.New("invalid speaker JSON")
	}
	var alice map[string]jsontext.Value
	if json.Unmarshal(doc["aliced"], &alice) != nil || alice == nil {
		return errors.New("missing aliced configuration")
	}
	var common map[string]jsontext.Value
	var backendLocal string
	if android {
		if json.Unmarshal(doc["common"], &common) != nil || common == nil {
			return errors.New("missing common configuration for Android backend relay")
		}
		backend := u.Clone()
		backend.Scheme = "http"
		if u.Scheme == "wss" {
			backend.Scheme = "https"
		}
		backend.Path = "/android-backend"
		backend.RawPath = ""
		backendLocal = backend.String()
	}
	var current string
	_ = json.Unmarshal(alice["uniProxyUrl"], &current)
	var saved speakerBackup
	previous, readErr := os.ReadFile(backupPath)
	if readErr == nil {
		if json.Unmarshal(previous, &saved) != nil || saved.Config != path || saved.Local != local || saved.BackendLocal != backendLocal || (saved.Backend != nil) != android {
			return errors.New("endpoint backup does not match this configuration")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return errors.New("cannot read endpoint backup")
	}
	if action == "apply" {
		if readErr != nil {
			saved = speakerBackup{Config: path, Local: local, URL: alice["uniProxyUrl"], QUIC: alice["uniProxyQuicUrl"]}
			if android {
				var originalBackend string
				if json.Unmarshal(common["backendUrl"], &originalBackend) != nil || strings.TrimSuffix(originalBackend, "/") != "https://quasar.yandex.net" {
					return errors.New("Android backend relay requires the official HTTPS Quasar backend")
				}
				saved.Backend, saved.BackendLocal = common["backendUrl"], backendLocal
			}
			if saved.URL == nil {
				return errors.New("missing original endpoint")
			}
			data, _ := json.Marshal(saved)
			if err := os.MkdirAll(filepath.Dir(backupPath), 0700); err != nil {
				return errors.New("cannot create endpoint backup directory")
			}
			f, err := os.OpenFile(backupPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return errors.New("cannot create endpoint backup")
			}
			_, err = f.Write(data)
			if err == nil {
				err = f.Sync()
			}
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				return errors.New("cannot save endpoint backup")
			}
		} else {
			expectedURL, expectedQUIC := saved.URL, saved.QUIC
			expectedBackend := saved.Backend
			if current == local {
				expectedURL, _ = json.Marshal(local)
				expectedQUIC = jsontext.Value(`""`)
				if android {
					expectedBackend, _ = json.Marshal(backendLocal)
				}
			}
			if !sameEndpoint(alice["uniProxyUrl"], expectedURL) || !sameEndpoint(alice["uniProxyQuicUrl"], expectedQUIC) || (android && !sameEndpoint(common["backendUrl"], expectedBackend)) {
				return errors.New("speaker endpoints changed externally; inspect the saved backup")
			}
		}
		alice["uniProxyUrl"], _ = json.Marshal(local)
		alice["uniProxyQuicUrl"] = jsontext.Value(`""`)
		if android {
			common["backendUrl"], _ = json.Marshal(backendLocal)
		}
	} else {
		if readErr != nil {
			return errors.New("no endpoint backup")
		}
		expectedBackend, _ := json.Marshal(backendLocal)
		if current != local || !sameEndpoint(alice["uniProxyQuicUrl"], jsontext.Value(`""`)) || (android && !sameEndpoint(common["backendUrl"], expectedBackend)) {
			return errors.New("speaker endpoints changed externally; refusing rollback")
		}
		alice["uniProxyUrl"] = saved.URL
		if android {
			common["backendUrl"] = saved.Backend
		}
		if saved.QUIC != nil {
			alice["uniProxyQuicUrl"] = saved.QUIC
		} else {
			delete(alice, "uniProxyQuicUrl")
		}
	}
	doc["aliced"], _ = json.Marshal(alice)
	if android {
		doc["common"], _ = json.Marshal(common)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".quasar-bridge-*")
	if err != nil {
		return errors.New("cannot stage speaker config; remount the filesystem writable")
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(info.Mode().Perm()); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot save speaker config")
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.New("cannot replace speaker config")
	}
	if action == "rollback" {
		return os.Remove(backupPath)
	}
	return nil
}

func sameEndpoint(a, b jsontext.Value) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
