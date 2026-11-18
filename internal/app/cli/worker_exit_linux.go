package cli

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Keep the PID reserved until the launcher has cleaned up the process group.
func workerExited(pid int) (bool, error) {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("check worker exit: %w", err)
		}
		return info.Signo != 0, nil
	}
}
