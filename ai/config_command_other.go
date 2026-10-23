//go:build !unix

package ai

import "os/exec"

func configureConfigCommand(cmd *exec.Cmd) {
	// ponytail: CommandContext kills the direct process; use a Windows job object
	// if configuration commands need descendant-process cleanup on Windows.
}
