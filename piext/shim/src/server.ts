// gRPC server: implements plugins.Plugin. The go-plugin client also expects
// three built-in services (GRPCStdio, GRPCController, GRPCBroker) defined
// in go-plugin's own proto; we don't carry those protos here, so we register
// only plugins.Plugin. The client tolerates missing built-ins (UNIMPLEMENTED
// on log capture / shutdown paths); the plugin works either way. Stdout is
// reserved for the handshake line — all shim diagnostics go to stderr.
//
// The Plugin service is what tau calls:
//   Initialize, ListRegistrations, OnEvent, ExecuteTool, ExecuteCommand,
//   Shutdown.

import {
	Server,
	ServerCredentials,
	type ServiceDefinition,
	type UntypedServiceImplementation,
	type handleUnaryCall,
} from "@grpc/grpc-js";
import { loadProto } from "./grpc.ts";
import type { MTLSMaterial } from "./cert.ts";
import { scheduleExit } from "./lifecycle.ts";
import {
	buildPi,
	refreshToolLoadout,
	flushToolLoadout,
	getRegistrations,
	toolParametersToMap,
	virtualModelKey,
} from "./pi-api.ts";
import { buildContext } from "./ctx.ts";
import { EventRouter } from "./events.ts";
import { HostClient } from "./host.ts";
import { refreshSessionEntries } from "./session-manager.ts";
import { shimState } from "./state.ts";
import { loadExtension } from "./loader.ts";
import { dispatchTerminalInput } from "./terminal-input.ts";
import type { PiAPI } from "./pi-types.ts";
import type {
	CommandInvocationP,
	CommandResultP,
	EventP,
	EventResponseP,
	InitRequestP,
	InitResponseP,
	ListRegistrationsRequestP,
	ModelRouteRequestP,
	ModelRouteResponseP,
	RegistrationsP,
	ShutdownRequestP,
	ShutdownResponseP,
	TerminalInputP,
	TerminalInputResponseP,
	ToolCallP,
	ToolResultP,
} from "./proto-types.ts";

export interface PluginServiceHandlers {
	ReachTools: handleUnaryCall<ToolCallP, { names: string[] }>;
	Initialize: handleUnaryCall<InitRequestP, InitResponseP>;
	ListRegistrations: handleUnaryCall<ListRegistrationsRequestP, RegistrationsP>;
	OnEvent: handleUnaryCall<EventP, EventResponseP>;
	ExecuteTool: handleUnaryCall<ToolCallP, ToolResultP>;
	ExecuteCommand: handleUnaryCall<CommandInvocationP, CommandResultP>;
	OnTerminalInput: handleUnaryCall<TerminalInputP, TerminalInputResponseP>;
	RouteModel: handleUnaryCall<ModelRouteRequestP, ModelRouteResponseP>;
	Shutdown: handleUnaryCall<ShutdownRequestP, ShutdownResponseP>;
}

export interface PluginServiceBundle {
	handlers: PluginServiceHandlers;
	api: PiAPI;
	router: EventRouter;
	host: HostClient;
	/** Resolves when in-flight handler promises settle (drain support). */
	waitForInFlight: () => Promise<void>;
}

export interface BuildPluginServiceOptions {
	host?: HostClient;
	extensionEntries: string[];
	cwd: string;
	pluginName: string;
	/** Invoked once the Shutdown RPC is acknowledged; defaults to scheduling
	 * process exit so a fast-exiting host cannot orphan the shim. */
	onShutdownAcknowledged?: () => void;
}

/**
 * Build the Plugin service impl. extensionEntries are absolute (or
 * cwd-relative) paths to JS/TS files; cwd is the package dir (set as
 * process.cwd() by tau for sh -c launched plugins).
 */
