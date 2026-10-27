import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, writeFile, symlink, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { loadExtension } from "../src/loader.ts";

test("linked extension resolves installed dependencies and source edits", async () => {
	const root = await mkdtemp(path.join(tmpdir(), "tau-linked-loader-"));
	try {
		const source = path.join(root, "source");
		const installed = path.join(root, "installed");
		const dependency = path.join(installed, "node_modules", "test-linked-dep");
		await mkdir(path.join(source, "extensions"), { recursive: true });
		await mkdir(dependency, { recursive: true });
		await writeFile(path.join(dependency, "package.json"), JSON.stringify({ name: "test-linked-dep", main: "index.js" }));
		await writeFile(path.join(dependency, "index.js"), "module.exports = 42;");
		await symlink(path.join(source, "extensions"), path.join(installed, "extensions"));
		for (const dialect of ["ts", "cjs", "mjs"]) {
			const entry = path.join(source, "extensions", `ext.${dialect}`);
			for (const increment of [0, 1]) {
				await writeFile(entry, dialect !== "cjs"
					? `import value from "test-linked-dep"; export default function() { return value + ${increment}; }`
					: `const value = require("test-linked-dep"); module.exports = function() { return value + ${increment}; };`);
				const extension = await loadExtension(`extensions/ext.${dialect}`, installed);
				assert.equal(typeof extension, "function");
				if (!extension) throw new Error("missing extension");
				assert.equal(Reflect.apply(extension, undefined, []), 42 + increment);
			}
		}
	} finally {
		await rm(root, { recursive: true, force: true });
	}
});
