// Package mcpoauth manages OAuth credentials for configured MCP servers.
package mcpoauth

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

const maxCredentialBytes = 1 << 20

var (
	ErrNotSignedIn    = errors.New("MCP OAuth server is not signed in")
	ErrSignInRequired = errors.New("MCP OAuth sign-in required; run -mcp-auth login")
	serverName        = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,19}$`)
)

type Binding struct {
	Name     string   `json:"name"`
	Endpoint string   `json:"endpoint"`
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes"`
}

type Record struct {
	Binding       Binding   `json:"binding"`
	TokenEndpoint string    `json:"token_endpoint,omitempty"`
	AccessToken   string    `json:"access_token,omitempty"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	Expiry        time.Time `json:"expiry,omitzero"`
	NeedsLogin    bool      `json:"needs_login,omitzero"`
}

type Store struct{ dir string }

type diskRecord struct {
	Version int    `json:"version"`
	Record  Record `json:"record"`
}

func NewStore(configPath string) (*Store, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, errors.New("cannot resolve MCP OAuth configuration path")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, errors.New("cannot resolve MCP OAuth configuration path")
	}
	hash := sha256.Sum256([]byte(resolved))
	return &Store{dir: filepath.Join(filepath.Dir(resolved), ".mcp-oauth", fmt.Sprintf("%x", hash))}, nil
}

func (s *Store) path(name string) (string, error) {
	if s == nil || s.dir == "" || !serverName.MatchString(name) {
		return "", errors.New("invalid MCP OAuth credential target")
	}
	return filepath.Join(s.dir, name+".json"), nil
}

func (s *Store) privateDirectories(create bool) error {
	for _, path := range []string{filepath.Dir(s.dir), s.dir} {
		if create {
			if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return errors.New("cannot create MCP OAuth storage directory")
			}
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && !create {
			return ErrNotSignedIn
		}
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return errors.New("MCP OAuth storage directory must be a private directory (0700)")
		}
	}
	return nil
}

func (s *Store) Load(b Binding) (Record, error) {
	path, err := s.path(b.Name)
	if err != nil {
		return Record{}, err
	}
	if err := s.privateDirectories(false); err != nil {
		return Record{}, err
	}
	return loadRecord(path, b)
}

func loadRecord(path string, b Binding) (Record, error) {
	f, err := openPrivate(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, ErrNotSignedIn
	}
	if err != nil {
		return Record{}, errors.New("cannot open protected MCP OAuth credentials")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return Record{}, errors.New("MCP OAuth credentials must be a private regular file (0600)")
	}
	body, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
	var stored diskRecord
	if err != nil || len(body) > maxCredentialBytes || json.Unmarshal(body, &stored, json.RejectUnknownMembers(true)) != nil || stored.Version != 1 {
		return Record{}, errors.New("invalid MCP OAuth credential record")
	}
	if !sameBinding(stored.Record.Binding, b) {
		return Record{}, ErrSignInRequired
	}
	return stored.Record, nil
}

func sameBinding(a, b Binding) bool {
	return a.Name == b.Name && a.Endpoint == b.Endpoint && a.Issuer == b.Issuer &&
		a.ClientID == b.ClientID && slices.Equal(sortedScopes(a.Scopes), sortedScopes(b.Scopes))
}

func sortedScopes(scopes []string) []string {
	out := slices.Clone(scopes)
	slices.Sort(out)
	return out
}

func (s *Store) Save(ctx context.Context, r Record) error {
	path, err := s.path(r.Binding.Name)
	if err != nil {
		return err
	}
	if err := s.privateDirectories(true); err != nil {
		return err
	}
	unlock, err := lockCredential(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	return saveRecord(ctx, path, r)
}

func (s *Store) Delete(ctx context.Context, name string) error {
	path, err := s.path(name)
	if err != nil {
		return err
	}
	if err := s.privateDirectories(false); err != nil {
		if errors.Is(err, ErrNotSignedIn) {
			return nil
		}
		return err
	}
	unlock, err := lockCredential(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot remove MCP OAuth credentials")
	}
	return nil
}

func (s *Store) Update(ctx context.Context, b Binding, change func(Record) (Record, error)) (Record, error) {
	path, err := s.path(b.Name)
	if err != nil {
		return Record{}, err
	}
	if err := s.privateDirectories(false); err != nil {
		return Record{}, err
	}
	unlock, err := lockCredential(ctx, path)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	current, err := loadRecord(path, b)
	if err != nil {
		return Record{}, err
	}
	next, err := change(current)
	if err != nil {
		return Record{}, err
	}
	if !sameBinding(next.Binding, b) {
		return Record{}, errors.New("MCP OAuth update cannot change the credential binding")
	}
	if current.TokenEndpoint == next.TokenEndpoint && current.AccessToken == next.AccessToken &&
		current.RefreshToken == next.RefreshToken && current.Expiry.Equal(next.Expiry) && current.NeedsLogin == next.NeedsLogin {
		return next, nil
	}
	if err := saveRecord(ctx, path, next); err != nil {
		return Record{}, err
	}
	return next, nil
}

func saveRecord(ctx context.Context, path string, r Record) error {
	body, err := json.Marshal(diskRecord{Version: 1, Record: r})
	if err != nil || len(body) > maxCredentialBytes {
		return errors.New("cannot encode MCP OAuth credentials")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".credential-*")
	if err != nil {
		return errors.New("cannot write MCP OAuth credentials")
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(body)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot write MCP OAuth credentials")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if os.Rename(temp, path) != nil {
		return errors.New("cannot write MCP OAuth credentials")
	}
	return nil
}