export function buildPluginService({
	extensionEntries,
	cwd,
	pluginName,
	host = new HostClient(pluginName),
	onShutdownAcknowledged = scheduleExit,
}: BuildPluginServiceOptions): PluginServiceBundle {
	const router = new EventRouter();
	const api = buildPi({ host, router, pluginName });

	// Internal subscriptions for derived state (isIdle, model).
	router.subscribeInternal("before_agent_start", () => shimState.markAgentStart());
	router.subscribeInternal("agent_settled", () => shimState.markAgentEnd());
	router.subscribeInternal("agent_start", () => shimState.markAgentStart());
	router.subscribeInternal("agent_end", () => shimState.markAgentEnd());
	router.subscribeInternal("model_select", (payload) => {
		if (payload && typeof payload === "object") shimState.setModel(payload);
	});

	let initialized = false;
	const inFlight = new Set<Promise<unknown>>();
	const waitForInFlight = (): Promise<void> => Promise.allSettled(Array.from(inFlight)).then(() => undefined);

	const track = (promise: Promise<unknown>): void => {
		inFlight.add(promise);
		promise.finally(() => inFlight.delete(promise)).catch(() => undefined);
	};
	const withLoadout = async <T>(action: () => Promise<T> | T): Promise<T> => {
		await refreshToolLoadout(api);
		let result: T;
		try {
			result = await action();
		} catch (error) {
			try {
				await flushToolLoadout(api);
			} catch (activationError) {
				throw new AggregateError([error, activationError], `${String(error)}; ${String(activationError)}`);
			}
			throw error;
		}
		await flushToolLoadout(api);
		return result;
	};

	const handlers: PluginServiceHandlers = {
		// NOTE: grpc-js handlers must invoke `callback` explicitly — a returned
		// Promise is ignored (async handlers are fine as long as they call back).
		async Initialize(_call, callback) {
			try {
				if (initialized) {
					callback(null, { apiVersion: "1", error: "already initialized" });
					return;
				}
				initialized = true;

				// Load each extension entry sequentially and invoke its factory.
				const errors: string[] = [];
				for (const entry of extensionEntries) {
					try {
						const factory = await loadExtension(entry, cwd);
						if (typeof factory !== "function") {
							errors.push(`${entry}: default export is not a function`);
							continue;
						}
						// pi factories may be sync or async; await the result.
						await factory(api);
					} catch (e) {
						errors.push(`${entry}: ${e instanceof Error ? e.message : String(e)}`);
					}
				}

				if (errors.length > 0) {
					callback(null, { apiVersion: "1", error: errors.join("; ") });
					return;
				}
				callback(null, { apiVersion: "1", error: "" });
			} catch (e) {
				callback(e as Error, undefined);
			}
		},

		ReachTools(call, callback) {
			const controller = new AbortController();
			const cancel = () => controller.abort();
			call.on?.("cancelled", cancel);
			if (call.cancelled) cancel();
			const handlerPromise = (async () => {
				try {
					controller.signal.throwIfAborted();
					const tool = getRegistrations(api).tools.get(call.request.toolName ?? "");
					if (!tool?.reachesTools) throw new Error("reach callback unavailable");
					const args: unknown = JSON.parse(call.request.arguments || "{}");
					if (!args || typeof args !== "object" || Array.isArray(args))
						throw new Error("reach arguments must be an object");
					const reach = await tool.reachesTools(args as Record<string, unknown>, { signal: controller.signal });
					controller.signal.throwIfAborted();
					if (reach !== "all" && (!Array.isArray(reach) || reach.some((name) => typeof name !== "string")))
						throw new Error("invalid tool reach");
					callback(null, { names: reach === "all" ? ["all"] : reach });
				} catch (error) {
					callback(new Error(error instanceof Error ? error.message : String(error)), null);
				} finally {
					call.removeListener?.("cancelled", cancel);
				}
			})();
			track(handlerPromise);
		},
		ListRegistrations(_call, callback) {
			const registrations = getRegistrations(api);
			const tools = Array.from(registrations.tools.values()).map((t) => ({
				hasReachesTools: typeof t.reachesTools === "function",
				name: t.name,
				description: t.description ?? "",
				parameters: toolParametersToMap(t.parameters),
				namespace: t.namespace ? JSON.stringify(t.namespace) : "",
				...(t.defaultActive === undefined ? {} : { defaultActive: t.defaultActive }),
				exposure: t.exposure ?? "",
				outputSchema: t.outputSchema ? JSON.stringify(t.outputSchema) : "",
				constrainedSampling: t.constrainedSampling === undefined ? "" : JSON.stringify(t.constrainedSampling),
			}));
			const commands = Array.from(registrations.commands.values()).map((c) => ({
				name: c.name,
				description: c.description ?? "",
				argumentHint: "",
			}));
			const eventSubscriptions = router.subscribedEvents();
			const cliFlags = Array.from(registrations.flags.values()).map((f) => ({
				name: f.name,
				description: f.description ?? "",
				required: false,
				defaultValue: String(f.default ?? ""),
			}));
			const virtualModels = Array.from(registrations.virtualModels.values()).map((model) => ({
				provider: model.provider,
				id: model.id,
				name: model.name ?? model.id,
				thinkingLevels: [...(model.thinkingLevels ?? [])],
				input: [...(model.input ?? [])],
				contextWindow: model.contextWindow ?? 0,
				maxTokens: model.maxTokens ?? 0,
			}));
			callback(null, {
				tools,
				commands,
				eventSubscriptions,
				cliFlags,
				toolPrefix: "",
				virtualModels,
			});
		},

		OnEvent(call, callback) {
			const req = call.request;
			const handlerPromise = (async (): Promise<EventResponseP> => {
				return withLoadout(async () => {
					// session_start: pre-fetch entries so ctx.sessionManager serves pi's
					// synchronous getEntries/getBranch contract (extensions read their
					// custom entries here to restore per-session state).
					if ((req.name ?? "") === "session_start") {
						await refreshSessionEntries(host);
					}
					const result = await router.dispatch(req.name ?? "", req.payload ?? "", () =>
						buildContext({ host, cwd, pluginName }),
					);
					return {
						block: result.block,
						blockReason: result.blockReason,
						responseData: result.responseData,
					};
				});
			})();
			inFlight.add(handlerPromise);
			handlerPromise.finally(() => inFlight.delete(handlerPromise)).catch(() => undefined);
			handlerPromise.then(
				(resp) => callback(null, resp),
				(e: unknown) => callback(e instanceof Error ? e : new Error(String(e)), undefined),
			);
		},

		ExecuteTool(call, callback) {
			const controller = new AbortController();
			const cancel = () => controller.abort();
			call.on?.("cancelled", cancel);
			if (call.cancelled) cancel();
			const handlerPromise = (async () => {
				const req = call.request;
				const toolName = req.toolName ?? "";
				const tool = getRegistrations(api).tools.get(toolName);
				if (!tool) {
					callback(null, {
						output: JSON.stringify({ error: `unknown tool: ${toolName}` }),
						isError: true,
						errorMessage: `unknown tool: ${toolName}`,
					});
					return;
				}
				let args: Record<string, unknown>;
				try {
					const parsed: unknown = JSON.parse(req.arguments || "{}");
					if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("expected an object");
					args = parsed as Record<string, unknown>;
				} catch (error) {
					const message = `invalid tool arguments: ${error instanceof Error ? error.message : String(error)}`;
					callback(null, { output: message, isError: true, rejection: message, errorMessage: message });
					return;
				}
				const ctx = buildContext({ host, cwd, pluginName, signal: controller.signal });
				let settled = false;
				let progress = Promise.resolve();
				let progressFailure: unknown;
				ctx.onUpdate = (partial) => {
					if (settled || controller.signal.aborted) return Promise.resolve();
					const update = {
						toolCallId: req.toolCallId ?? "",
						content: (partial.content ?? [])
							.filter((c) => c.type === "text")
							.map((c) => String(c.text ?? ""))
							.join(""),
						isError: partial.isError,
						nestedCalls: partial.nestedCalls?.slice(-128),
					};
					progress = progress
						.then(() => {
							if (!controller.signal.aborted) return host.toolProgress(update);
						})
						.catch((error) => {
							progressFailure = error;
						});
					return progress;
				};
				try {
					const result = await withLoadout(async () => {
						let result;
						let failure: unknown;
						try {
							controller.signal.throwIfAborted();
							result = await tool.execute(args, ctx);
						} catch (error) {
							failure = error;
						} finally {
							settled = true;
							await progress;
						}
						if (failure !== undefined && progressFailure !== undefined)
							throw new AggregateError([failure, progressFailure], `${String(failure)}; ${String(progressFailure)}`);
						if (failure !== undefined) throw failure;
						if (progressFailure !== undefined) throw progressFailure;
						controller.signal.throwIfAborted();
						return result;
					});
					// pi ToolResult: { content: TextContent[] | ImageContent[], isError }
					const content = Array.isArray(result?.content) ? result.content : [];
					const text = content
						.filter((c) => c && (c.type === "text" || typeof c.text === "string"))
						.map((c) => String(c.text ?? ""))
						.join("");
					callback(null, {
						output: text || (result ? JSON.stringify(result) : ""),
						content: JSON.stringify(content),
						nestedCalls: result?.nestedCalls === undefined ? "" : JSON.stringify(result.nestedCalls.slice(-128)),
						structuredContent: result?.structuredContent === undefined ? "" : JSON.stringify(result.structuredContent),
						usage: result?.usage === undefined ? "" : JSON.stringify(result.usage),
						isError: result?.isError === true,
						errorMessage: result?.isError === true ? text || "tool error" : "",
					});
				} catch (e) {
					callback(null, {
						output: `tool error: ${e instanceof Error ? e.message : String(e)}`,
						rejection: e instanceof Error ? e.message : String(e),
						isError: true,
						errorMessage: e instanceof Error ? e.message : String(e),
					});
				}
			})();
			track(handlerPromise.finally(() => call.removeListener?.("cancelled", cancel)));
		},

		ExecuteCommand(call, callback) {
			const handlerPromise = (async () => {
				const req = call.request;
				const name = req.commandName ?? "";
				const args = req.arguments ?? "";
				const cmd = getRegistrations(api).commands.get(name);
				if (!cmd) {
					callback(null, {
						output: `unknown command: ${name}`,
						isError: true,
						errorMessage: `unknown command: ${name}`,
					});
					return;
				}
				const ctx = buildContext({ host, cwd, pluginName });
				try {
					const result = await withLoadout(() => cmd.handler(args, ctx));
					callback(null, { output: commandOutput(result), isError: false, errorMessage: "" });
				} catch (e) {
					callback(null, {
						output: e instanceof Error ? e.message : String(e),
						isError: true,
						errorMessage: e instanceof Error ? e.message : String(e),
					});
				}
			})();
			track(handlerPromise);
		},

		RouteModel(call, callback) {
			const controller = new AbortController();
			const cancel = () => controller.abort();
			call.on?.("cancelled", cancel);
			if (call.cancelled) cancel();
			const handlerPromise = (async (): Promise<ModelRouteResponseP> => {
				const { provider = "", modelId = "", requestJson = "" } = call.request;
				const model = getRegistrations(api).virtualModels.get(virtualModelKey(provider, modelId));
				if (!model) throw new Error(`Virtual model ${provider}/${modelId} is not registered.`);
				const request = JSON.parse(requestJson || "{}") as { state?: unknown };
				const state = request.state;
				const route = await model.route(
					{ ...request, signal: controller.signal },
					buildContext({ host, cwd, pluginName, signal: controller.signal }),
				);
				if (!route?.model?.provider || !route.model.id)
					throw new Error(`Virtual model ${provider}/${modelId} returned no model.`);
				// Returning request.state (or nothing) keeps the stored state.
				const kept = route.state === undefined || route.state === state;
				return {
					provider: route.model.provider,
					modelId: route.model.id,
					thinkingLevel: route.thinkingLevel ?? "",
					stateJson: kept ? "" : JSON.stringify(route.state),
					error: "",
				};
			})();
			handlerPromise.then(
				(response) => callback(null, response),
				(e: unknown) =>
					callback(null, {
						provider: "",
						modelId: "",
						thinkingLevel: "",
						stateJson: "",
						error: e instanceof Error ? e.message : String(e),
					}),
			);
			track(handlerPromise.finally(() => call.removeListener?.("cancelled", cancel)));
		},

		OnTerminalInput(call, callback) {
			const req = call.request;
			const consume = dispatchTerminalInput({
				key: req.key ?? "",
				text: req.text ?? "",
				mod: req.mod ?? "",
			});
			callback(null, { consume });
		},

		Shutdown(call, callback) {
			void (async () => {
				const drainSeconds = call.request?.drainTimeoutSeconds ?? 0;
				const deadline = new Promise<void>((resolve) => setTimeout(resolve, drainSeconds * 1000));
				// Wait for in-flight handlers up to the drain timeout.
				await Promise.race([waitForInFlight(), deadline]);
				callback(null, { graceful: true });
				// The host kills us after this RPC resolves; exit on our own so
				// a fast-exiting host can't orphan us first (go-plugin's Kill
				// waits 2s for graceful exit before SIGKILL).
				onShutdownAcknowledged();
			})();
		},
	};

	return { handlers, api, router, host, waitForInFlight };
}

