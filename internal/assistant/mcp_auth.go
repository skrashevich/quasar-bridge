package assistant

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"quasarbridge/internal/mcpoauth"
)

func (c MCPServerConfig) oauthBinding() mcpoauth.Binding {
	return mcpoauth.Binding{
		Name: c.Name, Endpoint: c.endpoint(), Issuer: c.OAuth.Issuer,
		ClientID: c.OAuth.ClientID, Scopes: slices.Clone(c.OAuth.Scopes),
	}
}

func MCPAuth(ctx context.Context, configs []MCPServerConfig, store *mcpoauth.Store, action, name, listen string, out io.Writer) error {
	index := slices.IndexFunc(configs, func(c MCPServerConfig) bool { return c.Name == name })
	if index < 0 || configs[index].OAuth == nil {
		return errors.New("MCP OAuth server is not configured; select it with -mcp-server")
	}
	cfg := configs[index]
	switch action {
	case "status":
		status, err := MCPOAuthStatus(store, cfg)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, status)
		return err
	case "logout":
		if err := store.Delete(ctx, name); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "logged out locally")
		return err
	case "login":
		if cfg.Disabled {
			return errors.New("MCP OAuth server is disabled")
		}
		return loginMCP(ctx, store, cfg, listen, out)
	default:
		return errors.New("-mcp-auth must be login, status or logout")
	}
}

func MCPOAuthStatus(store *mcpoauth.Store, cfg MCPServerConfig) (string, error) {
	if cfg.OAuth == nil {
		return "", errors.New("MCP OAuth server is not configured")
	}
	return localMCPAuthStatus(store, cfg.oauthBinding(), time.Now())
}

func localMCPAuthStatus(store *mcpoauth.Store, b mcpoauth.Binding, now time.Time) (string, error) {
	r, err := store.Load(b)
	switch {
	case errors.Is(err, mcpoauth.ErrNotSignedIn):
		return "not signed in", nil
	case errors.Is(err, mcpoauth.ErrSignInRequired):
		return "sign-in required", nil
	case err != nil:
		return "", err
	case r.NeedsLogin:
		return "sign-in required", nil
	case r.AccessToken != "" && (r.Expiry.IsZero() || now.Before(r.Expiry)):
		return "access token valid", nil
	case r.RefreshToken != "":
		return "refresh needed", nil
	default:
		return "sign-in required", nil
	}
}

type MCPLoginAttempt interface {
	Handler() auth.OAuthHandler
	Record() (mcpoauth.Record, error)
}

func NewMCPPanelAttempt(cfg MCPServerConfig, publish func(context.Context, string) error) (*mcpoauth.Attempt, error) {
	if cfg.Disabled || cfg.OAuth == nil || validateMCP([]MCPServerConfig{cfg}) != nil {
		return nil, errors.New("MCP OAuth configuration is not available")
	}
	return mcpoauth.NewAttempt(cfg.oauthBinding(), mcpoauth.ManualRedirect, publish, nil)
}

func ExchangeMCPLogin(ctx context.Context, cfg MCPServerConfig, attempt MCPLoginAttempt) (mcpoauth.Record, error) {
	session, closeSession, err := openMCPSessionWithOAuth(ctx, cfg, attempt.Handler(), nil)
	if err != nil {
		if ctx.Err() != nil {
			return mcpoauth.Record{}, ctx.Err()
		}
		if errors.Is(err, mcpoauth.ErrLoginDenied) {
			return mcpoauth.Record{}, mcpoauth.ErrLoginDenied
		}
		return mcpoauth.Record{}, errors.New("MCP OAuth sign-in did not complete or authenticated initialization failed")
	}
	defer closeSession()
	defer session.Close()
	return attempt.Record()
}

func loginMCP(ctx context.Context, store *mcpoauth.Store, cfg MCPServerConfig, listen string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	login, err := mcpoauth.NewLogin(cfg.oauthBinding(), listen, out, nil)
	if err != nil {
		return err
	}
	defer login.Close()
	record, err := ExchangeMCPLogin(ctx, cfg, login)
	if err != nil {
		return err
	}
	if err := store.Save(ctx, record); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "MCP OAuth sign-in complete")
	return err
}
