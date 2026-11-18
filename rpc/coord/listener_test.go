package coord

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListenUnixCloseDoesNotRemoveSuccessor(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tau.sock")
	first, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("closed predecessor removed successor socket: %v", err)
	}
}
