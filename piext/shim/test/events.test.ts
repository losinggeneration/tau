// Unit tests for the EventRouter: handler registration, dispatch, and the
// serialization of response_data (which tau uses to chain system-prompt
// rewrites across plugins).

import { test } from "node:test";
import assert from "node:assert/strict";
import { CANCELLABLE, EventRouter, HANDLED_EVENTS } from "../src/events.ts";

test("on registers and dispatches synchronously", async () => {
	const router = new EventRouter();
	let seen: { type: string } | undefined;
	router.on("agent_start", (event) => {
		seen = event as { type: string };
	});
	const result = await router.dispatch("agent_start", JSON.stringify({}));
	assert.equal(seen?.type, "agent_start");
	assert.equal(result.block, false);
	assert.equal(result.responseData, "");
});

test("unknown events are skipped", async () => {
	const router = new EventRouter();
	const result = await router.dispatch("compilation_request", "{}");
	assert.equal(result.block, false);
	assert.equal(result.responseData, "");
});

test("internal subscribers always fire even without extension subscription", async () => {
	const router = new EventRouter();
	let calls = 0;
	router.subscribeInternal("before_agent_start", () => {
		calls++;
	});
	await router.dispatch("before_agent_start", JSON.stringify({ systemPrompt: "x" }));
	assert.equal(calls, 1);
});

test("before_agent_start: systemPrompt rewrites chain in registration order", async () => {
	const router = new EventRouter();
	router.on("before_agent_start", (event) => ({
		systemPrompt: (event.systemPrompt ?? "") + "[A]",
	}));
	router.on("before_agent_start", (event) => ({
		// B sees the chained value because dispatch mutates event.systemPrompt
		// between handlers.
		systemPrompt: (event.systemPrompt ?? "") + "[B]",
	}));
	const result = await router.dispatch("before_agent_start", JSON.stringify({ systemPrompt: "base" }));
	const parsed = JSON.parse(result.responseData) as { systemPrompt: string };
	assert.equal(parsed.systemPrompt, "base[A][B]");
});

test("tool_call: handler block verdict is propagated", async () => {
	const router = new EventRouter();
	router.on("tool_call", () => ({ block: true, reason: "denied" }));
	const result = await router.dispatch(
		"tool_call",
		JSON.stringify({ toolName: "bash", toolCallId: "c1", arguments: "{}" }),
	);
	assert.equal(result.block, true);
	assert.equal(result.blockReason, "denied");
});

test("tool_call: input.arguments is parsed and re-serialized", async () => {
	const router = new EventRouter();
	let receivedInput: Record<string, unknown> | undefined;
	router.on("tool_call", (event) => {
		// Capture first, then mutate to mimic pi's input-rewriting pattern.
		receivedInput = { ...event.input };
		event.input!["mutated"] = true;
	});
	const result = await router.dispatch(
		"tool_call",
		JSON.stringify({
			toolName: "bash",
			toolCallId: "c1",
			arguments: JSON.stringify({ cmd: "ls" }),
		}),
	);
	assert.deepEqual(receivedInput, { cmd: "ls" });
	const parsed = JSON.parse(result.responseData) as { arguments: Record<string, unknown> };
	assert.deepEqual(parsed.arguments, { cmd: "ls", mutated: true });
});

test("session_start: payload is projected to pi shape", async () => {
	const router = new EventRouter();
	let event: { id?: string; cwd?: string; reason?: string } | undefined;
	router.on("session_start", (e) => {
		event = e as { id?: string; cwd?: string; reason?: string };
	});
	await router.dispatch(
		"session_start",
		JSON.stringify({ id: "s1", cwd: "/x", model: "anthropic/claude", reason: "new" }),
	);
	assert.equal(event?.id, "s1");
	assert.equal(event?.cwd, "/x");
	assert.equal(event?.reason, "new");
});

test("async handlers are awaited in order", async () => {
	const router = new EventRouter();
	const order: string[] = [];
	router.on("agent_start", async () => {
		await new Promise<void>((r) => setTimeout(r, 10));
		order.push("A");
	});
	router.on("agent_start", async () => {
		order.push("B");
	});
	await router.dispatch("agent_start", "{}");
	assert.deepEqual(order, ["A", "B"]);
});

test("handler throwing does not crash the router", async () => {
	const router = new EventRouter();
	router.on("agent_start", () => {
		throw new Error("boom");
	});
	router.on("agent_start", () => {
		// still runs; the first handler's throw is contained
	});
	await router.dispatch("agent_start", "{}");
});

test("HANDLED_EVENTS and CANCELLABLE are exported and populated", () => {
	assert.ok(HANDLED_EVENTS instanceof Set);
	assert.ok(HANDLED_EVENTS.size > 10);
	assert.ok(HANDLED_EVENTS.has("session_start"));
	assert.ok(HANDLED_EVENTS.has("before_agent_start"));
	assert.ok(CANCELLABLE.has("tool_call"));
});

// pi handlers receive (event, ctx). The router must pass ctx when
// the dispatcher provides one, matching pi's extension contract.
test("handlers receive ctx as second argument when dispatcher provides it", async () => {
	const router = new EventRouter();
	const calls: Array<unknown> = [];
	const sentinel = { ui: { notify: () => undefined }, tag: "ctx" } as unknown as object;
	router.on("session_start", (e, ctx) => {
		calls.push({ type: e.type, ctx });
	});
	await router.dispatch(
		"session_start",
		JSON.stringify({ id: "s1", cwd: "/x", model: "anthropic/claude", reason: "new" }),
		() => sentinel as never,
	);
	assert.equal(calls.length, 1);
	const call = calls[0] as { type: string; ctx: unknown };
	assert.equal(call.type, "session_start");
	assert.equal(call.ctx, sentinel);
});

test("dispatch without makeCtx leaves ctx undefined (extensions must guard)", async () => {
	const router = new EventRouter();
	let observedCtx: unknown = "unset";
	router.on("agent_start", (_e, ctx) => {
		observedCtx = ctx;
	});
	await router.dispatch("agent_start", "{}");
	assert.equal(observedCtx, undefined);
});
