package yandex

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"path/filepath"
	"strings"

	"github.com/skrashevich/quasar-bridge/internal/localfile"
	account "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
	"google.golang.org/protobuf/proto"
)

// NewSpeaker reads the native account without refreshing or rewriting it.
// Credentials remain in memory and go only to the official Yandex host.
func NewSpeaker(configPath, timezone string) (*Client, error) {
	data, _, err := localfile.Read(configPath, localfile.Follow, 1<<20)
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
	// Absolute locations come from firmware or an operator-supplied config.
	// Relative names and the firmware's DATA placeholder use the speaker data directory.
	path := cmp.Or(cfg.Auth.Storage, "account_storage.dat")
	if rel, prefixed := strings.CutPrefix(path, "${DATA}/"); prefixed || !filepath.IsAbs(path) {
		path = rel
		if !filepath.IsLocal(path) {
			return nil, errors.New("invalid relative Yandex account storage path")
		}
		path = filepath.Join("/data/quasar", path)
	}
	data, _, err = localfile.Read(path, localfile.Follow, 1<<20)
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
