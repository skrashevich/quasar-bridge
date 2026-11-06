package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
)

// configureSoundCloud changes only the installer's selected fields. Credentials,
// relay addresses and unrelated configuration retain their saved values.
func configureSoundCloud(path, choice string) error {
	enabled, prefer, compare := true, false, false
	switch choice {
	case "off":
		enabled = false
	case "explicit":
	case "default":
		prefer = true
	case "compare-yandex":
		compare = true
	case "compare-soundcloud":
		prefer, compare = true, true
	default:
		return errors.New("unknown SoundCloud setup mode")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read configuration file")
	}
	var doc map[string]jsontext.Value
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return errors.New("invalid configuration JSON")
	}
	for key, value := range map[string]bool{"enabled": enabled, "default": prefer, "compare_yandex": compare} {
		encoded, _ := json.Marshal(value)
		if err := setConfigValue(doc, "soundcloud."+key, encoded); err != nil {
			return err
		}
	}
	if enabled {
		// Existing official/auto credentials remain usable; new installs use the
		// public catalog without requesting account credentials.
		if bytes.Equal(configurationValue(raw, "soundcloud.api"), []byte("null")) {
			if err := setConfigValue(doc, "soundcloud.api", jsontext.Value(`"web"`)); err != nil {
				return err
			}
		}
		if err := setConfigValue(doc, "mode", jsontext.Value(`"hybrid"`)); err != nil {
			return err
		}
	}
	data, err := json.Marshal(doc, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return errors.New("cannot encode configuration")
	}
	if _, err := parseConfig(data, false); err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, current) {
		return errors.New("configuration changed externally")
	}
	return writeConfiguration(path, append(data, '\n'), 0600)
}
