package fmqueue

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultQueue_SamePath(t *testing.T) {
	q := New()
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := q.WithLock("/tmp/test.txt", func() error {
				cur := inFlight.Add(1)
				if cur > maxInFlight.Load() {
					maxInFlight.Store(cur)
				}
				inFlight.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if maxInFlight.Load() != 1 {
		t.Errorf("expected max 1 concurrent access, got %d", maxInFlight.Load())
	}
}

func TestDefaultQueue_DifferentPaths(t *testing.T) {
	q := New()
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32

	const n = 10
	ready := make(chan struct{}) // barrier: closed when all goroutines are ready

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("/tmp/file_%d.txt", i)
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			// Signal readiness and wait for all others
			<-ready
			err := q.WithLock(p, func() error {
				cur := inFlight.Add(1)
				for {
					old := maxInFlight.Load()
					if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
						break
					}
				}
				// Hold the lock briefly so other goroutines can overlap
				time.Sleep(10 * time.Millisecond)
				inFlight.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}(path)
	}

	// Release all goroutines at once
	close(ready)
	wg.Wait()

	// Different paths should run concurrently, so max should be > 1
	if maxInFlight.Load() <= 1 {
		t.Errorf("expected concurrent access for different paths, got max %d", maxInFlight.Load())
	}
}

func TestSerialQueue_RecordsOrder(t *testing.T) {
	q := NewSerial()

	_ = q.WithLock("/tmp/a.txt", func() error { return nil })
	_ = q.WithLock("/tmp/b.txt", func() error { return nil })
	_ = q.WithLock("/tmp/a.txt", func() error { return nil })

	order := q.Order()
	if len(order) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(order))
	}
	if order[0] != "/tmp/a.txt" || order[1] != "/tmp/b.txt" || order[2] != "/tmp/a.txt" {
		t.Errorf("unexpected order: %v", order)
	}
}

func TestCountingQueue(t *testing.T) {
	inner := New()
	q := NewCounting(inner)

	_ = q.WithLock("/tmp/a.txt", func() error { return nil })
	_ = q.WithLock("/tmp/a.txt", func() error { return nil })
	_ = q.WithLock("/tmp/b.txt", func() error { return nil })

	if q.Count("/tmp/a.txt") != 2 {
		t.Errorf("expected 2 calls for a.txt, got %d", q.Count("/tmp/a.txt"))
	}
	if q.Count("/tmp/b.txt") != 1 {
		t.Errorf("expected 1 call for b.txt, got %d", q.Count("/tmp/b.txt"))
	}
	if q.Count("/tmp/c.txt") != 0 {
		t.Errorf("expected 0 calls for c.txt, got %d", q.Count("/tmp/c.txt"))
	}
}
