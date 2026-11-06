package soundcloud

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const webSessionKind = "soundcloud_web_session"

// A queued write or an HTML/challenge response does not confirm a like. Only
// inspect response structure: track titles and other free text are not markers.
func confirmedWebMutation(status int, body []byte) bool {
	if status != http.StatusOK && status != http.StatusCreated && status != http.StatusNoContent {
		return false
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return true
	}
	var reply map[string]any
	if json.Unmarshal(body, &reply) != nil || reply == nil {
		return false
	}
	for key, value := range reply {
		switch strings.ToLower(key) {
		case "error", "errors", "challenge", "captcha", "captcha_url", "captchaurl":
			switch value := value.(type) {
			case nil:
			case bool:
				if value {
					return false
				}
			case string:
				if value != "" {
					return false
				}
			case []any:
				if len(value) != 0 {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

type webSessionToken struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Token   string `json:"access_token"`
}

// Account contains public identity metadata, never account credentials.
type Account struct {
	ID       int64  `json:"id"`
	URN      string `json:"urn,omitempty"`
	Username string `json:"username"`
}

// NormalizeWebToken accepts a raw token or an OAuth Authorization header value.
func NormalizeWebToken(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	if prefix, rest, ok := strings.Cut(token, " "); ok && strings.EqualFold(prefix, "OAuth") {
		token = strings.TrimSpace(rest)
	}
	if token == "" || strings.EqualFold(token, "OAuth") || len(token) > 16<<10 {
		return "", errors.New("SoundCloud: некорректный токен веб-сессии")
	}
	for _, r := range token {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~+/=", r)) {
			return "", errors.New("SoundCloud: некорректный токен веб-сессии")
		}
	}
	return token, nil
}

func readWebToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("SoundCloud: файл веб-сессии должен быть обычным файлом")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("SoundCloud: не удалось прочитать файл веб-сессии")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return "", errors.New("SoundCloud: не удалось прочитать файл веб-сессии")
	}
	var session webSessionToken
	if json.Unmarshal(raw, &session, json.RejectUnknownMembers(true)) != nil || session.Version != 1 || session.Kind != webSessionKind {
		return "", errors.New("SoundCloud: некорректный формат файла веб-сессии")
	}
	return NormalizeWebToken(session.Token)
}

func (c *Client) loadWebToken() error {
	if c.cfg.WebAccessToken != "" {
		token, err := NormalizeWebToken(c.cfg.WebAccessToken)
		c.webToken = token
		return err
	}
	if c.cfg.WebTokenFile == "" {
		return nil
	}
	token, err := readWebToken(c.cfg.WebTokenFile)
	c.webToken = token
	return err
}

// SaveWebToken persists only a dedicated, typed web-session file. Call Account
// before saving to validate the user's principal with SoundCloud.
func SaveWebToken(path, rawToken string) error {
	if path == "" {
		return errors.New("SoundCloud: не задан файл веб-сессии")
	}
	token, err := NormalizeWebToken(rawToken)
	if err != nil {
		return err
	}
	if _, err := readWebToken(path); err != nil {
		return err
	}
	raw, err := json.Marshal(webSessionToken{Version: 1, Kind: webSessionKind, Token: token})
	if err != nil {
		return errors.New("SoundCloud: не удалось подготовить веб-сессию")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".soundcloud-session-*")
	if err != nil {
		return errors.New("SoundCloud: не удалось сохранить веб-сессию")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return errors.New("SoundCloud: не удалось сохранить веб-сессию")
	}
	return nil
}

// RemoveWebToken refuses to remove an official token file or another document.
func RemoveWebToken(path string) error {
	if path == "" {
		return errors.New("SoundCloud: не задан файл веб-сессии")
	}
	if _, err := readWebToken(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("SoundCloud: не удалось удалить веб-сессию")
	}
	return nil
}

// Account validates the selected user principal. Web sessions are never
// renewed through the official application's client-credentials grant.
func (c *Client) Account(ctx context.Context) (Account, error) {
	if !c.authorizedUser() {
		return Account{}, ErrAuthorization
	}
	raw, err := c.api(ctx, http.MethodGet, c.base()+"/me", nil)
	if err != nil {
		return Account{}, err
	}
	var account Account
	if json.Unmarshal(raw, &account) != nil {
		return Account{}, errors.New("SoundCloud: некорректный ответ профиля пользователя")
	}
	if account.URN != "" {
		numeric, ok := strings.CutPrefix(account.URN, "soundcloud:users:")
		id, err := strconv.ParseInt(numeric, 10, 64)
		if !ok || err != nil || id <= 0 || strings.IndexFunc(numeric, func(r rune) bool { return r < '0' || r > '9' }) != -1 || account.ID != 0 && account.ID != id {
			return Account{}, errors.New("SoundCloud: некорректный ответ профиля пользователя")
		}
		account.ID = id
	}
	if account.ID <= 0 {
		return Account{}, errors.New("SoundCloud: некорректный ответ профиля пользователя")
	}
	return account, nil
}

func itoa64(id int64) string { return strconv.FormatInt(id, 10) }

// personalTrack hydrates a track only for an authenticated user. Public search
// and Resolve retain the stricter catalogItem.track filtering.
func (c *Client) personalTrack(ctx context.Context, urn string) (Track, error) {
	if !c.authorizedUser() {
		return Track{}, ErrAuthorization
	}
	id, err := c.resourcePath(urn, "tracks")
	if err != nil {
		return Track{}, err
	}
	raw, err := c.api(ctx, http.MethodGet, c.base()+"/tracks/"+id, nil)
	if err != nil {
		if status, ok := errors.AsType[*httpStatusError](err); ok && (status.Code == http.StatusNotFound || status.Code == http.StatusGone) {
			return Track{}, ErrUnavailable
		}
		return Track{}, err
	}
	var item catalogItem
	if json.Unmarshal(raw, &item) != nil {
		return Track{}, errors.New("SoundCloud: некорректный ответ личного трека")
	}
	track, ok := item.personalTrack()
	if !ok {
		return Track{}, ErrUnavailable
	}
	return track, nil
}
