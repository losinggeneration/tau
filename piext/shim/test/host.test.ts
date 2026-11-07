// Host client integration test: stand up a fake Host gRPC server on a unix
// socket, point the HostClient at it, and verify each method lands correctly.

import { test } from "node:test";
import assert from "node:assert/strict";
import { Metadata, Server, ServerCredentials, status, type UntypedServiceImplementation } from "@grpc/grpc-js";
import path from "node:path";
import os from "node:os";
import fs from "node:fs";
import { HostClient, resolvePluginName } from "../src/host.ts";
import { loadProto } from "../src/grpc.ts";
import type {
	SessionAppendRequestP,
	SessionSendUserMessageRequestP,
	UINotifyRequestP,
} from "../src/proto-types.ts";

type FakeCallback = (err: { code: number; message: string } | null, value?: unknown) => void;

interface RecordedCall {
	method: string;
	request: unknown;
}

function startFakeHost(
	onCall: (method: string, request: unknown, cb: FakeCallback, metadata: Metadata) => void,
): Promise<{ server: Server; sockPath: string; calls: RecordedCall[] }> {
	const calls: RecordedCall[] = [];
	const sockPath = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "fake-host-")), "host.sock");
	const server = new Server();
	const impl: Record<string, (call: { request: unknown; metadata: Metadata }, cb: FakeCallback) => void> = {};
	const methods = [
		"UIConfirm",
		"UISelect",
		"UIInput",
		"UINotify",
		"UISetStatus",
		"UISetWidget",
		"Log",
		"GetConfig",
		"ToolProgress",
		"StateGet",
		"StateSet",
		"StateAppend",
		"StateDelete",
		"SessionAppend",
		"SessionScan",
		"SessionSetLabel",
		"SessionGetLabel",
		"SessionSendUserMessage",
	];
	for (const m of methods) {
		impl[m] = (call, cb) => {
			calls.push({ method: m, request: call.request });
			onCall(m, call.request, cb, call.metadata);
		};
	}
	const { HostService } = loadProto();
	server.addService(HostService, impl as unknown as UntypedServiceImplementation);
	return new Promise((resolve) => {
		server.bindAsync(`unix:${sockPath}`, ServerCredentials.createInsecure(), () => {
			resolve({ server, sockPath, calls });
		});
	});
}

function makeClient(sockPath: string): HostClient {
	process.env["TAU_HOST_SOCKET"] = sockPath;
	return new HostClient("test-plugin");
}

test("host: sessionAppend sends entryType + JSON payload", async (t) => {
	const { server, sockPath, calls } = await startFakeHost((_m, _req, cb) => cb(null, { entryId: "entry-1" }));
	t.after(() => {
		try {
			server.forceShutdown();
		} catch {}
		delete process.env["TAU_HOST_SOCKET"];
	});
	const client = makeClient(sockPath);
	t.after(() => client.close());

	const resp = await client.sessionAppend("custom", JSON.stringify({ type: "test", data: 1 }));
	assert.equal(resp.entryId, "entry-1");
	const call = calls.find((c) => c.method === "SessionAppend") as { request: SessionAppendRequestP } | undefined;
	assert.ok(call, "SessionAppend not called");
	assert.equal(call.request.entryType, "custom");
	const payload = JSON.parse(call.request.payload ?? "") as { type: string; data: number };
	assert.equal(payload.type, "test");
	assert.equal(payload.data, 1);
});

test("host: sessionSendUserMessage routes text + deliver_as", async (t) => {
	const { server, sockPath, calls } = await startFakeHost((_m, _req, cb) => cb(null, { ok: true }));
	t.after(() => {
		try {
			server.forceShutdown();
		} catch {}
		delete process.env["TAU_HOST_SOCKET"];
	});
	const client = makeClient(sockPath);
	t.after(() => client.close());

	const resp = await client.sessionSendUserMessage("hi from shim", "followUp");
	assert.equal(resp.ok, true);
	const call = calls.find((c) => c.method === "SessionSendUserMessage") as
		| { request: SessionSendUserMessageRequestP }
		| undefined;
	assert.ok(call, "SessionSendUserMessage not called");
	assert.equal(call.request.text, "hi from shim");
	assert.equal(call.request.deliverAs, "followUp");
});

test("host: uiNotify maps level names to ints", async (t) => {
	const { server, sockPath, calls } = await startFakeHost((_m, _req, cb) => cb(null, {}));
	t.after(() => {
		try {
			server.forceShutdown();
		} catch {}
		delete process.env["TAU_HOST_SOCKET"];
	});
	const client = makeClient(sockPath);
	t.after(() => client.close());

	await client.uiNotify("careful!", "warning");
	const call = calls.find((c) => c.method === "UINotify") as { request: UINotifyRequestP } | undefined;
	assert.ok(call, "UINotify not called");
	assert.equal(call.request.level, 2);
	assert.equal(call.request.message, "careful!");
});

test("host: missing TAU_HOST_SOCKET rejects on first call", async () => {
	process.env["TAU_HOST_SOCKET"] = "";
	const client = new HostClient("test");
	await assert.rejects(
		() => client.uiNotify("x"),
		/TAU_HOST_SOCKET not set/,
	);
});

test("host: launch identity overrides the package working directory", async (t) => {
	const previousName = process.env["TAU_PLUGIN_NAME"];
	const previousSocket = process.env["TAU_HOST_SOCKET"];
	t.after(() => {
		if (previousName === undefined) delete process.env["TAU_PLUGIN_NAME"];
		else process.env["TAU_PLUGIN_NAME"] = previousName;
		if (previousSocket === undefined) delete process.env["TAU_HOST_SOCKET"];
		else process.env["TAU_HOST_SOCKET"] = previousSocket;
	});
	process.env["TAU_PLUGIN_NAME"] = "extension";
	const { server, sockPath } = await startFakeHost((_method, _request, cb, metadata) => {
		const names = metadata.get("tau-plugin-name");
		if (names.length !== 1 || names[0] !== "extension") {
			cb({ code: status.PERMISSION_DENIED, message: "plugin identity does not match Host endpoint" });
			return;
		}
		cb(null, { exists: true, value: '"ultra"' });
	});
	t.after(() => {
		server.forceShutdown();
		fs.rmSync(path.dirname(sockPath), { recursive: true, force: true });
	});
	process.env["TAU_HOST_SOCKET"] = sockPath;
	const client = new HostClient(resolvePluginName("/packages/ponytail"));
	t.after(() => client.close());
	assert.deepEqual(await client.stateGet("mode"), { exists: true, value: '"ultra"' });
});

test("host: standalone identity falls back to the working directory", (t) => {
	const previousName = process.env["TAU_PLUGIN_NAME"];
	t.after(() => {
		if (previousName === undefined) delete process.env["TAU_PLUGIN_NAME"];
		else process.env["TAU_PLUGIN_NAME"] = previousName;
	});
	delete process.env["TAU_PLUGIN_NAME"];
	assert.equal(resolvePluginName("/packages/ponytail"), "ponytail");
	process.env["TAU_PLUGIN_NAME"] = "";
	assert.equal(resolvePluginName("/packages/ponytail"), "ponytail");
});
