package server

import (
	"os"
	"path/filepath"
	"strconv"
)

const socketName = "tau.sock"

// SocketPath returns the default Unix socket path for the current user.
func SocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "tau", socketName)
	}
	return filepath.Join(os.TempDir(), "tau-"+strconv.Itoa(os.Getuid()), socketName)
}
