package server

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSocketPathUsesXDGRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	want := filepath.Join(dir, "tau", socketName)
	if got := SocketPath(); got != want {
		t.Fatalf("SocketPath() = %q, want %q", got, want)
	}
}

func TestSocketPathFallbackIsUserScoped(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")

	got := SocketPath()
	if !strings.Contains(got, "tau-") || !strings.HasSuffix(got, socketName) {
		t.Fatalf("SocketPath() fallback = %q, want user-scoped tau socket", got)
	}
}
