package piext

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// outputTailLimit bounds how much captured subprocess output is appended to
// an install error. Enough to diagnose an npm failure, small enough that the
// error stays readable where it surfaces (chat log row, CLI line).
const outputTailLimit = 2_000

// NPMInstall installs production shim dependencies with output captured
// rather than inherited. Inheriting the descriptors was fine for the
// headless CLI, but the same function runs inside the interactive TUI (plugin
// reload triggers the shim ensure), where subprocess output writes below the
// rendered screen and scrolls it — the editor visibly jumps toward the top.
// The captured tail is appended to the error so failures stay diagnosable.
func NPMInstall(ctx context.Context, dir string) error {
	var buf bytes.Buffer
	if err := npmCICmd(ctx, dir, &buf).Run(); err != nil {
		return fmt.Errorf("npm ci: %w%s", err, outputTail(&buf))
	}
	return nil
}

// npmCICmd builds the npm install command with both output streams wired to
// w. Split out so tests can pin the descriptors without running a subprocess.
func npmCICmd(ctx context.Context, dir string, w io.Writer) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "npm", "ci", "--ignore-scripts", "--omit=dev", "--prefix", dir)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd
}

// outputTail returns "\n" plus the trailing outputTailLimit bytes of the
// captured output, with surrounding whitespace trimmed. Empty when nothing
// was captured.
func outputTail(buf *bytes.Buffer) string {
	data := buf.Bytes()
	if len(data) > outputTailLimit {
		data = data[len(data)-outputTailLimit:]
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return ""
	}
	return "\n" + trimmed
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
