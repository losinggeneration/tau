import { test } from "node:test";
import assert from "node:assert/strict";
import { toolParametersToMap } from "../src/pi-api.ts";

test("tool parameter transport retains every JSON schema keyword", () => {
	const schema = {
		type: "object",
		properties: { value: { type: "string" } },
		required: ["value"],
		additionalProperties: false,
		$defs: { entry: { type: "string" } },
		description: "lookup",
		minProperties: 1,
	};
	const wire = toolParametersToMap(schema);
	const decoded = Object.fromEntries(Object.entries(wire).map(([key, value]) => [key, JSON.parse(value)]));
	assert.deepEqual(decoded, schema);
});

import { buildPi, refreshToolLoadout, flushToolLoadout } from "../src/pi-api.ts";
import { HostClient } from "../src/host.ts";
import { EventRouter } from "../src/events.ts";

class RecordingHost extends HostClient {
	calls: unknown[] = [];
	failure = "";
	override async sessionSendCustomMessage(
		message: Record<string, unknown>,
		options?: { triggerTurn?: boolean; deliverAs?: "steer" | "followUp" | "nextTurn" },
	) {
		this.calls.push({ message, options });
		return { ok: !this.failure, error: this.failure };
	}
}

test("sendMessage forwards custom context and delivery options and logs host errors", async () => {
	const host = new RecordingHost("test");
	const api = buildPi({ host, router: new EventRouter(), pluginName: "test" });
	const message = { customType: "notice", content: null, display: true, details: { source: "test" } };
	const options = { triggerTurn: false, deliverAs: "nextTurn" } as const;
	await api.sendMessage(message, options);
	assert.deepEqual(host.calls, [{ message, options }]);
	host.failure = "delivery rejected";
	await api.sendMessage(message);
	assert.equal(host.calls.length, 2);
});

class DiscoveryHost extends HostClient {
	requests: unknown[] = [];
	override async toolDiscovery(
		operation: string,
		options: { names?: string[]; query?: string; namespace?: string; limit?: number } = {},
	) {
		this.requests.push({ operation, ...options });
		if (options.query === "fail") throw new Error("discovery rejected");
		return [{ name: "lookup" }];
	}
}

test("asynchronous discovery forwards search scopes and descriptions and rejects failures", async () => {
	const host = new DiscoveryHost("test");
	const api = buildPi({ host, router: new EventRouter(), pluginName: "test" });
	assert.deepEqual(await api.searchTools("manuals", { namespace: "docs", limit: 3 }), [{ name: "lookup" }]);
	await api.describeTool("lookup");
	await api.describeNamespace("docs");
	assert.deepEqual(host.requests, [
		{ operation: "search", query: "manuals", namespace: "docs", limit: 3 },
		{ operation: "tool", query: "lookup" },
		{ operation: "namespace", query: "docs" },
	]);
	await assert.rejects(api.searchTools("fail"), /discovery rejected/);
});

class LoadoutHost extends HostClient {
	active = ["read"];
	registered = [{ name: "read" }, { name: "lookup", defaultActive: false }];
	override async toolDiscovery(operation: string, options: { names?: string[] } = {}) {
		if (operation === "registered") return this.registered;
		if (operation === "activate") {
			if (options.names?.includes("missing")) throw new Error("unknown tool");
			this.active = options.names ?? [];
		}
		return this.active;
	}
}

test("synchronous loadouts use refreshed snapshots and flush ordered activation failures", async () => {
	const host = new LoadoutHost("test");
	const api = buildPi({ host, router: new EventRouter(), pluginName: "test" });
	await refreshToolLoadout(api);
	assert.deepEqual(api.getActiveTools(), ["read"]);
	assert.equal(api.getAllTools().length, 2);
	api.getActiveTools().push("mutated");
	assert.deepEqual(api.getActiveTools(), ["read"]);
	api.setActiveTools(["lookup"]);
	assert.deepEqual(api.getActiveTools(), ["lookup"]);
	api.setActiveTools([]);
	await flushToolLoadout(api);
	assert.deepEqual(host.active, []);
	api.setActiveTools(["missing"]);
	await assert.rejects(flushToolLoadout(api), /unknown tool/);
	await refreshToolLoadout(api);
	assert.deepEqual(api.getActiveTools(), []);
});

