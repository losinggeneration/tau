//go:build unix

package coord

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// socketDir returns a private directory short enough for Unix socket paths.
func socketDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("secure temp socket directory: %v", err)
	}
	if len(dir) <= 80 {
		return dir
	}
	dir, err := os.MkdirTemp("/tmp", "coord")
	if err != nil {
		t.Fatalf("create short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func mustListen(t *testing.T, path string) *endpoint {
	t.Helper()
	ep, err := listen(path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ep
}

func assertDialable(t *testing.T, path string) {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	_ = conn.Close()
}

func TestListenArbitratesSingleOwner(t *testing.T) {
	path := filepath.Join(socketDir(t), "c.sock")
	first := mustListen(t, path)

	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket mode = %v, %v; want owner-only", info, err)
	}
	if _, err := listen(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second listen error = %v, want ErrAlreadyRunning", err)
	}
	assertDialable(t, path)

	if err := first.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket after owner close: %v", err)
	}
	second := mustListen(t, path)
	if err := second.close(); err != nil {
		t.Fatalf("close second: %v", err)
	}
}

func TestListenLeavesUnlockedLiveListener(t *testing.T) {
	path := filepath.Join(socketDir(t), "c.sock")
	unlocked, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("unlocked listen: %v", err)
	}
	defer unlocked.Close()
	go func() {
		for {
			conn, err := unlocked.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if _, err := listen(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("listen over live socket error = %v, want ErrAlreadyRunning", err)
	}
	assertDialable(t, path)
}

func TestListenReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(socketDir(t), "c.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("stale listen: %v", err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatalf("close stale: %v", err)
	}

	ep := mustListen(t, path)
	assertDialable(t, path)
	if err := ep.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestListenRefusesUnsafePaths(t *testing.T) {
	t.Run("relative", func(t *testing.T) {
		if _, err := listen("c.sock"); err == nil {
			t.Fatal("relative path accepted")
		}
	})
	t.Run("regular file", func(t *testing.T) {
		path := filepath.Join(socketDir(t), "c.sock")
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := listen(path); err == nil {
			t.Fatal("regular file replaced")
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "keep" {
			t.Fatalf("regular file changed: %q, %v", data, err)
		}
	})
	t.Run("group readable dir", func(t *testing.T) {
		dir := filepath.Join(socketDir(t), "open")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := listen(filepath.Join(dir, "c.sock")); err == nil {
			t.Fatal("insecure dir accepted")
		}
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o750 {
			t.Fatalf("dir mode changed: %v, %v", info, err)
		}
	})
	t.Run("symlink dir", func(t *testing.T) {
		base := socketDir(t)
		target := filepath.Join(base, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := listen(filepath.Join(link, "c.sock")); err == nil {
			t.Fatal("symlinked dir accepted")
		}
	})
	t.Run("symlink lock", func(t *testing.T) {
		dir := socketDir(t)
		path := filepath.Join(dir, "c.sock")
		if err := os.Symlink(filepath.Join(dir, "elsewhere"), path+".lock"); err != nil {
			t.Fatal(err)
		}
		if _, err := listen(path); err == nil {
			t.Fatal("symlinked lock accepted")
		}
		if _, err := os.Lstat(filepath.Join(dir, "elsewhere")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink target created: %v", err)
		}
	})
}
