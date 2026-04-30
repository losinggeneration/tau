// Package state provides per-plugin persistent state storage.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	mu    sync.RWMutex
	dir   string
	kv    map[string]string
	log   []LogEntry
	logMu sync.Mutex
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

// NewStore creates or opens a state store for a plugin.
func NewStore(baseDir, pluginName string) (*Store, error) {
	dir := filepath.Join(baseDir, pluginName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create plugin state dir: %w", err)
	}

	s := &Store{
		dir: dir,
		kv:  make(map[string]string),
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

	oldVal := s.kv[key]
	s.kv[key] = value

	if err := s.save(); err != nil {
		s.kv[key] = oldVal
		return fmt.Errorf("save state: %w", err)
	}

	return nil
}

// Delete removes a value from the KV store.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.kv[key]; !ok {
		return nil
	}

	delete(s.kv, key)

	if err := s.save(); err != nil {
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

	s.kv = make(map[string]string)
	s.log = nil

	if err := s.save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if err := s.saveLog(); err != nil {
		return fmt.Errorf("save log: %w", err)
	}

	return nil
}

// Remove removes the entire state directory for this plugin.
func (s *Store) Remove() error {
	return os.RemoveAll(s.dir)
}

// RemovePlugin removes the state directory for a named plugin.
// It is safe to call even if the directory does not exist.
// The state directory path is: baseDir/plugin-state/pluginName.
func RemovePlugin(baseDir, pluginName string) error {
	path := filepath.Join(baseDir, "plugin-state", pluginName)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove plugin state dir %q: %w", path, err)
	}
	return nil
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
	// Load KV
	kvFile, err := os.Open(s.kvPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := json.NewDecoder(kvFile).Decode(&s.kv); err != nil {
		if closeErr := kvFile.Close(); closeErr != nil {
			return fmt.Errorf("close kv file after decode error: %w", closeErr)
		}
		return err
	}
	if err := kvFile.Close(); err != nil {
		return fmt.Errorf("close kv file: %w", err)
	}

	// Load log
	logFile, err := os.Open(s.logPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer func() {
		if closeErr := logFile.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close log file: %w", closeErr)
		}
	}()

	// Log is JSONL format
	s.log = nil
	dec := json.NewDecoder(logFile)
	for dec.More() {
		var entry LogEntry
		if err := dec.Decode(&entry); err != nil {
			return err
		}
		s.log = append(s.log, entry)
	}

	return nil
}

// save writes the KV to disk.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.kv, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.kvPath(), data, 0o600)
}

// saveLog writes the log to disk in JSONL format.
func (s *Store) saveLog() (err error) {
	f, err := os.OpenFile(s.logPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close log file: %w", closeErr)
		}
	}()

	for _, entry := range s.log {
		data, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return err
		}
	}

	return nil
}
