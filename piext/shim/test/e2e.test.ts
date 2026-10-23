import { HostClient } from "../src/host.ts";
// End-to-end test: load the fixture extension, build the Plugin service,
// and verify that Initialize, ListRegistrations, OnEvent, ExecuteCommand,
// and Shutdown all work against a real (in-process) gRPC client.

import { test } from "node:test";
import assert from "node:assert/strict";
import { Client, credentials, Metadata, type Server } from "@grpc/grpc-js";
import path from "node:path";
import os from "node:os";
import fs from "node:fs";
import { fileURLToPath } from "node:url";
import { buildPluginService, startServer } from "../src/server.ts";
import { callUnary, loadProto } from "../src/grpc.ts";
import type {
	ToolCallP,
	ToolResultP,
	CommandInvocationP,
	CommandResultP,
	EventP,
	EventResponseP,
	InitRequestP,
	InitResponseP,
	ListRegistrationsRequestP,
	RegistrationsP,
	ShutdownRequestP,
	ShutdownResponseP,
	ModelRouteRequestP,
	ModelRouteResponseP,
} from "../src/proto-types.ts";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

interface TestServer {
	sockPath: string;
	client: Client;
	server: Server;
}

async function startTestServer(extensionEntries: string[], cwd: string): Promise<TestServer> {
	const sockDir = fs.mkdtempSync(path.join(os.tmpdir(), "plugin-test-"));
	const sockPath = path.join(sockDir, "plugin.sock");
	const pluginName = "test-plugin";
	const host = new HostClient(pluginName);
	host.toolDiscovery = async (operation) => (operation === "registered" ? [{ name: "lookup" }] : []);
	const pluginService = buildPluginService({
		host,
		extensionEntries,
		cwd,
		pluginName,
		// Tests must not schedule the real process exit; the dedicated
		// shutdown test below covers exit scheduling explicitly.
		onShutdownAcknowledged: () => {},
	});
	const server = await startServer({ sockPath, mtls: undefined, pluginService });
	const { PluginClient } = loadProto();
	const client = new PluginClient(`unix:${sockPath}`, credentials.createInsecure()) as unknown as Client;
	return { sockPath, client, server };
}

function call<TReq, TRes>(client: Client, method: string, request: TReq): Promise<TRes> {
	return callUnary<TReq, TRes>(client, method, request, new Metadata(), { deadline: Date.now() + 4000 });
}

