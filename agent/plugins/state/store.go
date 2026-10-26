// Package state provides per-plugin persistent state storage.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// KVEntry represents a key-value entry for listing.
type KVEntry struct {
	Key   string
	Value string
}

// LogEntry represents a single append-log entry.
type LogEntry struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	Timestamp string `json:"timestamp"`
}

// Store manages state for a single plugin.
type Store struct {
	mu         sync.RWMutex
	dir        string
	baseDir    string
	pluginName string
	kv         map[string]string
	log        []LogEntry
	logMu      sync.Mutex
}

// KVEntries returns all KV entries as a slice.
func (s *Store) KVEntries() ([]KVEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries := make([]KVEntry, 0, len(s.kv))
	for k, v := range s.kv {
		entries = append(entries, KVEntry{Key: k, Value: v})
	}
	return entries, nil
}

// Log returns all log entries.
func (s *Store) Log() []LogEntry {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	result := make([]LogEntry, len(s.log))
	copy(result, s.log)
	return result
}

// LogEntries returns all log entries (alias for Log, for compatibility).
func (s *Store) LogEntries() ([]LogEntry, error) {
	return s.Log(), nil
}

func validateStateIdentity(baseDir, name string) error {
	if !filepath.IsLocal(name) || strings.Contains(name, `\`) {
		return fmt.Errorf("invalid plugin state identity %q", name)
	}
	parts := strings.Split(filepath.ToSlash(name), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid plugin state identity %q", name)
		}
	}
	if len(parts) > 2 || len(parts) == 2 && !strings.HasPrefix(parts[0], "@") {
		return fmt.Errorf("invalid plugin state identity %q", name)
	}
	for i := range parts {
		info, err := os.Lstat(filepath.Join(append([]string{baseDir}, parts[:i+1]...)...))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stat plugin state path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("plugin state path must be a real directory")
		}
	}
	return nil
}

// NewStore creates or opens a state store for a plugin.
func NewStore(baseDir, pluginName string) (*Store, error) {
	if err := validateStateIdentity(baseDir, pluginName); err != nil {
		return nil, err
	}
	dir := filepath.Join(baseDir, pluginName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create plugin state dir: %w", err)
	}

	s := &Store{
		dir:        dir,
		baseDir:    baseDir,
		pluginName: pluginName,
		kv:         make(map[string]string),
	}

	if err := s.load(); err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	return s, nil
}

// Get retrieves a value from the KV store.
func (s *Store) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.kv[key]
	return v, ok
}

// Set stores a value in the KV store.
func (s *Store) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldVal, existed := s.kv[key]
	s.kv[key] = value

	if err := s.save(); err != nil {
		if existed {
			s.kv[key] = oldVal
		} else {
			delete(s.kv, key)
		}
		return fmt.Errorf("save state: %w", err)
	}

	return nil
}

// Delete removes a value from the KV store.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldVal, ok := s.kv[key]
	if !ok {
		return nil
	}

	delete(s.kv, key)

	if err := s.save(); err != nil {
		s.kv[key] = oldVal
		return fmt.Errorf("save state: %w", err)
	}

	return nil
}

// Append adds a log entry.
func (s *Store) Append(key, value string) error {
	s.logMu.Lock()
	defer s.logMu.Unlock()

	entry := LogEntry{
		ID:        fmt.Sprintf("%d", len(s.log)+1),
		Key:       key,
		Value:     value,
		Timestamp: "",
	}
	s.log = append(s.log, entry)

	if err := s.saveLog(); err != nil {
		s.log = s.log[:len(s.log)-1]
		return fmt.Errorf("save log: %w", err)
	}

	return nil
}

// Clear removes all state and log entries.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logMu.Lock()
	defer s.logMu.Unlock()

	oldKV, oldLog := s.kv, s.log
	s.kv = make(map[string]string)
	if err := s.save(); err != nil {
		s.kv = oldKV
		return fmt.Errorf("save state: %w", err)
	}
	s.log = nil
	if err := s.saveLog(); err != nil {
		s.log = oldLog
		return fmt.Errorf("save log: %w", err)
	}

	return nil
}

// Remove removes the entire state directory for this plugin.
func (s *Store) Remove() error {
	if err := validateStateIdentity(s.baseDir, s.pluginName); err != nil {
		return err
	}
	return removeStateDirectory(s.baseDir, s.pluginName)
}

// RemovePlugin removes the state directory for a named plugin.
// It is safe to call even if the directory does not exist.
// The state directory path is: baseDir/plugin-state/pluginName.
func RemovePlugin(baseDir, pluginName string) error {
	if err := validateStateIdentity(filepath.Join(baseDir, "plugin-state"), pluginName); err != nil {
		return err
	}
	return removeStateDirectory(filepath.Join(baseDir, "plugin-state"), pluginName)
}

func removeStateDirectory(baseDir, pluginName string) (err error) {
	root, err := os.OpenRoot(baseDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return root.RemoveAll(pluginName)
}

func (s *Store) openRoot() (root *os.Root, err error) {
	if err := validateStateIdentity(s.baseDir, s.pluginName); err != nil {
		return nil, err
	}
	base, err := os.OpenRoot(s.baseDir)
	if err != nil {
		return nil, err
	}
	root, err = base.OpenRoot(s.pluginName)
	closeErr := base.Close()
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, root.Close())
	}
	return root, nil
}

// kvPath returns the path to the KV file.
func (s *Store) kvPath() string {
	return filepath.Join(s.dir, "kv.json")
}

// logPath returns the path to the log file.
func (s *Store) logPath() string {
	return filepath.Join(s.dir, "log.jsonl")
}

// load reads the state from disk.
func (s *Store) load() (err error) {
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	data, err := root.ReadFile("kv.json")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(data, &s.kv); err != nil {
			return err
		}
		if s.kv == nil {
			return fmt.Errorf("plugin KV state must be an object")
		}
	}
	data, err = root.ReadFile("log.jsonl")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var entry LogEntry
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		s.log = append(s.log, entry)
	}
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.kv, "", "  ")
	if err != nil {
		return err
	}
	return s.writeFile("kv.json", data)
}

func (s *Store) saveLog() error {
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	for _, entry := range s.log {
		if err := encoder.Encode(entry); err != nil {
			return err
		}
	}
	return s.writeFile("log.jsonl", data.Bytes())
}

func (s *Store) writeFile(name string, data []byte) (err error) {
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	temporary := ".tau-state-" + uuid.NewString()
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, file.Close())
		}
		if removeErr := root.Remove(temporary); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, removeErr)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	closed = true
	if err := file.Close(); err != nil {
		return err
	}
	return root.Rename(temporary, name)
}