test("plugin handlers preserve nested records, reach callbacks, and rejection distinctions", async () => {
	const { buildPluginService } = await import("../src/server.ts");
	const host = new LoadoutHost("test");
	const { api, handlers } = buildPluginService({
		host,
		extensionEntries: [],
		cwd: ".",
		pluginName: "test",
		onShutdownAcknowledged() {},
	});
	api.registerTool({
		name: "caller",
		reachesTools: async () => "all" as const,
		execute: () => ({
			isError: true,
			content: [{ type: "text", text: "usable" }],
			nestedCalls: Array.from({ length: 140 }, (_, i) => ({ id: String(i), name: "child", status: "ok" as const })),
		}),
	});
	const execute = (toolName: string) =>
		new Promise<Record<string, unknown>>((resolve, reject) =>
			handlers.ExecuteTool({ request: { toolName, arguments: "{}" } } as never, (error, result) =>
				error ? reject(error) : resolve(result as Record<string, unknown>),
			),
		);
	const reach = (args: string) =>
		new Promise<{ names: string[] }>((resolve, reject) =>
			handlers.ReachTools({ request: { toolName: "caller", arguments: args } } as never, (error, result) =>
				error ? reject(error) : resolve(result as { names: string[] }),
			),
		);
	assert.deepEqual(await reach("{}"), { names: ["all"] });
	await assert.rejects(reach("invalid"));
	for (const args of ["[]", "null", "1", '"text"']) await assert.rejects(reach(args), /object/);
	const explicit = await execute("caller");
	assert.equal(explicit.isError, true);
	assert.equal(explicit.rejection, undefined);
	const records = JSON.parse(explicit.nestedCalls as string);
	assert.equal(records.length, 128);
	assert.equal(records[0].id, "12");
	api.registerTool({
		name: "throws",
		execute() {
			throw new Error("execution failed");
		},
	});
	assert.equal((await execute("throws")).rejection, "execution failed");
});

test("failed tool handlers settle staged activation before replying and draining", async () => {
	const { buildPluginService } = await import("../src/server.ts");
	const host = new LoadoutHost("test");
	let release!: () => void;
	const pending = new Promise<void>((resolve) => {
		release = resolve;
	});
	const discovery = host.toolDiscovery.bind(host);
	host.toolDiscovery = async (operation, options) => {
		if (operation === "activate") await pending;
		return discovery(operation, options);
	};
	const { api, handlers, waitForInFlight } = buildPluginService({
		host,
		extensionEntries: [],
		cwd: ".",
		pluginName: "test",
	});
	let started!: () => void;
	const entered = new Promise<void>((resolve) => {
		started = resolve;
	});
	api.registerTool({
		name: "throws",
		execute() {
			api.setActiveTools(["lookup"]);
			started();
			throw new Error("tool failed");
		},
	});
	let replied = false;
	const response = new Promise<Record<string, unknown>>((resolve, reject) =>
		handlers.ExecuteTool({ request: { toolName: "throws", arguments: "{}" } } as never, (error, result) => {
			replied = true;
			error ? reject(error) : resolve(result as Record<string, unknown>);
		}),
	);
	await entered;
	let drained = false;
	const draining = waitForInFlight().then(() => {
		drained = true;
	});
	await Promise.resolve();
	await Promise.resolve();
	const prematureReply = replied;
	const prematureDrain = drained;
	release();
	const result = await response;
	await draining;
	assert.equal(prematureReply, false);
	assert.equal(prematureDrain, false);
	assert.equal(result.rejection, "tool failed");
	assert.deepEqual(host.active, ["lookup"]);
});

