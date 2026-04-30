// Simple pi extension fixture used by unit tests. Mirrors common pi extension
// patterns so the shim's translation paths are exercised end-to-end.
// Written in TS to exercise the jiti TS-loading path.

import type { PiAPI } from "../../src/pi-types.ts";

export const description = "test extension";

export default function (pi: PiAPI): void {
	pi.on("before_agent_start", async (event) => {
		return { systemPrompt: (event.systemPrompt ?? "") + "\n[test-rules]" };
	});

	pi.on("input", (event) => {
		return event;
	});

	pi.on("agent_start", () => {
		// shim tracks isIdle from these internally
	});

	pi.on("agent_end", () => {});

	pi.on("session_start", () => {});

	pi.registerCommand("hello", {
		description: "say hi",
		handler: async (args, ctx) => {
			ctx.ui.notify(`hello ${args || "world"}`, "info");
			return `hello ${args || "world"}`;
		},
	});

	pi.registerCommand("mode", {
		description: "set mode",
		handler: async (args, ctx) => {
			await pi.appendEntry("test-mode", { mode: String(args || "") });
			ctx.ui.setStatus("test", "active");
		},
	});
}
