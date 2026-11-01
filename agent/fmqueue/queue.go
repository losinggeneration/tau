// Package fmqueue provides per-realpath serialization of file mutations
// so parallel tool calls from the model can't corrupt shared files.
package fmqueue // import "github.com/losinggeneration/tau/agent/fmqueue"

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"syscall"
)

// MutationQueue serializes file mutations per realpath. Only write and edit
// tools go through the queue; read, grep, find, ls do not.
type MutationQueue interface {
	// WithLock acquires a lock for the given realpath and calls fn.
	// Calls for the same realpath are serialized; different realpaths
	// run concurrently.
	WithLock(realpath string, fn func() error) error
}

type mutationLock struct {
	mu    sync.Mutex
	users int
}

// defaultQueue registers holders and waiters before acquiring their file lock.
type defaultQueue struct {
	mu    sync.Mutex
	locks map[string]*mutationLock
}

// New returns a per-realpath mutation queue.
func New() MutationQueue {
	return &defaultQueue{locks: make(map[string]*mutationLock)}
}

func (q *defaultQueue) WithLock(realpath string, fn func() error) error {
	q.mu.Lock()
	lock := q.locks[realpath]
	if lock == nil {
		lock = &mutationLock{}
		q.locks[realpath] = lock
	}
	lock.users++
	q.mu.Unlock()

	lock.mu.Lock()
	defer func() {
		lock.mu.Unlock()
		q.mu.Lock()
		defer q.mu.Unlock()
		lock.users--
		if lock.users == 0 {
			delete(q.locks, realpath)
		}
	}()
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
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return absPath, nil
		}
		return "", fmt.Errorf("resolve realpath %s: %w", absPath, err)
	}
	return realpath, nil
}
