package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestRunUnixLifecyclePermissionsAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tau.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunUnix(ctx, path, &mockRuntime{}, newTestRegistry(), nil)
	}()

	conn := dialUnix(t, path)
	defer func() { _ = conn.Close() }()

	if info, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("stat socket dir: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode = %o, want 700", info.Mode().Perm())
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat socket: %v", err)
	} else if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket mode = %o, want owner-only", info.Mode().Perm())
	}

	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if !strings.Contains(line, `"type":"meta"`) {
		t.Fatalf("first line = %q, want meta", line)
	}
	active, total := UnixStats()
	if active < 1 || total < 1 {
		t.Fatalf("UnixStats() = active %d total %d, want active and total connection", active, total)
	}

	cmd := Command{ID: "state-1", Type: "get_state", Args: json.RawMessage(`{}`)}
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write command: %v", err)
	}
	line, err = r.ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp := parseResp(t, line)
	if resp["id"] != "state-1" || resp["ok"] != true {
		t.Fatalf("response = %#v", resp)
	}

	cancel()
	waitOrFail(t, waitErr(done, t), timeout, "daemon shutdown")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket exists after shutdown: %v", err)
	}
}

func TestRunUnixDoesNotCreateIdleSessionFile(t *testing.T) {
	sessions := t.TempDir()
	t.Setenv("TAU_SESSION_DIR", sessions)
	path := filepath.Join(t.TempDir(), "tau.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunUnix(ctx, path, &mockRuntime{}, newTestRegistry(), nil)
	}()

	conn := dialUnix(t, path)
	if closeErr := conn.Close(); closeErr != nil {
		t.Fatalf("close connection: %v", closeErr)
	}
	cancel()
	waitOrFail(t, waitErr(done, t), timeout, "daemon shutdown")

	entries, err := os.ReadDir(sessions)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("idle daemon created session files: %v", entries)
	}
}

func dialUnix(t *testing.T, path string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.Dial("unix", path)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial unix socket: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitErr(ch <-chan error, t *testing.T) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := <-ch; err != nil {
			t.Errorf("daemon returned error: %v", err)
		}
	}()
	return done
}
