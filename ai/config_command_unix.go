//go:build unix

package ai

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureConfigCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killConfigCommandGroup(cmd.Process.Pid, syscall.Kill) }
}

func killConfigCommandGroup(pid int, kill func(int, syscall.Signal) error) error {
	err := kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
