// Package assistant runs built-in and configured MCP tools for LLM answers.
package assistant

import (
	"errors"
	"net/url"
	"strings"
	"time"
	_ "time/tzdata" // Speaker firmware may not have an IANA zoneinfo database.
)

type Config struct {
	MCPServers       []MCPServerConfig `json:"mcp_servers,omitempty"`
	Enabled          bool              `json:"enabled"`
	Web              bool              `json:"web"`
	History          bool              `json:"history"`
	Time             bool              `json:"time"`
	Updates          bool              `json:"updates"`
	PlaySound        bool              `json:"play_sound"`
	MusicBaseURL     string            `json:"music_base_url,omitempty"`
	Timezone         string            `json:"timezone"`
	MaxRounds        int               `json:"max_rounds"`
	MaxCalls         int               `json:"max_calls"`
	WebTimeoutMS     int               `json:"web_timeout_ms"`
	WebMaxBytes      int               `json:"web_max_bytes"`
	WebMaxChars      int               `json:"web_max_chars"`
	HistoryMaxScanMB int               `json:"history_max_scan_mb"`
}

func DefaultConfig() Config {
	return Config{Enabled: false, Updates: true, Web: true, History: true, Time: true, Timezone: "UTC", MaxRounds: 3, MaxCalls: 4, WebTimeoutMS: 8000, WebMaxBytes: 1 << 20, WebMaxChars: 12000, HistoryMaxScanMB: 16}
}
func (c Config) Validate() error {
	if c.MusicBaseURL != "" {
		u, err := url.Parse(c.MusicBaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.ContainsAny(c.MusicBaseURL, "#\r\n") || (u.Path != "" && u.Path != "/") {
			return errors.New("assistant_tools.music_base_url must be an HTTP/HTTPS origin without credentials, path, query or fragment")
		}
	}
	if err := validateMCP(c.MCPServers); err != nil {
		return err
	}
	for _, server := range c.MCPServers {
		if !server.Disabled && !c.Enabled {
			return errors.New("MCP servers require assistant_tools.enabled")
		}
	}
	if c.MaxRounds < 1 || c.MaxRounds > 5 || c.MaxCalls < 1 || c.MaxCalls > 10 || c.WebTimeoutMS < 100 || c.WebTimeoutMS > 20000 || c.WebMaxBytes < 1024 || c.WebMaxBytes > 2<<20 || c.WebMaxChars < 100 || c.WebMaxChars > 20000 || c.HistoryMaxScanMB < 1 || c.HistoryMaxScanMB > 64 {
		return errors.New("invalid assistant_tools limits")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return errors.New("invalid assistant_tools timezone")
	}
	return nil
}
