// Simple pi extension fixture used by unit tests. Mirrors common pi extension
// patterns so the shim's translation paths are exercised end-to-end.
// Written in TS to exercise the jiti TS-loading path.

import type { PiAPI } from "../../src/pi-types.ts";

export const description = "test extension";

export default function (pi: PiAPI): void {
	pi.registerTool({
		name: "lookup",
		description: "metadata fixture",
		parameters: { type: "object", properties: {} },
		exposure: "deferred",
		outputSchema: { type: "object" },
		constrainedSampling: { type: "json_schema", strict: "require" },
		execute: () => ({
			content: [{ type: "text", text: "ok" }],
			structuredContent: { found: true },
			usage: { input: 1, output: 2, totalTokens: 3 },
		}),
	});
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

	pi.registerVirtualModel({
		provider: "router",
		id: "auto",
		name: "Auto",
		thinkingLevels: ["off", "high"],
		contextWindow: 1000,
		route: (request: { reason: string; state?: { routes: number }; thinkingLevel: string }) => {
			if (request.reason === "direct") throw new Error("no direct routes");
			if (request.state)
				return {
					model: { provider: "anthropic", id: "claude-sonnet-4-6" },
					thinkingLevel: request.thinkingLevel,
					state: request.state,
				};
			return {
				model: { provider: "anthropic", id: "claude-sonnet-4-6" },
				thinkingLevel: request.thinkingLevel,
				state: { routes: 1 },
			};
		},
	});
	pi.registerVirtualModel({
		provider: "router",
		id: "gone",
		name: "Gone",
		route: () => ({ model: { provider: "x", id: "y" }, thinkingLevel: "off" }),
	});
	pi.unregisterVirtualModel("router", "gone");

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
