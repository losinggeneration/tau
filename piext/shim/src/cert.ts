// Self-signed cert generation for go-plugin AutoMTLS.
//
// The certificate profile mirrors go-plugin's own mtls.go generateCert():
// CN "localhost" with a matching DNS SAN (the go-plugin client dials with
// ServerName "localhost" and verifies the hostname), IsCA, and both
// client/server auth usages. Deviating from this profile makes the host's
// TLS handshake fail hostname verification — see the interop test.

import forge from "node-forge";

export interface GeneratedCert {
	certPem: string;
	keyPem: string;
	/** DER bytes as a binary (latin1) string — base64 it for the handshake line. */
	certDer: string;
}

/**
 * Generate a self-signed cert acceptable to go-plugin's AutoMTLS client.
 */
export function generateSelfSignedCert(options?: { commonName?: string; days?: number }): GeneratedCert {
	const commonName = options?.commonName ?? "localhost";
	const days = options?.days ?? 30;

	const keys = forge.pki.rsa.generateKeyPair({ bits: 2048, e: 0x10001 });
	const cert = forge.pki.createCertificate();
	cert.publicKey = keys.publicKey;
	cert.serialNumber = `01${Date.now().toString(16)}`;
	cert.validity.notBefore = new Date(Date.now() - 30 * 1000);
	cert.validity.notAfter = new Date(Date.now() + days * 24 * 60 * 60 * 1000);
	const attrs = [{ shortName: "CN", value: commonName }];
	cert.setSubject(attrs);
	cert.setIssuer(attrs);
	// Match go-plugin's extension profile: CA constraint, DNS SAN for the
	// ServerName the client verifies, and client+server auth usages.
	cert.setExtensions([
		{ name: "basicConstraints", cA: true },
		{ name: "keyUsage", keyCertSign: true, digitalSignature: true, keyEncipherment: true, keyAgreement: true },
		{ name: "extKeyUsage", serverAuth: true, clientAuth: true },
		{ name: "subjectAltName", altNames: [{ type: 2, value: commonName }] },
	]);
	cert.sign(keys.privateKey, forge.md.sha256.create());

	const certAsn1 = forge.pki.certificateToAsn1(cert);
	const certDer = forge.asn1.toDer(certAsn1).getBytes();

	return {
		certPem: forge.pki.certificateToPem(cert),
		keyPem: forge.pki.privateKeyToPem(keys.privateKey),
		certDer,
	};
}

/** keyCertPairs element shape grpc-js ServerCredentials.createSsl expects
 * (snake_case, matching @grpc/grpc-js's KeyCertPair). */
export interface KeyCertPair {
	private_key: Buffer;
	cert_chain: Buffer;
}

export interface MTLSMaterial {
	/** Concatenated CA PEM(s) grpc-js ServerCredentials.createSsl expects as rootCerts. */
	rootCerts: Buffer;
	keyCertPairs: KeyCertPair[];
	checkClientCertificate: boolean;
}

/**
 * Build the mTLS material for grpc-js ServerCredentials.createSsl when
 * AutoMTLS is in effect.
 */
export function buildMTLSMaterial(
	clientCertCaPem: string,
	serverCertPem: string,
	serverKeyPem: string,
): MTLSMaterial {
	return {
		rootCerts: Buffer.from(clientCertCaPem, "utf8"),
		keyCertPairs: [
			{
				private_key: Buffer.from(serverKeyPem, "utf8"),
				cert_chain: Buffer.from(serverCertPem, "utf8"),
			},
		],
		checkClientCertificate: true,
	};
}

/** DER (binary string) → base64 without padding (go-plugin RawStdEncoding). */
export function derToRawBase64(der: string): string {
	return Buffer.from(der, "binary").toString("base64").replace(/=+$/, "");
}
