package piext

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// NPMInstall installs production shim dependencies.
func NPMInstall(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "npm", "ci", "--omit=dev", "--prefix", dir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm ci: %w", err)
	}
	return nil
}

// StderrStatus writes a status line to stderr.
func StderrStatus(message string) {
	fmt.Fprintln(os.Stderr, message)
}

// TimedNPMInstall reports the one-time install status around NPMInstall.
func TimedNPMInstall(ctx context.Context, dir string) error {
	start := time.Now()
	fmt.Fprintln(os.Stderr, "Installing pi-ext-shim (one-time, ~30s)…")
	if err := NPMInstall(ctx, dir); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Done. (%s)\n", time.Since(start).Round(time.Second))
	return nil
}
