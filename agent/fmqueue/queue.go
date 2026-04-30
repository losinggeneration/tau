// Package fmqueue provides per-realpath serialization of file mutations
// so parallel tool calls from the model can't corrupt shared files.
package fmqueue // import "github.com/losinggeneration/tau/agent/fmqueue"

import (
	"path/filepath"
	"sync"
)

// MutationQueue serializes file mutations per realpath. Only write and edit
// tools go through the queue; read, grep, find, ls do not.
type MutationQueue interface {
	// WithLock acquires a lock for the given realpath and calls fn.
	// Calls for the same realpath are serialized; different realpaths
	// run concurrently.
	WithLock(realpath string, fn func() error) error
}

// defaultQueue is the per-realpath-mutex implementation.
type defaultQueue struct {
	mu sync.Map // map[string]*sync.Mutex
}

// New returns the default MutationQueue implementation using per-realpath
// mutexes cached in a sync.Map.
func New() MutationQueue {
	return &defaultQueue{}
}

func (q *defaultQueue) WithLock(realpath string, fn func() error) error {
	mu, _ := q.mu.LoadOrStore(realpath, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	return fn()
}

// Realpath resolves the canonical path for mutation queue locking.
// It follows symlinks and returns the resolved absolute path.
// For non-existent files or relative paths, it returns the resolved absolute path.
// This ensures two paths pointing to the same file (via symlinks or
// different relative/absolute forms) serialize against each other.
func Realpath(path string) (string, error) {
	absPath := path
	if !filepath.IsAbs(path) {
		var err error
		absPath, err = filepath.Abs(path)
		if err != nil {
			return path, err
		}
	}

	realpath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// File doesn't exist yet — use the resolved absolute path
		return absPath, nil
	}
	return realpath, nil
}
