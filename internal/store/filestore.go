// Package store persists server state (MCP OAuth clients and tokens, and the
// mailbox's Google OAuth token) in a single JSON file on a private volume.
// Codes and tokens are keyed by hashes, never raw values. The service is a
// single instance, so one process-wide mutex is sufficient.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/zachlatta/zrl-bot-email-control/internal/auth"
	"golang.org/x/oauth2"
)

type state struct {
	Clients       map[string]auth.Client `json:"clients"`
	Codes         map[string]auth.Code   `json:"codes"`
	Tokens        map[string]auth.Token  `json:"tokens"`
	RefreshTokens map[string]auth.Token  `json:"refresh_tokens"`
	GoogleToken   *oauth2.Token          `json:"google_token,omitempty"`
}

// FileStore implements auth.Store plus Google token storage.
type FileStore struct {
	path  string
	now   func() time.Time
	mu    sync.Mutex
	state state
}

// Open loads (or initializes) the state file at path.
func Open(path string, now func() time.Time) (*FileStore, error) {
	if now == nil {
		now = time.Now
	}
	store := &FileStore{path: path, now: now, state: emptyState()}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
		return store, store.saveLocked()
	case err != nil:
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return nil, fmt.Errorf("decode state %s: %w", path, err)
	}
	store.fillMaps()
	return store, nil
}

func emptyState() state {
	return state{
		Clients:       map[string]auth.Client{},
		Codes:         map[string]auth.Code{},
		Tokens:        map[string]auth.Token{},
		RefreshTokens: map[string]auth.Token{},
	}
}

func (s *FileStore) fillMaps() {
	empty := emptyState()
	if s.state.Clients == nil {
		s.state.Clients = empty.Clients
	}
	if s.state.Codes == nil {
		s.state.Codes = empty.Codes
	}
	if s.state.Tokens == nil {
		s.state.Tokens = empty.Tokens
	}
	if s.state.RefreshTokens == nil {
		s.state.RefreshTokens = empty.RefreshTokens
	}
}

func (s *FileStore) saveLocked() error {
	now := s.now()
	for key, code := range s.state.Codes {
		if !now.Before(code.ExpiresAt) {
			delete(s.state.Codes, key)
		}
	}
	for _, tokens := range []map[string]auth.Token{s.state.Tokens, s.state.RefreshTokens} {
		for key, token := range tokens {
			if !now.Before(token.ExpiresAt) {
				delete(tokens, key)
			}
		}
	}
	data, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.json")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), s.path)
}

func (s *FileStore) mutate(apply func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	apply()
	return s.saveLocked()
}

func (s *FileStore) SaveClient(_ context.Context, client auth.Client) error {
	client.RedirectURIs = slices.Clone(client.RedirectURIs)
	return s.mutate(func() { s.state.Clients[client.ID] = client })
}

func (s *FileStore) Client(_ context.Context, id string) (auth.Client, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	client, ok := s.state.Clients[id]
	client.RedirectURIs = slices.Clone(client.RedirectURIs)
	return client, ok, nil
}

func (s *FileStore) SaveCode(_ context.Context, codeHash string, code auth.Code) error {
	return s.mutate(func() { s.state.Codes[codeHash] = code })
}

func (s *FileStore) TakeCode(_ context.Context, codeHash string) (auth.Code, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	code, ok := s.state.Codes[codeHash]
	if !ok {
		return auth.Code{}, false, nil
	}
	delete(s.state.Codes, codeHash)
	return code, true, s.saveLocked()
}

func (s *FileStore) SaveToken(_ context.Context, tokenHash string, token auth.Token) error {
	return s.mutate(func() { s.state.Tokens[tokenHash] = token })
}

func (s *FileStore) Token(_ context.Context, tokenHash string) (auth.Token, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.state.Tokens[tokenHash]
	return token, ok, nil
}

func (s *FileStore) SaveRefreshToken(_ context.Context, tokenHash string, token auth.Token) error {
	return s.mutate(func() { s.state.RefreshTokens[tokenHash] = token })
}

func (s *FileStore) RefreshToken(_ context.Context, tokenHash string) (auth.Token, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.state.RefreshTokens[tokenHash]
	return token, ok, nil
}

// SaveGoogleToken stores the mailbox OAuth token. A refreshed token without a
// refresh_token keeps the previously stored refresh token.
func (s *FileStore) SaveGoogleToken(_ context.Context, token *oauth2.Token) error {
	return s.mutate(func() {
		copied := *token
		if copied.RefreshToken == "" && s.state.GoogleToken != nil {
			copied.RefreshToken = s.state.GoogleToken.RefreshToken
		}
		s.state.GoogleToken = &copied
	})
}

func (s *FileStore) GoogleToken(_ context.Context) (*oauth2.Token, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.GoogleToken == nil {
		return nil, false, nil
	}
	copied := *s.state.GoogleToken
	return &copied, true, nil
}

func (s *FileStore) ClearGoogleToken(_ context.Context) error {
	return s.mutate(func() { s.state.GoogleToken = nil })
}
