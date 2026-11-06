package soundcloud

import (
	"errors"
	"net/url"
)

type Config struct {
	Enabled        bool   `json:"enabled,omitzero"`
	Default        bool   `json:"default,omitzero"`
	CompareYandex  bool   `json:"compare_yandex,omitzero"`
	API            string `json:"api,omitempty"`
	ClientID       string `json:"client_id,omitempty"`
	ClientSecret   string `json:"client_secret,omitempty"`
	AccessToken    string `json:"access_token,omitempty"`
	RefreshToken   string `json:"refresh_token,omitempty"`
	TokenFile      string `json:"token_file,omitempty"`
	WebAccessToken string `json:"web_access_token,omitempty"`
	WebTokenFile   string `json:"web_token_file,omitempty"`
	BaseURL        string `json:"base_url,omitempty"`
	ProxyURL       string `json:"proxy_url,omitempty"`
	QueueLimit     int    `json:"queue_limit,omitzero"`
}

func (c *Config) Defaults() {
	if c.API == "" {
		c.API = "auto"
	}
	if c.QueueLimit == 0 {
		c.QueueLimit = 50
	}
}
func (c Config) Validate() error {
	c.Defaults()
	if _, err := parseProxyURL(c.ProxyURL); err != nil {
		return err
	}
	if c.API != "auto" && c.API != "official" && c.API != "web" {
		return errors.New("soundcloud: api must be auto, official or web")
	}
	if c.QueueLimit < 1 || c.QueueLimit > 100 {
		return errors.New("soundcloud: queue_limit must be 1..100")
	}
	if c.BaseURL != "" {
		u, e := url.Parse(c.BaseURL)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("soundcloud: invalid base_url")
		}
	}
	return nil
}
