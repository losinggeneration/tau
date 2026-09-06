package piext

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// NPMInstall is invoked from inside the interactive TUI (plugin reload runs
// the shim ensure). The npm subprocess must never write to the inherited
// stdout/stderr: in raw terminal mode that output lands below the rendered
// screen and scrolls it up — the input editor visibly jumps toward the top of
// the screen. This pins the command construction to a captured writer.
func TestNPMInstallCapturesSubprocessOutput(t *testing.T) {
	var buf bytes.Buffer
	cmd := npmCICmd(context.Background(), t.TempDir(), &buf)

	if cmd.Stdout != &buf {
		t.Errorf("cmd.Stdout = %T, want the captured buffer (inherited writers scroll the TUI)", cmd.Stdout)
	}
	if cmd.Stderr != &buf {
		t.Errorf("cmd.Stderr = %T, want the captured buffer (inherited writers scroll the TUI)", cmd.Stderr)
	}
	if len(cmd.Args) < 4 || cmd.Args[0] != "npm" {
		t.Errorf("cmd.Args = %v, want npm ci --omit=dev --prefix <dir>", cmd.Args)
	}
}

// The buffer type here matters: NPMInstall passes a *bytes.Buffer, so the cap
// has to hold for that concrete type. An earlier version keyed truncation off
// io.ReadSeeker, which a bytes.Buffer does not implement — the test passed
// against a strings.Reader while the real error stayed unbounded.
func TestOutputTail(t *testing.T) {
	if got := outputTail(&bytes.Buffer{}); got != "" {
		t.Errorf("outputTail(empty) = %q, want empty", got)
	}
	if got := outputTail(bytes.NewBufferString(" \n\t ")); got != "" {
		t.Errorf("outputTail(whitespace) = %q, want empty", got)
	}

	got := outputTail(bytes.NewBufferString(strings.Repeat("x", outputTailLimit+50)))
	if len(got) != outputTailLimit+1 { // leading newline + limit bytes
		t.Errorf("outputTail length = %d, want %d (tail must be capped)", len(got), outputTailLimit+1)
	}
	if !strings.HasPrefix(got, "\n") {
		t.Errorf("outputTail = %q, want leading newline separating it from the error", got)
	}

	multiline := bytes.NewBufferString("line one\nline two\nline three\n")
	if got := outputTail(multiline); got != "\nline one\nline two\nline three" {
		t.Errorf("outputTail = %q, want trimmed multiline output", got)
	}
}
