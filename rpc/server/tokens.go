package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	path     string
	mu       sync.Mutex
	snapshot os.FileInfo
	accepted map[string]struct{}
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
	info, err := os.Stat(ts.path)
	if err != nil {
		ts.snapshot, ts.accepted = nil, nil
		return false
	}
	// A CLI update publishes a new inode. Keep the metadata check on every
	// frame but only read and parse the store when its snapshot changes.
	if ts.snapshot == nil || !os.SameFile(ts.snapshot, info) || info.Size() != ts.snapshot.Size() || !info.ModTime().Equal(ts.snapshot.ModTime()) {
		tokens, snapshot, err := ts.readSnapshot()
		if err != nil {
			ts.snapshot, ts.accepted = nil, nil
			return false
		}
		ts.accepted = make(map[string]struct{}, len(tokens))
		for _, t := range tokens {
			ts.accepted[t.Token] = struct{}{}
		}
		ts.snapshot = snapshot
	}
	_, found := ts.accepted[token]
	return found
}

func (ts *TokenStore) load() ([]RemoteToken, error) {
	tokens, _, err := ts.readSnapshot()
	return tokens, err
}

func (ts *TokenStore) readSnapshot() (_ []RemoteToken, _ os.FileInfo, err error) {
	file, err := os.Open(ts.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open token store: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close token store: %w", closeErr))
		}
	}()
	// The path may have been replaced since the caller's stat. Cache the
	// metadata for the open file, not the path we used to find it.
	info, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat token snapshot: %w", err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, fmt.Errorf("read token store: %w", err)
	}
	var tokens []RemoteToken
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, nil, fmt.Errorf("parse token store: %w", err)
	}
	return tokens, info, nil
}

func (ts *TokenStore) save(tokens []RemoteToken) (err error) {
	if err := os.MkdirAll(filepath.Dir(ts.path), 0o700); err != nil {
		return fmt.Errorf("create token store dir: %w", err)
	}
	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal token store: %w", err)
	}
	// Readers in another process must never see a partially written store.
	file, err := os.CreateTemp(filepath.Dir(ts.path), ".remote-tokens-*")
	if err != nil {
		return fmt.Errorf("create token snapshot: %w", err)
	}
	defer func() {
		if err != nil {
			if removeErr := os.Remove(file.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove token snapshot: %w", removeErr))
			}
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write token snapshot: %w", errors.Join(err, file.Close()))
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync token snapshot: %w", errors.Join(err, file.Close()))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close token snapshot: %w", err)
	}
	if err := os.Rename(file.Name(), ts.path); err != nil {
		return fmt.Errorf("publish token snapshot: %w", err)
	}
	return nil
}
