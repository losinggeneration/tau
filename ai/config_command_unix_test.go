//go:build unix

package ai

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestConfigCommandCancellationKillsProcessGroup(t *testing.T) {
	cmd := &exec.Cmd{}
	configureConfigCommand(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.Cancel == nil {
		t.Fatal("command cancellation does not own its process group")
	}
	for _, failure := range []error{nil, syscall.ESRCH, syscall.EPERM} {
		called := false
		err := killConfigCommandGroup(123, func(pid int, signal syscall.Signal) error {
			called = true
			if pid != -123 || signal != syscall.SIGKILL {
				t.Error("incorrect process group termination")
			}
			return failure
		})
		want := failure
		if errors.Is(want, syscall.ESRCH) {
			want = os.ErrProcessDone
		}
		if !called || !errors.Is(err, want) {
			t.Fatalf("kill error=%v want=%v", err, want)
		}
	}
}