test("e2e: load fixture, init, list, dispatch, shutdown", async (t) => {
	const fixture = path.resolve(__dirname, "fixtures/extension.ts");
	const cwd = path.dirname(fixture);
	const { sockPath, client, server } = await startTestServer([fixture], cwd);
	t.after(() => {
		try {
			(client as unknown as { close?: () => void }).close?.();
		} catch {}
		try {
			server.forceShutdown();
		} catch {}
		try {
			fs.unlinkSync(sockPath);
		} catch {}
	});

	// Initialize
	const initResp = await call<InitRequestP, InitResponseP>(client, "initialize", {
		version: "test",
		sessionId: "s1",
		cwd,
		manifestPath: "",
	});
	assert.equal(initResp.apiVersion, "1");
	assert.equal(initResp.error, "", `Initialize returned error: ${initResp.error}`);

	// ListRegistrations
	const regs = await call<ListRegistrationsRequestP, RegistrationsP>(client, "listRegistrations", {});
	assert.equal(regs.toolPrefix, "");
	const lookup = regs.tools?.find((tool) => tool.name === "lookup");
	assert.equal(lookup?.exposure, "deferred");
	assert.deepEqual(JSON.parse(lookup?.outputSchema ?? "null"), { type: "object" });
	assert.deepEqual(JSON.parse(lookup?.constrainedSampling ?? "null"), { type: "json_schema", strict: "require" });
	const cmds = (regs.commands ?? []).map((c) => c.name);
	assert.ok(cmds.includes("hello"), `commands: ${cmds.join(", ")}`);
	assert.ok(cmds.includes("mode"), `commands: ${cmds.join(", ")}`);
	const subs = regs.eventSubscriptions ?? [];
	for (const expected of ["before_agent_start", "session_start", "agent_start", "agent_end", "input"]) {
		assert.ok(subs.includes(expected), `expected subscription: ${expected}; got: ${subs.join(", ")}`);
	}

	assert.deepEqual(
		(regs.virtualModels ?? []).map((model) => [
			model.provider,
			model.id,
			model.name,
			model.thinkingLevels,
			Number(model.contextWindow),
		]),
		[["router", "auto", "Auto", ["off", "high"], 1000]],
	);
	const routed = await call<ModelRouteRequestP, ModelRouteResponseP>(client, "routeModel", {
		provider: "router",
		modelId: "auto",
		requestJson: JSON.stringify({ reason: "user", thinkingLevel: "high", messages: [] }),
	});
	assert.deepEqual(
		[routed.provider, routed.modelId, routed.thinkingLevel, JSON.parse(routed.stateJson ?? "null"), routed.error],
		["anthropic", "claude-sonnet-4-6", "high", { routes: 1 }, ""],
	);
	const kept = await call<ModelRouteRequestP, ModelRouteResponseP>(client, "routeModel", {
		provider: "router",
		modelId: "auto",
		requestJson: JSON.stringify({ reason: "user", thinkingLevel: "off", state: { routes: 1 }, messages: [] }),
	});
	assert.equal(kept.stateJson, "", "returning request.state must keep the stored state");
	const failed = await call<ModelRouteRequestP, ModelRouteResponseP>(client, "routeModel", {
		provider: "router",
		modelId: "auto",
		requestJson: JSON.stringify({ reason: "direct", thinkingLevel: "off", messages: [] }),
	});
	assert.equal(failed.error, "no direct routes");
	const missing = await call<ModelRouteRequestP, ModelRouteResponseP>(client, "routeModel", {
		provider: "router",
		modelId: "gone",
		requestJson: "{}",
	});
	assert.match(missing.error ?? "", /not registered/);

	const toolResult = await call<ToolCallP, ToolResultP>(client, "executeTool", {
		toolName: "lookup",
		toolCallId: "call",
		arguments: "{}",
	});
	assert.deepEqual(JSON.parse(toolResult.structuredContent ?? "null"), { found: true });
	assert.deepEqual(JSON.parse(toolResult.content ?? "null"), [{ type: "text", text: "ok" }]);
	assert.deepEqual(JSON.parse(toolResult.usage ?? "null"), { input: 1, output: 2, totalTokens: 3 });

	// OnEvent: before_agent_start with systemPrompt — chained rewrite
	const eventResp = await call<EventP, EventResponseP>(client, "onEvent", {
		name: "before_agent_start",
		payload: JSON.stringify({ systemPrompt: "base" }),
		sessionId: "s1",
		timestamp: "2025-01-01T00:00:00Z",
	});
	assert.equal(eventResp.block, false);
	assert.ok(eventResp.responseData, "expected response_data");
	const data = JSON.parse(eventResp.responseData) as { systemPrompt: string };
	assert.equal(data.systemPrompt, "base\n[test-rules]");

	// OnEvent: tool_call (fixture has no tool_call handler; dispatch must
	// still succeed with an empty result).
	const tcResp = await call<EventP, EventResponseP>(client, "onEvent", {
		name: "tool_call",
		payload: JSON.stringify({
			toolName: "bash",
			toolCallId: "c1",
			arguments: JSON.stringify({ cmd: "ls" }),
		}),
		sessionId: "s1",
		timestamp: "2025-01-01T00:00:00Z",
	});
	assert.equal(tcResp.block, false);

	// ExecuteCommand: unknown command → isError
	const badResp = await call<CommandInvocationP, CommandResultP>(client, "executeCommand", {
		commandName: "nope",
		arguments: "",
		sessionId: "s1",
	});
	assert.equal(badResp.isError, true);

	// ExecuteCommand: hello (notify hits a non-existent Host; contained)
	const helloResp = await call<CommandInvocationP, CommandResultP>(client, "executeCommand", {
		commandName: "hello",
		arguments: "world",
		sessionId: "s1",
	});
	assert.equal(helloResp.isError, false);
	assert.equal(helloResp.output, "hello world");

	// Shutdown
	const shutdownResp = await call<ShutdownRequestP, ShutdownResponseP>(client, "shutdown", {
		drainTimeoutSeconds: 0,
	});
	assert.equal(shutdownResp.graceful, true);
});

// The shim must exit after acknowledging Shutdown. go-plugin's Kill() waits
// up to 2s for a graceful exit before SIGKILL, and a fast-exiting host (or
// one that only sends the Shutdown RPC) otherwise orphans the process.
test("e2e: shutdown RPC schedules process exit", async (t) => {
	const fixture = path.resolve(__dirname, "fixtures/extension.ts");
	const cwd = path.dirname(fixture);
	const sockDir = fs.mkdtempSync(path.join(os.tmpdir(), "plugin-test-"));
	const sockPath = path.join(sockDir, "plugin.sock");

	let acknowledged = 0;
	const pluginService = buildPluginService({
		extensionEntries: [fixture],
		cwd,
		pluginName: "test-plugin",
		onShutdownAcknowledged: () => {
			acknowledged++;
		},
	});
	const server = await startServer({ sockPath, mtls: undefined, pluginService });
	const { PluginClient } = loadProto();
	const client = new PluginClient(`unix:${sockPath}`, credentials.createInsecure()) as unknown as Client;
	t.after(() => {
		try {
			(client as unknown as { close?: () => void }).close?.();
		} catch {}
		try {
			server.forceShutdown();
		} catch {}
		try {
			fs.unlinkSync(sockPath);
		} catch {}
	});

	const shutdownResp = await call<ShutdownRequestP, ShutdownResponseP>(client, "shutdown", {
		drainTimeoutSeconds: 0,
	});
	assert.equal(shutdownResp.graceful, true);
	assert.equal(acknowledged, 1, "Shutdown acknowledgment must schedule process exit");
});
