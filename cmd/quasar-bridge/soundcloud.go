package main

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"quasarbridge/internal/soundcloud"
)

func configureSoundCloudOrigin(cfg *config) error {
	cfg.SoundCloud.Defaults()
	if cfg.SoundCloud.Enabled && cfg.Mode != "hybrid" {
		return errors.New("soundcloud: enabled provider requires hybrid mode")
	}
	if err := cfg.SoundCloud.Validate(); err != nil {
		return err
	}
	if cfg.SoundCloud.BaseURL != "" {
		u, err := url.Parse(cfg.SoundCloud.BaseURL)
		if err != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") {
			return errors.New("soundcloud: base_url must be an HTTP origin without a path")
		}
		return nil
	}
	if !cfg.SoundCloud.Enabled {
		return nil
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return errors.New("soundcloud: listen must include host and port")
	}
	ip, ipErr := netip.ParseAddr(host)
	if cfg.TLSCert != "" || port == "0" || (host != "localhost" && (ipErr != nil || !ip.IsLoopback())) {
		return errors.New("soundcloud: base_url reachable by the speaker is required for TLS or non-loopback listen")
	}
	cfg.SoundCloud.BaseURL = "http://" + net.JoinHostPort(host, port)
	return nil
}

func newSoundCloudProvider(cfg config) (*soundcloud.Provider, *soundcloud.Relay, error) {
	if !cfg.SoundCloud.Enabled || cfg.Mode != "hybrid" {
		return nil, nil, nil
	}
	if err := configureSoundCloudOrigin(&cfg); err != nil {
		return nil, nil, err
	}
	httpClient, err := soundcloud.NewHTTPClient(cfg.SoundCloud.ProxyURL)
	if err != nil {
		return nil, nil, err
	}
	client, err := soundcloud.NewClient(cfg.SoundCloud, httpClient)
	if err != nil {
		return nil, nil, err
	}
	relay, err := soundcloud.NewRelay(cfg.SoundCloud.BaseURL, httpClient)
	if err != nil {
		return nil, nil, err
	}
	return soundcloud.NewProvider(cfg.SoundCloud, client, relay), relay, nil
}

func soundCloudHealth(cfg config, active bool, providers ...interface{ IsDefault() bool }) map[string]any {
	value := cfg.SoundCloud.Default && active
	if len(providers) > 0 {
		value = active && providers[0] != nil && providers[0].IsDefault()
	}
	return map[string]any{"enabled": cfg.SoundCloud.Enabled, "active": active, "provider": "SoundCloud", "api": cfg.SoundCloud.API, "default": value, "compare_yandex": active && cfg.SoundCloud.CompareYandex}
}
