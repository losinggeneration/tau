// Cert generation smoke test: node-forge can mint a self-signed cert and we
// can serialize the DER for the handshake line (RawStdEncoding base64).

import { test } from "node:test";
import assert from "node:assert/strict";
import { derToRawBase64, generateSelfSignedCert } from "../src/cert.ts";

test("generates a self-signed cert with PEM + DER", () => {
	const { certPem, keyPem, certDer } = generateSelfSignedCert({ commonName: "test" });
	assert.ok(certPem.startsWith("-----BEGIN CERTIFICATE-----"));
	assert.ok(keyPem.startsWith("-----BEGIN RSA PRIVATE KEY-----"));
	assert.ok(certDer.length > 0);
});

test("derToRawBase64 strips padding (go-plugin RawStdEncoding)", () => {
	const { certDer } = generateSelfSignedCert({ commonName: "roundtrip" });
	const b64 = derToRawBase64(certDer);
	assert.ok(b64.length > 100, "cert DER suspiciously small");
	assert.ok(!b64.includes("="), "RawStdEncoding output must not contain padding");
	// Round-trip: decode and compare lengths.
	const decoded = Buffer.from(b64, "base64");
	assert.equal(decoded.length, Buffer.from(certDer, "binary").length);
});
