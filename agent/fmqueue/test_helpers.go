package fmqueue

import (
	"sync"
	"sync/atomic"
)

// SerialQueue is a test implementation that records call order. All calls
// are serialized through a single mutex so the recorded order is deterministic.
type SerialQueue struct {
	mu    sync.Mutex
	order []string
}

// NewSerial returns a SerialQueue for testing.
func NewSerial() *SerialQueue {
	return &SerialQueue{}
}

func (q *SerialQueue) WithLock(realpath string, fn func() error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.order = append(q.order, realpath)
	return fn()
}

// Order returns the recorded realpath call order.
func (q *SerialQueue) Order() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, len(q.order))
	copy(out, q.order)
	return out
}

// Reset clears the recorded order.
func (q *SerialQueue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.order = q.order[:0]
}

// concurrentQueue tests that per-realpath locking works correctly under
// concurrent access. It tracks how many goroutines are in-flight for each
// realpath simultaneously.
type concurrentQueue struct {
	mu       sync.Map
	maxDepth map[string]int64
}

// NewConcurrent returns a queue that tracks max concurrency per realpath.
func NewConcurrent() *concurrentQueue {
	return &concurrentQueue{
		maxDepth: make(map[string]int64),
	}
}

func (q *concurrentQueue) WithLock(realpath string, fn func() error) error {
	mu, _ := q.mu.LoadOrStore(realpath, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	return fn()
}

// MaxDepth returns the maximum concurrent in-flight calls for a realpath.
// Not used in the simple implementation since per-realpath mutex serializes.
func (q *concurrentQueue) MaxDepth(realpath string) int64 {
	return q.maxDepth[realpath]
}

// CountingQueue wraps a MutationQueue and counts how many times WithLock
// was called for each realpath.
type CountingQueue struct {
	inner  MutationQueue
	counts sync.Map // map[string]*atomic.Int64
}

// NewCounting wraps inner and counts WithLock calls per realpath.
func NewCounting(inner MutationQueue) *CountingQueue {
	return &CountingQueue{inner: inner}
}

func (q *CountingQueue) WithLock(realpath string, fn func() error) error {
	counter, _ := q.counts.LoadOrStore(realpath, &atomic.Int64{})
	counter.(*atomic.Int64).Add(1)
	return q.inner.WithLock(realpath, fn)
}

// Count returns how many times WithLock was called for the given realpath.
func (q *CountingQueue) Count(realpath string) int64 {
	v, ok := q.counts.Load(realpath)
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}
