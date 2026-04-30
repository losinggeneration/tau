// Process lifecycle: how the shim decides to die.
//
// The host kills plugins with go-plugin's Client.Kill(), which attempts a
// graceful controller shutdown, waits up to 2s, and only then SIGKILLs. A
// host that exits quickly (one-shot tau commands) can orphan the shim before
// that SIGKILL lands, so the shim also exits on its own:
//   - after acknowledging the Plugin.Shutdown RPC (scheduleExit)
//   - when the host process disappears (exitWhenOrphaned — covers host
//     SIGKILL/crash, where no shutdown RPC or signal ever reaches us)

/** Exit shortly after acknowledging Shutdown. The delay lets the RPC
 * response flush to the host before the process goes away. */
export function scheduleExit(delayMs: number = 150): void {
	setTimeout(() => process.exit(0), delayMs);
}

/**
 * Exit when the host (tau) process dies. A child's ppid only changes when
 * its parent dies and it is reparented (to init or a subreaper), so a
 * changed ppid is a reliable host-death signal.
 */
export function exitWhenOrphaned(
	pollMs: number = 2000,
	getPPid: () => number = () => process.ppid,
	exit: (code: number) => void = (code) => process.exit(code),
): void {
	const hostPid = getPPid();
	const timer = setInterval(() => {
		if (getPPid() !== hostPid) {
			exit(0);
		}
	}, pollMs);
	timer.unref();
}
