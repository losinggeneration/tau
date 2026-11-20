package coord

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// Inspect the socket peer, not an arbitrary PID supplied in a worker frame.
// procfs keeps the running executable accessible after an atomic replacement.
func peerBuildID(conn net.Conn, pid int) string {
	peer, ok := conn.(*net.UnixConn)
	if !ok || pid <= 0 {
		return ""
	}
	raw, err := peer.SyscallConn()
	if err != nil {
		return ""
	}
	var creds *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		creds, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil || creds == nil || int(creds.Pid) != pid || int(creds.Uid) != os.Geteuid() {
		return ""
	}
	return ExecutableBuildID(fmt.Sprintf("/proc/%d/exe", creds.Pid))
}
