package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RemoteToken is a named bearer token issued to one remote device.
type RemoteToken struct {
	Name    string    `json:"name"`
	Token   string    `json:"token"`
	Created time.Time `json:"created"`
}

// TokenStore persists per-device bearer tokens as JSON. The bridge consults
// it on every auth check, so revocation takes effect without a restart.
type TokenStore struct {
	path string
	mu   sync.Mutex
}

// NewTokenStore creates a token store backed by the given JSON file.
func NewTokenStore(path string) *TokenStore {
	return &TokenStore{path: path}
}

// Add generates a new token for name, replacing any existing entry with that
// name. The full token is only returned here; list output must keep showing
// prefixes only.
func (ts *TokenStore) Add(name string) (RemoteToken, error) {
	if name == "" {
		return RemoteToken{}, fmt.Errorf("token name cannot be empty")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return RemoteToken{}, fmt.Errorf("generate token: %w", err)
	}
	tok := RemoteToken{Name: name, Token: hex.EncodeToString(buf), Created: time.Now().UTC()}

	ts.mu.Lock()
	defer ts.mu.Unlock()
	tokens, err := ts.load()
	if err != nil {
		return RemoteToken{}, err
	}
	replaced := false
	for i := range tokens {
		if tokens[i].Name == name {
			tokens[i] = tok
			replaced = true
			break
		}
	}
	if !replaced {
		tokens = append(tokens, tok)
	}
	if err := ts.save(tokens); err != nil {
		return RemoteToken{}, err
	}
	return tok, nil
}

// List returns all stored device tokens.
func (ts *TokenStore) List() ([]RemoteToken, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.load()
}

// Revoke removes the named token and reports whether it existed.
func (ts *TokenStore) Revoke(name string) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("token name cannot be empty")
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	tokens, err := ts.load()
	if err != nil {
		return false, err
	}
	kept := make([]RemoteToken, 0, len(tokens))
	found := false
	for _, t := range tokens {
		if t.Name == name {
			found = true
			continue
		}
		kept = append(kept, t)
	}
	if !found {
		return false, nil
	}
	if err := ts.save(kept); err != nil {
		return false, err
	}
	return true, nil
}

// Contains reports whether token matches a stored device token.
func (ts *TokenStore) Contains(token string) bool {
	if token == "" {
		return false
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	tokens, err := ts.load()
	if err != nil {
		return false
	}
	for _, t := range tokens {
		if t.Token == token {
			return true
		}
	}
	return false
}

func (ts *TokenStore) load() ([]RemoteToken, error) {
	data, err := os.ReadFile(ts.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read token store: %w", err)
	}
	var tokens []RemoteToken
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, fmt.Errorf("parse token store: %w", err)
	}
	return tokens, nil
}

func (ts *TokenStore) save(tokens []RemoteToken) error {
	if err := os.MkdirAll(filepath.Dir(ts.path), 0o700); err != nil {
		return fmt.Errorf("create token store dir: %w", err)
	}
	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal token store: %w", err)
	}
	if err := os.WriteFile(ts.path, data, 0o600); err != nil {
		return fmt.Errorf("write token store: %w", err)
	}
	return nil
}
