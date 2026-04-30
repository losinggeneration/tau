package fmqueue

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRealpath_Symlink verifies that two paths pointing to the same
// file via a symlink resolve to the same realpath.
func TestRealpath_Symlink(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a real file
	realFile := filepath.Join(tmpDir, "real.txt")
	err := os.WriteFile(realFile, []byte("content"), 0o644)
	if err != nil {
		t.Fatalf("failed to create real file: %v", err)
	}

	// Create a symlink to it
	symlinkFile := filepath.Join(tmpDir, "link.txt")
	err = os.Symlink(realFile, symlinkFile)
	if err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// Both paths should resolve to the same realpath
	realReal, err := Realpath(realFile)
	if err != nil {
		t.Fatalf("Realpath(real) failed: %v", err)
	}
	symReal, err := Realpath(symlinkFile)
	if err != nil {
		t.Fatalf("Realpath(symlink) failed: %v", err)
	}

	if realReal != symReal {
		t.Errorf("expected same realpath for symlink pair, got %q vs %q", realReal, symReal)
	}
}

// TestRealpath_NewFile verifies that a path to a non-existent file
// resolves to its absolute path (not an error).
func TestRealpath_NewFile(t *testing.T) {
	tmpDir := t.TempDir()

	newFile := filepath.Join(tmpDir, "newfile.txt")
	realpath, err := Realpath(newFile)
	if err != nil {
		t.Fatalf("Realpath(new file) failed: %v", err)
	}

	abs, err := filepath.Abs(newFile)
	if err != nil {
		t.Fatalf("filepath.Abs failed: %v", err)
	}

	if realpath != abs {
		t.Errorf("expected absolute path for non-existent file, got %q, want %q", realpath, abs)
	}
}

// TestRealpath_ChainedSymlinks verifies that chained symlinks resolve
// to the ultimate real file.
func TestRealpath_ChainedSymlinks(t *testing.T) {
	tmpDir := t.TempDir()

	// Create: real -> link1 -> link2
	realFile := filepath.Join(tmpDir, "real.txt")
	err := os.WriteFile(realFile, []byte("content"), 0o644)
	if err != nil {
		t.Fatalf("failed to create real file: %v", err)
	}

	link1 := filepath.Join(tmpDir, "link1.txt")
	err = os.Symlink(realFile, link1)
	if err != nil {
		t.Fatalf("failed to create link1: %v", err)
	}

	link2 := filepath.Join(tmpDir, "link2.txt")
	err = os.Symlink(link1, link2)
	if err != nil {
		t.Fatalf("failed to create link2: %v", err)
	}

	// All three should resolve to the same realpath
	realPath, _ := Realpath(realFile)
	link1Path, _ := Realpath(link1)
	link2Path, _ := Realpath(link2)

	if realPath != link1Path || link1Path != link2Path {
		t.Errorf("expected all paths to match, got real=%q link1=%q link2=%q", realPath, link1Path, link2Path)
	}
}

// TestDefaultQueue_SymlinkSerialization verifies that concurrent writes
// to the same file via a symlink are serialized.
func TestDefaultQueue_SymlinkSerialization(t *testing.T) {
	tmpDir := t.TempDir()

	realFile := filepath.Join(tmpDir, "real.txt")
	err := os.WriteFile(realFile, []byte("initial"), 0o644)
	if err != nil {
		t.Fatalf("failed to create real file: %v", err)
	}

	symlinkFile := filepath.Join(tmpDir, "link.txt")
	err = os.Symlink(realFile, symlinkFile)
	if err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	q := New()

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	var wg sync.WaitGroup

	// Launch concurrent operations: some use real path, some use symlink.
	// They should serialize because both resolve to the same realpath.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			path := realFile
			if idx%2 == 0 {
				path = symlinkFile
			}

			// Use the Realpath helper to get the canonical path for locking
			canonPath, err := Realpath(path)
			if err != nil {
				t.Errorf("Realpath failed: %v", err)
				return
			}

			err = q.WithLock(canonPath, func() error {
				cur := inFlight.Add(1)
				for {
					old := maxInFlight.Load()
					if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
						break
					}
				}
				inFlight.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("WithLock failed: %v", err)
			}
		}(i)
	}

	wg.Wait()

	// With proper realpath-based locking, max concurrent should be 1
	if maxInFlight.Load() != 1 {
		t.Errorf("expected serial execution (maxInFlight=1), got %d", maxInFlight.Load())
	}
}