test("reach callbacks remain in shutdown draining until they settle", async () => {
	const { buildPluginService } = await import("../src/server.ts");
	const { api, handlers, waitForInFlight } = buildPluginService({
		host: new LoadoutHost("test"),
		extensionEntries: [],
		cwd: ".",
		pluginName: "test",
	});
	let release!: () => void;
	const pending = new Promise<void>((resolve) => {
		release = resolve;
	});
	api.registerTool({
		name: "caller",
		reachesTools: async () => {
			await pending;
			return ["lookup"];
		},
		execute() {},
	});
	const response = new Promise<unknown>((resolve, reject) =>
		handlers.ReachTools({ request: { toolName: "caller", arguments: "{}" } } as never, (error, result) =>
			error ? reject(error) : resolve(result),
		),
	);
	let drained = false;
	const draining = waitForInFlight().then(() => {
		drained = true;
	});
	await Promise.resolve();
	await Promise.resolve();
	const premature = drained;
	release();
	await response;
	await draining;
	assert.equal(premature, false);
});

test("factory activation waits for the first session refresh", async () => {
	const host = new LoadoutHost("test");
	let ready = false;
	const discovery = host.toolDiscovery.bind(host);
	host.toolDiscovery = async (operation, options) => {
		if (!ready) throw new Error("no active tool session");
		return discovery(operation, options);
	};
	const api = buildPi({ host, router: new EventRouter(), pluginName: "test" });
	api.setActiveTools(["lookup"]);
	await Promise.resolve();
	await Promise.resolve();
	await Promise.resolve();
	ready = true;
	await refreshToolLoadout(api);
	assert.deepEqual(host.active, ["lookup"]);
	assert.deepEqual(api.getActiveTools(), ["lookup"]);
});

test("reach cancellation signals admitted work and waits for settlement", async () => {
	const { EventEmitter } = await import("node:events");
	const { buildPluginService } = await import("../src/server.ts");
	const { api, handlers, waitForInFlight } = buildPluginService({
		host: new LoadoutHost("test"),
		extensionEntries: [],
		cwd: ".",
		pluginName: "test",
	});
	let entered!: () => void;
	const started = new Promise<void>((resolve) => {
		entered = resolve;
	});
	let release!: () => void;
	const pending = new Promise<void>((resolve) => {
		release = resolve;
	});
	let signal: AbortSignal | undefined;
	api.registerTool({
		name: "caller",
		reachesTools: async (_args, options) => {
			signal = options?.signal;
			entered();
			await pending;
			return ["lookup"];
		},
		execute() {},
	});
	const call = Object.assign(new EventEmitter(), {
		request: { toolName: "caller", arguments: "{}" },
		cancelled: false,
	});
	let replied = false;
	const response = new Promise<unknown>((resolve) =>
		handlers.ReachTools(call as never, (error, result) => {
			replied = true;
			resolve(error ?? result);
		}),
	);
	await started;
	call.cancelled = true;
	call.emit("cancelled");
	const aborted = signal?.aborted;
	let drained = false;
	const draining = waitForInFlight().then(() => {
		drained = true;
	});
	await Promise.resolve();
	await Promise.resolve();
	const premature = replied || drained;
	release();
	const result = await response;
	await draining;
	assert.equal(aborted, true);
	assert.equal(premature, false);
	assert.ok(result instanceof Error);
	assert.equal(call.listenerCount("cancelled"), 0);
});

test("tool progress carries bounded running records and settles before replying", async () => {
	const { buildPluginService } = await import("../src/server.ts");
	class ProgressHost extends LoadoutHost {
		updates: Array<{ toolCallId: string; content: string; nestedCalls?: unknown[] }> = [];
		override async toolProgress(update: { toolCallId: string; content: string; nestedCalls?: unknown[] }) {
			this.updates.push(update);
		}
	}
	const host = new ProgressHost("test");
	const { api, handlers } = buildPluginService({ host, extensionEntries: [], cwd: ".", pluginName: "test" });
	api.registerTool({
		name: "caller",
		async execute(_args, ctx) {
			await ctx.onUpdate?.({
				content: [{ type: "text", text: "parent output" }],
				nestedCalls: Array.from({ length: 140 }, (_, i) => ({
					id: String(i),
					name: "child",
					status: "running" as const,
				})),
			});
			return {};
		},
	});
	await new Promise<void>((resolve, reject) =>
		handlers.ExecuteTool(
			{ request: { toolName: "caller", toolCallId: "parent", arguments: "{}" } } as never,
			(error, result) => (error ? reject(error) : result?.isError ? reject(new Error(result.output)) : resolve()),
		),
	);
	assert.equal(host.updates.length, 1);
	assert.equal(host.updates[0]?.toolCallId, "parent");
	assert.equal(host.updates[0]?.content, "parent output");
	assert.equal(host.updates[0]?.nestedCalls?.length, 128);
});

