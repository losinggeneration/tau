// Entry point for the pi-ext-shim.
//
// argv after the script name is a list of extension entry paths (relative to
// cwd, which is the package dir set by tau's `sh -c` launch). Tau supplies the
// plugin identity through TAU_PLUGIN_NAME. Standalone launches fall back to
// the last path component of cwd.
//
// Lifecycle:
//   1. Prepare the handshake (magic cookie, mTLS cert, socket path)
//   2. Build the Plugin service impl (loads extensions on Initialize)
//   3. Bind the gRPC server, then print the handshake line (bind → print,
//      matching go-plugin's server ordering so the host never races)
//   4. Exit on the host's Shutdown RPC, SIGTERM/SIGINT, or host death —
//      never wait for a SIGKILL that a fast-exiting host won't deliver.

import { resolvePluginName } from "./host.ts";
import { prepareHandshake, printHandshakeLine } from "./handshake.ts";
import { exitWhenOrphaned } from "./lifecycle.ts";
import { buildPluginService, startServer } from "./server.ts";

const cwd = process.cwd();
const pluginName = resolvePluginName(cwd);

const extensionEntries = process.argv.slice(2);
if (extensionEntries.length === 0) {
	process.stderr.write("pi-ext-shim: no extension entry paths provided. Usage: node index.js <entry> [entry...]\n");
	process.exit(2);
}

try {
	const prep = await prepareHandshake(pluginName);
	const pluginService = buildPluginService({ extensionEntries, cwd, pluginName });
	await startServer({
		sockPath: prep.sockPath,
		mtls: prep.mtls,
		pluginService,
		onBound: () => printHandshakeLine(prep.sockPath, prep.certB64Raw),
	});
	// Watch for host death: if tau is SIGKILLed or crashes, no shutdown RPC
	// or signal reaches us and an orphaned shim would linger forever.
	exitWhenOrphaned();
} catch (e) {
	process.stderr.write(`pi-ext-shim: fatal during startup: ${e instanceof Error ? e.message : String(e)}\n`);
	process.exit(1);
}

// On SIGTERM (host killing us after Shutdown timeout), exit cleanly.
process.on("SIGTERM", () => process.exit(0));
process.on("SIGINT", () => process.exit(0));
