package fmqueue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestRealpathPropagatesResolutionFailures(t *testing.T) {
	dir := t.TempDir()
	loop := filepath.Join(dir, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	_, resolveErr := filepath.EvalSymlinks(loop)
	if resolveErr == nil {
		t.Fatal("expected a symlink loop error")
	}
	_, err := Realpath(loop)
	if err == nil {
		t.Fatal("Realpath hid a symlink resolution failure")
	}
	if !strings.Contains(err.Error(), resolveErr.Error()) {
		t.Errorf("Realpath error = %v, want wrapped %v", err, resolveErr)
	}
	missing := filepath.Join(dir, "missing", "file")
	path, err := Realpath(missing)
	if err != nil || path != missing {
		t.Errorf("missing path = %q, %v", path, err)
	}
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path, err = Realpath(filepath.Join(file, "child"))
	if err != nil || path != filepath.Join(file, "child") {
		t.Errorf("non-directory path = %q, %v", path, err)
	}
}

func TestDefaultQueueReleasesSettledKeys(t *testing.T) {
	q := New().(*defaultQueue)
	failure := errors.New("mutation failed")
	if err := q.WithLock("failed", func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("mutation error = %v", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("mutation panic was swallowed")
			}
		}()
		if err := q.WithLock("panic", func() error { panic("mutation panic") }); err != nil {
			t.Error(err)
		}
	}()
	for i := range 100 {
		if err := q.WithLock(fmt.Sprint(i), func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	q.mu.Lock()
	count := len(q.locks)
	q.mu.Unlock()
	if count != 0 {
		t.Errorf("queue retains %d settled keys", count)
	}
}

func TestDefaultQueueKeepsOneLockUntilAllWaitersSettle(t *testing.T) {
	q := New().(*defaultQueue)
	releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
	firstDone, secondEntered, secondDone, thirdEntered := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	defer wg.Wait()
	defer close(releaseSecond)
	defer close(releaseFirst)
	waitUsers := func(want int) *mutationLock {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for {
			q.mu.Lock()
			lock := q.locks["file"]
			var users int
			if lock != nil {
				users = lock.users
			}
			q.mu.Unlock()
			if users == want {
				return lock
			}
			if time.Now().After(deadline) {
				t.Fatalf("registered users = %d, want %d", users, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	wg.Go(func() {
		defer close(firstDone)
		if err := q.WithLock("file", func() error { <-releaseFirst; return nil }); err != nil {
			t.Error(err)
		}
	})
	waitUsers(1)
	wg.Go(func() {
		defer close(secondDone)
		if err := q.WithLock("file", func() error { close(secondEntered); <-releaseSecond; return nil }); err != nil {
			t.Error(err)
		}
	})
	lock := waitUsers(2)
	releaseFirst <- struct{}{}
	<-firstDone
	<-secondEntered
	wg.Go(func() {
		if err := q.WithLock("file", func() error { close(thirdEntered); return nil }); err != nil {
			t.Error(err)
		}
	})
	if current := waitUsers(2); current != lock {
		t.Error("waiter registration replaced the active lock")
	}
	select {
	case <-thirdEntered:
		t.Error("third mutation entered while second held the lock")
	default:
	}
	releaseSecond <- struct{}{}
	<-secondDone
	<-thirdEntered
	wg.Wait()
	q.mu.Lock()
	remaining := len(q.locks)
	q.mu.Unlock()
	if remaining != 0 {
		t.Errorf("retained %d settled keys", remaining)
	}
}
