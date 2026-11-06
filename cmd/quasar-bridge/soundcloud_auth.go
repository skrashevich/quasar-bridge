package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"quasarbridge/internal/soundcloud"
)

const soundCloudTokenInputLimit = (16 << 10) + 16

func configureSoundCloudTokenPath(cfg *config, path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return errors.New("SoundCloud: не удалось определить путь настроек")
	}
	tokenPath := cfg.SoundCloud.WebTokenFile
	if tokenPath == "" {
		tokenPath = "soundcloud-session.json"
	}
	if !filepath.IsAbs(tokenPath) {
		tokenPath = filepath.Join(filepath.Dir(absolute), tokenPath)
	}
	cfg.SoundCloud.WebTokenFile = filepath.Clean(tokenPath)
	return nil
}

func soundCloudAuthTokenPath(cfg config, path string) (string, error) {
	tokenPath := cfg.SoundCloud.WebTokenFile
	if tokenPath == "" || strings.ContainsFunc(tokenPath, unicode.IsControl) {
		return "", errors.New("SoundCloud: некорректный путь файла веб-сессии")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("SoundCloud: не удалось определить путь настроек")
	}
	protected := []string{absolute, filepath.Join(filepath.Dir(absolute), "runtime.env")}
	if official := cfg.SoundCloud.TokenFile; official != "" {
		resolved, err := filepath.Abs(official)
		if err != nil {
			return "", errors.New("SoundCloud: некорректный путь официальной авторизации")
		}
		protected = append(protected, resolved)
		if !filepath.IsAbs(official) {
			protected = append(protected, filepath.Join(filepath.Dir(absolute), official))
		}
	}
	for _, other := range protected {
		if filepath.Clean(other) == tokenPath {
			return "", errors.New("SoundCloud: файл веб-сессии не должен совпадать с настройками или официальной авторизацией")
		}
	}
	if info, err := os.Lstat(tokenPath); err == nil && !info.Mode().IsRegular() {
		return "", errors.New("SoundCloud: файл веб-сессии должен быть обычным файлом")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("SoundCloud: файл веб-сессии недоступен")
	}
	return tokenPath, nil
}

func readSoundCloudWebToken(in io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(in, soundCloudTokenInputLimit+1)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) || len(line) > soundCloudTokenInputLimit {
		return "", errors.New("SoundCloud: не удалось прочитать токен из первой строки stdin")
	}
	return soundcloud.NormalizeWebToken(line)
}

func runSoundCloudAuth(ctx context.Context, path, action string, in io.Reader, out io.Writer) error {
	return runSoundCloudAuthHTTP(ctx, path, action, in, out, nil)
}

func runSoundCloudAuthHTTP(ctx context.Context, path, action string, in io.Reader, out io.Writer, hc *http.Client) error {
	switch action {
	case "login", "status", "logout", "token-file":
	default:
		return errors.New("soundcloud-auth: допустимы login, status или logout")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("SoundCloud: не удалось прочитать настройки")
	}
	cfg, err := parseConfig(raw, true)
	if err != nil {
		return err
	}
	if err = configureSoundCloudTokenPath(&cfg, path); err != nil {
		return err
	}
	tokenPath, err := soundCloudAuthTokenPath(cfg, path)
	if err != nil {
		return err
	}
	if action == "token-file" {
		_, err = fmt.Fprintln(out, tokenPath)
		return err
	}
	if action == "logout" {
		if cfg.SoundCloud.WebAccessToken != "" {
			return errors.New("SoundCloud: web_access_token или QUASAR_SOUNDCLOUD_WEB_ACCESS_TOKEN перекрывает файл; удалите это значение перед выходом")
		}
		updated, err := soundCloudAuthConfig(raw, tokenPath, false)
		if err != nil {
			return err
		}
		if err = persistSoundCloudSession(path, raw, updated, tokenPath, "", true, writeConfiguration); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "SoundCloud: веб-сессия удалена. Перезапустите quasar-bridge.")
		return err
	}
	if action == "login" && os.Getenv("QUASAR_SOUNDCLOUD_WEB_ACCESS_TOKEN") != "" {
		return errors.New("SoundCloud: удалите QUASAR_SOUNDCLOUD_WEB_ACCESS_TOKEN перед сохранением новой веб-сессии")
	}
	baseline, err := snapshotSoundCloudFile(tokenPath)
	if err != nil {
		return err
	}
	auth := cfg.SoundCloud
	auth.API = "web"
	if action == "login" {
		token, err := readSoundCloudWebToken(in)
		if err != nil {
			return err
		}
		auth.WebAccessToken, auth.WebTokenFile = token, ""
	}
	if action == "status" && auth.WebAccessToken == "" {
		if _, err := os.Stat(tokenPath); errors.Is(err, os.ErrNotExist) {
			return writeSoundCloudAccountStatus(out, nil)
		}
	}
	if hc == nil {
		hc, err = soundcloud.NewHTTPClient(auth.ProxyURL)
		if err != nil {
			return errors.New("SoundCloud: некорректные настройки прокси")
		}
	}
	client, err := soundcloud.NewClient(auth, hc)
	if err != nil {
		if action == "status" {
			_ = writeSoundCloudAccountStatus(out, nil)
		}
		return errors.New("SoundCloud: не удалось загрузить веб-сессию")
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	account, err := client.Account(bounded)
	if err != nil {
		if action == "status" {
			_ = writeSoundCloudAccountStatus(out, nil)
		}
		return errors.New("SoundCloud: не удалось подтвердить учётную запись; проверьте токен и доступ через настроенный прокси")
	}
	if action == "login" {
		now, snapshotErr := snapshotSoundCloudFile(tokenPath)
		if snapshotErr != nil || !sameSoundCloudSnapshot(baseline, now) {
			return errors.New("SoundCloud: веб-сессия изменена извне; повторите вход")
		}
		updated, err := soundCloudAuthConfig(raw, tokenPath, true)
		if err != nil {
			return err
		}
		if err = persistSoundCloudSession(path, raw, updated, tokenPath, auth.WebAccessToken, false, writeConfiguration); err != nil {
			return err
		}
		if _, err = fmt.Fprintln(out, "SoundCloud: вход выполнен. Перезапустите quasar-bridge."); err != nil {
			return err
		}
	}
	return writeSoundCloudAccountStatus(out, &account)
}

