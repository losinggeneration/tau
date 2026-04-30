// go-plugin handshake implementation.
//
// Flow (mirrors go-plugin's server.go ordering: bind → print → serve):
//   1. Verify magic cookie env var TAU_PLUGIN_API_VERSION == "1"
//   2. Generate a self-signed cert (AutoMTLS); read PLUGIN_CLIENT_CERT for CA
//   3. Pick the socket path (PLUGIN_UNIX_SOCKET_DIR or a fresh temp dir)
//   4. [caller] startServer: grpc bindAsync → on bound, printHandshakeLine
//      CORE|APP|NETWORK|ADDR|PROTO|CERT
//      1     1    unix    path  grpc  <base64 DER cert, RawStdEncoding>
//
// stdout is reserved for the handshake line — anything written to
// process.stdout after this point corrupts the protocol. All shim
// diagnostics go to stderr.

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { buildMTLSMaterial, derToRawBase64, generateSelfSignedCert, type MTLSMaterial } from "./cert.ts";

export const MAGIC_COOKIE_KEY = "TAU_PLUGIN_API_VERSION";
export const MAGIC_COOKIE_VALUE = "1";
export const PROTO_VERSION = 1;
export const CORE_PROTOCOL_VERSION = 1;

export interface HandshakePrep {
	sockPath: string;
	certB64Raw: string;
	mtls: MTLSMaterial | undefined;
}

function die(msg: string, code = 1): never {
	process.stderr.write(`pi-ext-shim: ${msg}\n`);
	process.exit(code);
}

function verifyMagicCookie(): void {
	const v = process.env[MAGIC_COOKIE_KEY];
	if (v !== MAGIC_COOKIE_VALUE) {
		die(
			`magic cookie mismatch: ${MAGIC_COOKIE_KEY}=${JSON.stringify(v)} (expected ${JSON.stringify(MAGIC_COOKIE_VALUE)}). Was this launched by tau's plugin manager?`,
		);
	}
}

/**
 * Find a socket dir to bind. PLUGIN_UNIX_SOCKET_DIR is the official env var
 * when the host uses go-plugin's RunnerFunc path; for our path (tau launches
 * a subprocess directly) it's typically unset, so fall back to a fresh
 * per-process dir under os.tmpdir().
 */
function pickSocketDir(): string {
	const env = process.env["PLUGIN_UNIX_SOCKET_DIR"];
	if (env && env !== "") return env;
	return fs.mkdtempSync(path.join(os.tmpdir(), "tau-pi-shim-"));
}

/**
 * Prepare the handshake: verify the cookie, pick the socket path, generate
 * mTLS material. Does NOT bind or print — the caller binds the gRPC server
 * first and prints the handshake line from the bound callback so the host
 * never races a missing listener.
 */
export async function prepareHandshake(_pluginName: string): Promise<HandshakePrep> {
	verifyMagicCookie();

	const sockDir = pickSocketDir();
	const sockPath = path.join(sockDir, `plugin-${process.pid}.sock`);
	// Clear a stale socket file from a crashed previous run (same pid reuse).
	try {
		fs.unlinkSync(sockPath);
	} catch (e) {
		if ((e as NodeJS.ErrnoException).code !== "ENOENT") throw e;
	}

	const clientCAPem = process.env["PLUGIN_CLIENT_CERT"];
	let mtls: MTLSMaterial | undefined;
	let certB64Raw = "";
	if (clientCAPem) {
		// AutoMTLS: mint a self-signed cert and build mTLS material.
		const { certPem, keyPem, certDer } = generateSelfSignedCert({
			commonName: "localhost",
		});
		mtls = buildMTLSMaterial(clientCAPem, certPem, keyPem);
		certB64Raw = derToRawBase64(certDer);
	}
	// No AutoMTLS (host didn't set the env): serve plaintext. Production tau
	// always sets AutoMTLS so PLUGIN_CLIENT_CERT is present; the insecure path
	// covers tests and hosts that opt out.

	return { sockPath, certB64Raw, mtls };
}

/**
 * Print the go-plugin handshake line to stdout. Must be called only after
 * the gRPC server is bound to sockPath.
 */
export function printHandshakeLine(sockPath: string, certB64Raw: string): void {
	const line = [CORE_PROTOCOL_VERSION, PROTO_VERSION, "unix", sockPath, "grpc", certB64Raw].join("|");
	process.stdout.write(`${line}\n`);
}
