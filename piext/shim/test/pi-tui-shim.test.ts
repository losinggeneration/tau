import { test } from "node:test";
import assert from "node:assert/strict";
import { stripTerminalSequences } from "../src/shims/pi-tui/index.ts";

test("strips SGR color sequences", () => {
	assert.equal(stripTerminalSequences("\x1b[31mhello\x1b[0m"), "hello");
});

test("strips CSI cursor sequences", () => {
	assert.equal(stripTerminalSequences("a\x1b[2Kb"), "ab");
});

test("returns plain strings untouched", () => {
	assert.equal(stripTerminalSequences("plain text"), "plain text");
});

test("return value is iterable (pi-goal spreads it)", () => {
	const out = stripTerminalSequences("\x1b[1mab\x1b[0m");
	assert.deepEqual([...out], ["a", "b"]);
});