function commandOutput(result: unknown): string {
	if (result == null) return "";
	if (typeof result === "string") return result;
	if (typeof result !== "object") return String(result);

	const record = result as Record<string, unknown>;
	for (const key of ["output", "text", "message", "content"]) {
		const value = record[key];
		if (typeof value === "string") return value;
	}
	return JSON.stringify(result);
}

export interface StartServerOptions {
	sockPath: string;
	/** mTLS material when AutoMTLS is active; undefined = insecure. */
	mtls: MTLSMaterial | undefined;
	pluginService: PluginServiceBundle;
	/** Called after the server is bound (handshake line must be printed here). */
	onBound?: () => void;
}

/**
 * Start the gRPC server on the unix socket. Resolves once bound.
 *
 * The handlers are typed via PluginServiceHandlers; the single cast to
 * UntypedServiceImplementation is the proto-loader boundary (grpc-js cannot
 * know message types from dynamically loaded protos).
 */
export async function startServer(options: StartServerOptions): Promise<Server> {
	const { sockPath, pluginService, onBound } = options;
	const credentialsObj = options.mtls
		? ServerCredentials.createSsl(
				options.mtls.rootCerts,
				options.mtls.keyCertPairs,
				options.mtls.checkClientCertificate,
			)
		: ServerCredentials.createInsecure();

	const { PluginService } = loadProto();
	const server = new Server();
	server.addService(
		PluginService as ServiceDefinition,
		pluginService.handlers as unknown as UntypedServiceImplementation,
	);

	await new Promise<void>((resolve, reject) => {
		server.bindAsync(`unix:${sockPath}`, credentialsObj, (err) => {
			if (err) {
				reject(new Error(`server.bindAsync failed: ${err.message}`));
				return;
			}
			resolve();
		});
	});
	onBound?.();
	return server;
}
