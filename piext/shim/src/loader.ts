// jiti-based pi extension loader.
//
// Loads a JS/TS/CJS/ESM extension entry and returns its default export
// (factory function). jiti handles all four dialects transparently,
// matching how pi itself loads extensions.
//
// Extensions may mix ESM with a `createRequire` bridge to CJS helpers. jiti
// handles that out of the box.

import { createJiti } from "jiti";
import path from "node:path";
import type { ExtensionFactory } from "./pi-types.ts";

/**
 * Load a single extension entry file.
 *
 * @param entryPath  absolute or cwd-relative path
 * @param cwd        process cwd (the package dir)
 * @returns the factory function, or null if the entry exports none
 */
export async function loadExtension(entryPath: string, cwd: string): Promise<ExtensionFactory | null> {
	const absolute = path.isAbsolute(entryPath) ? entryPath : path.resolve(cwd, entryPath);
	const jiti = createJiti(import.meta.url, {
		moduleCache: false,
		interopDefault: true,
		alias: piModuleAliases(cwd),
	});
	let mod: unknown;
	try {
		mod = await jiti.import(absolute, { default: true });
	} catch (e) {
		throw new Error(`failed to import extension ${absolute}: ${e instanceof Error ? e.message : String(e)}`);
	}
	if (typeof mod === "function") return mod as ExtensionFactory;
	if (mod && typeof (mod as { default?: unknown }).default === "function") {
		return (mod as { default: ExtensionFactory }).default;
	}
	return null;
}

function piModuleAliases(_cwd: string): Record<string, string> {
	const shimDir = path.resolve(import.meta.dirname, "shims");
	return {
		"@earendil-works/pi-coding-agent": path.join(shimDir, "pi-coding-agent", "index.ts"),
		"@earendil-works/pi-coding-agent/compat": path.join(shimDir, "pi-coding-agent", "compat.ts"),
		"@mariozechner/pi-coding-agent": path.join(shimDir, "pi-coding-agent", "index.ts"),
		"@mariozechner/pi-coding-agent/compat": path.join(shimDir, "pi-coding-agent", "compat.ts"),
		"@earendil-works/pi-agent-core": path.join(shimDir, "pi-agent-core", "index.ts"),
		"@earendil-works/pi-ai": path.join(shimDir, "pi-ai", "index.ts"),
		"@earendil-works/pi-ai/compat": path.join(shimDir, "pi-ai", "compat.ts"),
		"@earendil-works/pi-tui": path.join(shimDir, "pi-tui", "index.ts"),
	};
}
