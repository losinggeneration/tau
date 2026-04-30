// pi's SessionManager is synchronous; the shim pre-fetches entries
// on session_start and serves them from shimState. An async contract would
// silently break extensions that read their custom entries without awaiting.

import { test } from "node:test";
import assert from "node:assert/strict";
import { SessionManager, refreshSessionEntries } from "../src/session-manager.ts";
import { shimState } from "../src/state.ts";
import type { HostClient } from "../src/host.ts";

class FakeHost {
	constructor(private readonly result: unknown) {}
	async sessionScan() {
		if (this.result instanceof Error) throw this.result;
		return this.result as never;
	}
}

test("refreshSessionEntries stores normalized entries in shimState", async () => {
	shimState.setSessionEntries([]);
	const host = new FakeHost({
		entries: [
			{ id: "e1", entryType: "custom", customType: "test-mode", payload: '{"mode":"ultra"}', parentId: "" },
		],
	});
	await refreshSessionEntries(host as unknown as HostClient);
	const entries = new SessionManager().getEntries();
	assert.equal(entries.length, 1);
	assert.equal(entries[0]?.type, "custom");
	assert.equal(entries[0]?.customType, "test-mode");
	assert.deepEqual(entries[0]?.data, { mode: "ultra" });
});

test("refreshSessionEntries swallows scan failures (lazy / unowned session)", async () => {
	shimState.setSessionEntries([{ id: "stale" } as never]);
	const host = new FakeHost(new Error("session not found"));
	await refreshSessionEntries(host as unknown as HostClient);
	assert.deepEqual(new SessionManager().getEntries(), []);
});

test("getEntries is synchronous (returns an array, not a Promise)", () => {
	shimState.setSessionEntries([
		{ id: "x", entryType: "custom", customType: "c", payload: {}, parentId: "", timestamp: "", label: "" } as never,
	]);
	const e = new SessionManager().getEntries();
	assert.ok(Array.isArray(e));
});