package yandex

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	account "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
	"google.golang.org/protobuf/proto"
)

// NewSpeaker reads the native account without refreshing or rewriting it.
// Credentials remain in memory and go only to the official Yandex host.
func NewSpeaker(configPath, timezone string) (*Client, error) {
	data, err := readNativeFile(configPath)
	if err != nil {
		return nil, errors.New("Yandex text queries require the native speaker configuration; run -query on the speaker")
	}
	var cfg struct {
		Aliced struct {
			APIKey     string `json:"apiKey"`
			AppID      string `json:"app_id"`
			AppVersion string `json:"app_version"`
		} `json:"aliced"`
		Auth struct {
			Storage string `json:"accountStorageFile"`
		} `json:"authd"`
	}
	if json.Unmarshal(data, &cfg) != nil || cfg.Aliced.APIKey == "" || cfg.Aliced.AppID == "" {
		return nil, errors.New("invalid native speaker configuration for Yandex text queries")
	}
	path := strings.ReplaceAll(cmp.Or(cfg.Auth.Storage, "account_storage.dat"), "${DATA}", "/data/quasar")
	if !filepath.IsAbs(path) {
		path = filepath.Join("/data/quasar", path)
	}
	data, err = readNativeFile(path)
	if err != nil {
		return nil, errors.New("Yandex speaker account storage is unavailable")
	}
	var storage account.AccountStorage
	if proto.Unmarshal(data, &storage) != nil {
		return nil, errors.New("invalid Yandex speaker account storage")
	}
	var selected *account.AccountInfo
	for _, a := range storage.GetAccounts() {
		if id := storage.GetCurrentAccountId(); id != 0 {
			if a.GetId() == id {
				selected = a
				break
			}
		} else if a.GetType() == account.AccountType_OWNER || len(storage.GetAccounts()) == 1 {
			if selected != nil {
				return nil, errors.New("Yandex speaker current account is ambiguous")
			}
			selected = a
		}
	}
	token := cmp.Or(selected.GetAuth().GetToken(), selected.GetAuthToken())
	if token == "" {
		return nil, errors.New("Yandex speaker current account has no authorization token")
	}
	return &Client{endpoint: "wss://uniproxy.alice.yandex.net/uni.ws", token: token, apiKey: cfg.Aliced.APIKey, appID: cfg.Aliced.AppID, appVersion: cfg.Aliced.AppVersion, timezone: timezone, source: configPath, accountID: selected.GetId()}, nil
}

func readNativeFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("invalid native file size or read failure")
	}
	return data, nil
}
