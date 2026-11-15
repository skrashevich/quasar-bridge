package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// The device controller must work without wget or curl on Android firmware.
func checkHealth(ctx context.Context, address string) error {
	return checkHealthWithBackend(ctx, address, false)
}

func checkHealthWithBackend(ctx context.Context, address string, requireAndroid bool) error {
	u, err := url.Parse(address)
	if err != nil {
		return errors.New("invalid loopback health URL")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.IsLoopback() || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/healthz" {
		return errors.New("invalid loopback health URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return errors.New("invalid loopback health URL")
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("bridge health check failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	var health struct {
		Status         string `json:"status"`
		AndroidBackend bool   `json:"android_backend_proxy"`
	}
	if err != nil || len(data) > 4096 || resp.StatusCode != http.StatusOK || json.Unmarshal(data, &health) != nil || health.Status != "ok" || requireAndroid && !health.AndroidBackend {
		return errors.New("bridge is not healthy")
	}
	return nil
}
