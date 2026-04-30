package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// UsageEntry records a single LLM call for the cumulative usage log.
type UsageEntry struct {
	Timestamp    string  `json:"timestamp"`
	Session      string  `json:"session"`
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// UsageLogger writes append-only usage entries to ~/.tau/agent/usage.jsonl.
type UsageLogger struct {
	mu   sync.Mutex
	file *os.File
}

var (
	usageLogger     *UsageLogger
	usageLoggerOnce sync.Once
)

// GetUsageLogger returns the singleton UsageLogger.
func GetUsageLogger() (*UsageLogger, error) {
	var initErr error
	usageLoggerOnce.Do(func() {
		home, err := os.UserHomeDir()
		if err != nil {
			initErr = fmt.Errorf("get home dir: %w", err)
			return
		}
		dir := filepath.Join(home, ".tau", "agent")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			initErr = fmt.Errorf("create agent dir: %w", err)
			return
		}
		path := filepath.Join(dir, "usage.jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			initErr = fmt.Errorf("open usage log: %w", err)
			return
		}
		usageLogger = &UsageLogger{file: f}
	})
	return usageLogger, initErr
}

// Log records a usage entry.
func (l *UsageLogger) Log(entry UsageEntry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal usage entry: %w", err)
	}
	data = append(data, '\n')

	if _, err := l.file.Write(data); err != nil {
		return fmt.Errorf("write usage entry: %w", err)
	}
	return l.file.Sync()
}

// Close closes the usage log file.
func (l *UsageLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}