func writeSoundCloudAccountStatus(out io.Writer, account *soundcloud.Account) error {
	value := struct {
		Authenticated bool                `json:"authenticated"`
		Account       *soundcloud.Account `json:"account,omitempty"`
	}{account != nil, account}
	if err := json.MarshalWrite(out, value); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out)
	return err
}

func soundCloudAuthConfig(raw []byte, tokenPath string, login bool) ([]byte, error) {
	var doc map[string]jsontext.Value
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return nil, errors.New("SoundCloud: некорректные настройки")
	}
	tokenValue, _ := json.Marshal(tokenPath)
	for key, value := range map[string]jsontext.Value{"soundcloud.api": jsontext.Value(`"web"`), "soundcloud.web_token_file": tokenValue} {
		if err := setConfigValue(doc, key, value); err != nil {
			return nil, errors.New("SoundCloud: не удалось обновить настройки")
		}
	}
	if login {
		for key, value := range map[string]jsontext.Value{"soundcloud.enabled": jsontext.Value(`true`), "soundcloud.web_access_token": jsontext.Value(`""`), "mode": jsontext.Value(`"hybrid"`)} {
			if err := setConfigValue(doc, key, value); err != nil {
				return nil, errors.New("SoundCloud: не удалось обновить настройки")
			}
		}
	}
	data, err := json.Marshal(doc, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return nil, errors.New("SoundCloud: не удалось подготовить настройки")
	}
	if _, err = parseConfig(data, true); err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

type soundCloudFileSnapshot struct {
	data   []byte
	mode   os.FileMode
	exists bool
}

func snapshotSoundCloudFile(path string) (soundCloudFileSnapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return soundCloudFileSnapshot{}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return soundCloudFileSnapshot{}, errors.New("SoundCloud: файл веб-сессии недоступен")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return soundCloudFileSnapshot{}, errors.New("SoundCloud: не удалось прочитать веб-сессию")
	}
	return soundCloudFileSnapshot{data, info.Mode().Perm(), true}, nil
}

func sameSoundCloudSnapshot(a, b soundCloudFileSnapshot) bool {
	return a.exists == b.exists && a.mode == b.mode && bytes.Equal(a.data, b.data)
}

// Configuration and the dedicated session file commit together. Rollback only
// touches the bytes this operation wrote; external replacements are preserved.
func persistSoundCloudSession(path string, original, updated []byte, tokenPath, token string, remove bool, writeConfig func(string, []byte, os.FileMode) error) error {
	old, err := snapshotSoundCloudFile(tokenPath)
	if err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		return errors.New("SoundCloud: настройки изменены извне, веб-сессия сохранена без изменений")
	}
	if remove {
		err = soundcloud.RemoveWebToken(tokenPath)
	} else {
		err = soundcloud.SaveWebToken(tokenPath, token)
	}
	if err != nil {
		return errors.New("SoundCloud: не удалось изменить файл веб-сессии")
	}
	written, err := snapshotSoundCloudFile(tokenPath)
	if err != nil {
		return err
	}
	current, readErr := os.ReadFile(path)
	if readErr == nil && bytes.Equal(current, original) {
		err = writeConfig(path, updated, 0600)
	} else {
		err = errors.New("configuration changed")
	}
	if err == nil {
		return nil
	}
	now, snapshotErr := snapshotSoundCloudFile(tokenPath)
	if snapshotErr != nil || !sameSoundCloudSnapshot(now, written) {
		return errors.New("SoundCloud: настройки не сохранены; внешние изменения веб-сессии оставлены без изменений")
	}
	if old.exists {
		err = writeConfiguration(tokenPath, old.data, old.mode&0600)
	} else {
		err = os.Remove(tokenPath)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	if err != nil {
		return errors.New("SoundCloud: настройки не сохранены; не удалось восстановить прежнюю веб-сессию")
	}
	return errors.New("SoundCloud: настройки не сохранены; прежняя веб-сессия восстановлена")
}
