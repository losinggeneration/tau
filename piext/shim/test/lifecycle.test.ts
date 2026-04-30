// Lifecycle tests: the shim must die when the host shuts it down or when
// the host process disappears, instead of waiting for a SIGKILL that a
// fast-exiting host never delivers.

import { test } from "node:test";
import assert from "node:assert/strict";
import { exitWhenOrphaned } from "../src/lifecycle.ts";

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

test("exitWhenOrphaned exits when ppid changes", async () => {
	let ppid = 4242;
	const exits: number[] = [];
	exitWhenOrphaned(
		10,
		() => ppid,
		(code) => exits.push(code),
	);

	await sleep(35);
	assert.equal(exits.length, 0, "must not exit while the host ppid is unchanged");

	ppid = 1; // host died; orphan reparented to init/subreaper
	await sleep(35);
	assert.ok(exits.length > 0, "must exit once the host ppid changes");
	assert.ok(exits.every((code) => code === 0), `exit codes = ${exits.join(",")}, want all 0`);
});

test("exitWhenOrphaned tolerates ppid staying stable", async () => {
	const ppid = 4242;
	const exits: number[] = [];
	exitWhenOrphaned(
		10,
		() => ppid,
		(code) => exits.push(code),
	);

	await sleep(35);
	assert.equal(exits.length, 0);
});