test("tool cancellation joins work and suppresses cancelled and settled progress", async () => {
	const { EventEmitter } = await import("node:events");
	const { buildPluginService } = await import("../src/server.ts");
	class ProgressHost extends LoadoutHost {
		updates = 0;
		override async toolProgress() {
			this.updates++;
		}
	}
	const host = new ProgressHost("test");
	const { api, handlers, waitForInFlight } = buildPluginService({
		host,
		extensionEntries: [],
		cwd: ".",
		pluginName: "test",
	});
	let entered!: () => void;
	const started = new Promise<void>((resolve) => {
		entered = resolve;
	});
	let release!: () => void;
	const pending = new Promise<void>((resolve) => {
		release = resolve;
	});
	let signal: AbortSignal | undefined;
	let late: (() => Promise<void>) | undefined;
	api.registerTool({
		name: "caller",
		async execute(_args, ctx) {
			signal = ctx.signal;
			late = () => ctx.onUpdate!({ content: [{ type: "text", text: "late" }] });
			entered();
			await pending;
			await late();
			return {};
		},
	});
	const call = Object.assign(new EventEmitter(), {
		request: { toolName: "caller", arguments: "{}" },
		cancelled: false,
	});
	let replied = false;
	const response = new Promise<unknown>((resolve) =>
		handlers.ExecuteTool(call as never, (_error, result) => {
			replied = true;
			resolve(result);
		}),
	);
	await started;
	call.cancelled = true;
	call.emit("cancelled");
	const aborted = signal?.aborted;
	let drained = false;
	const draining = waitForInFlight().then(() => {
		drained = true;
	});
	await Promise.resolve();
	await Promise.resolve();
	const premature = replied || drained;
	release();
	const result = await response;
	await draining;
	await late!();
	assert.equal(aborted, true);
	assert.equal(premature, false);
	assert.equal(host.updates, 0);
	assert.equal((result as { isError: boolean }).isError, true);
	assert.equal(call.listenerCount("cancelled"), 0);
});

test("malformed tool arguments never reach execution", async () => {
	const { buildPluginService } = await import("../src/server.ts");
	const { api, handlers } = buildPluginService({
		host: new LoadoutHost("test"),
		extensionEntries: [],
		cwd: ".",
		pluginName: "test",
	});
	let calls = 0;
	api.registerTool({
		name: "caller",
		execute() {
			calls++;
			return {};
		},
	});
	for (const args of ["{", "[]", "null", "1"]) {
		const result = await new Promise<{ isError?: boolean; rejection?: string }>((resolve, reject) =>
			handlers.ExecuteTool({ request: { toolName: "caller", arguments: args } } as never, (error, result) =>
				error ? reject(error) : resolve(result!),
			),
		);
		assert.equal(result.isError, true);
		assert.match(result.rejection ?? "", /arguments/);
	}
	assert.equal(calls, 0);
});

test("factory loadout reads reject unavailable session snapshots", async () => {
	const api = buildPi({ host: new LoadoutHost("test"), router: new EventRouter(), pluginName: "test" });
	assert.throws(() => api.getAllTools(), /not initialized/);
	assert.throws(() => api.getActiveTools(), /not initialized/);
	await refreshToolLoadout(api);
	assert.deepEqual(api.getActiveTools(), ["read"]);
	assert.equal(api.getAllTools().length, 2);
});

test("failed first refresh does not expose a fabricated factory snapshot", async () => {
	const host = new LoadoutHost("test");
	const discovery = host.toolDiscovery.bind(host);
	let ready = false;
	host.toolDiscovery = async (operation, options) => {
		if (!ready) throw new Error("no active tool session");
		return discovery(operation, options);
	};
	const api = buildPi({ host, router: new EventRouter(), pluginName: "test" });
	await assert.rejects(refreshToolLoadout(api), /no active tool session/);
	assert.throws(() => api.getAllTools(), /not initialized/);
	ready = true;
	await refreshToolLoadout(api);
	assert.equal(api.getAllTools().length, 2);
});