// TestDefaultQueue_DifferentRealpathsConcurrent verifies that operations
// on different realpaths can run concurrently.
func TestDefaultQueue_DifferentRealpathsConcurrent(t *testing.T) {
	tmpDir := t.TempDir()

	file1 := filepath.Join(tmpDir, "file1.txt")
	file2 := filepath.Join(tmpDir, "file2.txt")
	err := os.WriteFile(file1, []byte("content1"), 0o644)
	if err != nil {
		t.Fatalf("failed to create file1: %v", err)
	}
	err = os.WriteFile(file2, []byte("content2"), 0o644)
	if err != nil {
		t.Fatalf("failed to create file2: %v", err)
	}

	// Pre-resolve canonical paths outside goroutines to avoid race
	canon1, _ := Realpath(file1)
	canon2, _ := Realpath(file2)

	q := New()

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	ready := make(chan struct{})

	var wg sync.WaitGroup
	// Launch 10 operations: 5 on file1, 5 on file2
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			<-ready

			// Alternate: first 5 use file1, second 5 use file2
			canonPath := canon1
			if idx >= 5 {
				canonPath = canon2
			}

			err := q.WithLock(canonPath, func() error {
				cur := inFlight.Add(1)
				for {
					old := maxInFlight.Load()
					if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
						break
					}
				}
				// Hold lock briefly to allow concurrency between different files
				time.Sleep(5 * time.Millisecond)
				inFlight.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("WithLock failed: %v", err)
			}
		}(i)
	}

	close(ready)
	wg.Wait()

	// Different files should be able to run concurrently
	if maxInFlight.Load() <= 1 {
		t.Errorf("expected concurrent access for different files, got max %d", maxInFlight.Load())
	}
}

// TestWriteTool_SymlinkViaQueue tests that write tool's realpath resolution
// is consistent with what the mutation queue expects.
// This is a unit test of the realpath resolution logic without the full tool.
func TestWriteTool_SymlinkViaQueue(t *testing.T) {
	tmpDir := t.TempDir()

	realFile := filepath.Join(tmpDir, "real.txt")
	err := os.WriteFile(realFile, []byte("original"), 0o644)
	if err != nil {
		t.Fatalf("failed to create real file: %v", err)
	}

	symlinkFile := filepath.Join(tmpDir, "link.txt")
	err = os.Symlink(realFile, symlinkFile)
	if err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	q := New()

	// Simulate what WriteTool does: resolve realpath, then lock
	realpath, _ := Realpath(symlinkFile)

	var order []string
	err = q.WithLock(realpath, func() error {
		// First write via symlink
		err := os.WriteFile(symlinkFile, []byte("via symlink"), 0o644)
		if err != nil {
			return err
		}
		order = append(order, "symlink")
		return nil
	})
	if err != nil {
		t.Fatalf("first WithLock failed: %v", err)
	}

	// Second write via real path should serialize
	err = q.WithLock(realpath, func() error {
		// Read what was written via symlink
		data, err := os.ReadFile(realFile)
		if err != nil {
			return err
		}
		if string(data) != "via symlink" {
			t.Errorf("expected 'via symlink', got %q", string(data))
		}
		order = append(order, "real")
		return nil
	})
	if err != nil {
		t.Fatalf("second WithLock failed: %v", err)
	}

	// Verify both operations completed in order
	if len(order) != 2 || order[0] != "symlink" || order[1] != "real" {
		t.Errorf("unexpected order: %v", order)
	}
}

// TestRealpath_RelativePath tests realpath resolution for relative paths.
func TestRealpath_RelativePath(t *testing.T) {
	tmpDir := t.TempDir()
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	defer os.Chdir(origCwd)

	err = os.Chdir(tmpDir)
	if err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}

	// Create file
	realFile := "./subdir/file.txt"
	err = os.MkdirAll(filepath.Dir(realFile), 0o755)
	if err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}
	err = os.WriteFile(realFile, []byte("content"), 0o644)
	if err != nil {
		t.Fatalf("failed to create file: %v", err)
	}

	// Resolve relative path
	canon, err := Realpath(realFile)
	if err != nil {
		t.Fatalf("Realpath failed: %v", err)
	}

	abs, err := filepath.Abs(realFile)
	if err != nil {
		t.Fatalf("filepath.Abs failed: %v", err)
	}

	if canon != abs {
		t.Errorf("Realpath(relative) = %q, want %q", canon, abs)
	}
}
