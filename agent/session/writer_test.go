package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerFileSameProcessRefCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	if err := ClaimOwner(path); err != nil {
		t.Fatalf("claim owner first: %v", err)
	}
	if err := ClaimOwner(path); err != nil {
		t.Fatalf("claim owner second: %v", err)
	}

	if err := ReleaseOwner(path); err != nil {
		t.Fatalf("release owner first: %v", err)
	}
	if _, err := os.Stat(path + ".owner"); err != nil {
		t.Fatalf("owner file removed before last same-process writer closed: %v", err)
	}

	if err := ReleaseOwner(path); err != nil {
		t.Fatalf("release owner second: %v", err)
	}
	if _, err := os.Stat(path + ".owner"); !os.IsNotExist(err) {
		t.Fatalf("owner file after final release err = %v, want not exist", err)
	}
}
