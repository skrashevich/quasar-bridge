package main

import (
	"context"
	"io"
	"log/slog"

	"github.com/skrashevich/quasar-bridge/internal/assistant"
	"github.com/skrashevich/quasar-bridge/internal/mcpoauth"
)

func runMCPAuth(ctx context.Context, cfg config, path, action, name, listen string, out io.Writer) error {
	store, err := mcpoauth.NewStore(path)
	if err != nil {
		return err
	}
	return assistant.MCPAuth(ctx, cfg.AssistantTools.MCPServers, store, action, name, listen, out)
}

func mcpOAuthSource(path string, servers []assistant.MCPServerConfig) *mcpoauth.Source {
	for _, server := range servers {
		if server.OAuth == nil || server.Disabled {
			continue
		}
		store, err := mcpoauth.NewStore(path)
		if err != nil {
			slog.Warn("MCP OAuth storage is unavailable", "error", err)
			return nil
		}
		return &mcpoauth.Source{Store: store}
	}
	return nil
}
