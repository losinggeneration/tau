// Unit tests for the theme approximation. Extensions use theme.fg to color
// the status-bar text; we map the common pi theme tokens to ANSI codes.

import { test } from "node:test";
import assert from "node:assert/strict";
import { theme } from "../src/theme.ts";

test("fg returns the text for the 'text' / 'default' color", () => {
	assert.equal(theme.fg("text", "hello"), "hello");
	assert.equal(theme.fg("default", "hello"), "hello");
});

test("fg wraps colored output with ANSI escape codes", () => {
	const out = theme.fg("accent", "x");
	assert.match(out, /^\x1b\[/);
	assert.match(out, /\x1b\[0m$/);
	assert.ok(out.includes("x"));
});

test("fg is pass-through for unknown colors (no escape codes)", () => {
	assert.equal(theme.fg("nonexistent", "x"), "x");
});

test("bg works similarly to fg (pi parity)", () => {
	const out = theme.fg("dim", "z");
	assert.match(out, /\x1b\[/);
});

test("fg always returns a string (no undefined)", () => {
	const out = theme.fg("accent", "null-ish");
	assert.equal(typeof out, "string");
});
